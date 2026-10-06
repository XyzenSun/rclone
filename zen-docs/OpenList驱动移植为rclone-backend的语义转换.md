# 把 OpenList 驱动实现为 rclone backend:语义转换经验

本文档与《在rclone中实现openlist的上游.md》互补:那篇讲选型决策与通用移植模式(HTTP 栈注入、凭据持久化、config 向导、ID 目录缓存),本篇讲实现层的语义转换——OpenList 驱动概念如何映射到 rclone backend、哪些能直接复用、哪些必须按 rclone 规范重写。案例全部来自 github backend 的一次完整实现与真实凭证验收,通用结论适用于任何 OpenList 上游。

## 概念对照

OpenList 驱动与 rclone backend 的接口粒度和语义约定差异很大,下表是两边的概念映射,也是移植工作量所在:

| OpenList 驱动 | rclone backend | 转换要点 |
|---|---|---|
| driver.Driver 的 List/Link/Put/MakeDir/Move/Rename/Copy/Remove | fs.Fs 的 List/NewObject/Put/Mkdir/Rmdir/Move/Copy/DirMove + Object 的 Open/Update/Remove | 粒度不同:OpenList 的 Rename 与 Move(移入目录)是两个操作,rclone 的 Move(src, remote) 是全路径移动,需重写为统一算法而非拼接两个旧操作 |
| model.Obj / model.Object | fs.Dir / fs.Object | OpenList 对象的 modtime 无语义约束(普遍零时间),rclone 的 fstests 有精度检查,必须有真实时间源 |
| Addition(json tag + default) | fs.Options(config tag + Default/Advanced/Sensitive) | token 类字段加 Sensitive;模板/高级项加 Advanced |
| Link 返回 URL 由 server 层消费 | Object.Open 自己发请求 | rclone backend 要自己处理 Range、限流重试、错误映射;OpenList 侧的 proxy/redirect 逻辑一概不需要 |
| errs.ObjectNotFound / NotFolder | fs.ErrorObjectNotFound / ErrorDirNotFound / ErrorIsDir | fstests 直接断言这些 sentinel,映射错了整套测试失败 |
| resty + 手工状态码检查 | lib/rest + fs.Pacer + shouldRetry | 限流(429/403+Reset 头)统一走 pacer 退避,不要在业务代码里散落重试 |
| 绝对路径 + utils.FixAndCleanPath | root 相对路径 + f.gitPath + encoder | 见下文"路径模型"陷阱 |
| 无 hash 体系 | fs/hash.RegisterHash | 值得做:注册与服务端 sha 同源的自定义 hash,--checksum 免流量校验(gitsha1 案例) |
| 单请求模型,无并发写概念 | --transfers 并发调用 Put/Move | 写序列(如 tree->commit->ref)需要进程内互斥锁 |
| ReaderUpdatingProgress 包装上传流 | 无需 | rclone 核心层已对 reader 做 accounting 包装,backend 只管流式拼接 |

## 可直接复用与必须重写的边界

协议知识可以整体复用:API 端点、请求/响应结构(types.go 基本照搬换包名)、请求编排顺序(如 git 的 blobs -> trees -> commits -> refs 四步)、配置项设计意图。这些是读 OpenList 驱动的真正收益——社区已经逆向好了协议,不用自己抓包摸索。

架构必须按 rclone 规范从零写:路径模型、错误映射、限流重试、hash/modtime 语义、并发控制、编码转换。判断标准很简单:凡是 fstests 有断言的行为,都以 fstests 为准重写;凡是 OpenList 框架层提供的能力(proxy、进度包装、路径清洗),一律丢弃换成 rclone 对应物。

## 语义陷阱

**路径模型。** OpenList 内部用绝对路径(`/a/b`)加 `FixAndCleanPath` 清洗;rclone 的 remote 是相对 Fs root 的路径,且要经过 encoder 双向转换(见《经验.md》的 encoder 一节)。github 实现中所有路径先 `path.Join(f.root, remote)` 再 `FromStandardPath` 编码,返回条目时反向解码。曾因在 NewFs 里误用 `gitPath(f.root)` 造成双重拼接,这类"两种路径体系混用"的错误在移植中最常见,建议路径转换函数集中收口。

**接口语义。** 四个直接踩过的契约:`Mkdir("")` 表示创建 Fs 根目录本身(`rclone mkdir remote:dir` 以 dir 为 root 调用它);Move/Copy 的目标父目录可能不存在,backend 要在同一事务内补建;DirMove 在目标已存在时必须返回 `fs.ErrorDirExists`;NewObject 必须返回非零 modtime。OpenList 的 MakeDir/Move 都假设父目录存在且无这些返回值约定,照搬必然失败。

**并发。** OpenList 驱动是单请求模型,没有并发写的概念;rclone 会以 `--transfers` 并发调用 Put/Move。凡是"读状态 -> 计算 -> 提交"的多步写序列,必须用互斥锁串行化,否则并发下互相覆盖。

**流式。** OpenList 的 `putBlob` 用 `ReaderUpdatingProgress` 包装上传流做进度统计;rclone 里这层完全不需要——核心层(operations)在调 Put 前已经把 reader 包进 accounting,backend 只需要 io.Pipe 流式拼接请求体。

## OpenList 驱动的已知缺陷,不要照搬

以 github 驱动为参照,至少四个缺陷会在照搬时被继承:`getPathCommonAncestor` 在一方是另一方前缀时数组越界 panic(嵌套路径移动场景);non-fast-forward 时用旧 tree 重提交会静默覆盖外部并发提交;raw 下载 URL 受 CDN 缓存影响,写后读返回旧内容;`renewParentTrees` 按 sha 匹配父目录条目,同目录两个内容相同的条目会匹配错(应按名字匹配)。github backend 的 `applyEdits` 变更集合算法(自底向上重建、根目录最后处理、按名字续链)是针对这些缺陷的重写,可作为路径型上游 tree 操作的参考实现。

## 验证流程

实现完成后的验证顺序:先读 `fstest/fstests/fstests.go` 相关测试段确认契约理解无误;协议行为存疑处用 curl 直测隔离(本次确认了 trees API 序列化约束与 CDN 缓存行为);最后真实凭证跑 fstests 完整套件与人工命令冒烟。流程细节已固化在 `AGENTS.override.md` 的测试策略一节,教训细节见《经验.md》。
