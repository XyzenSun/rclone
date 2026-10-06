# AGENTS.override.md

本文件是 XyzenSun/rclone fork 的项目规则，由 pi 优先加载并取代同目录上游 `AGENTS.md`（该文件保留原样不改动，以避免与上游 merge 冲突；`CLAUDE.md` 同理保留）。上游 AGENTS.md 中对本 fork 仍有效的构建与测试命令已吸收到本文。

## 项目定位

这是对 rclone/rclone 的长期自用 fork, 不计划向上游提交代码或请求上游接纳自用改动. 日常开发与发布都以 `zen` 分支为准, 定期通过 merge 获取上游更新. 工程决策优先控制与上游的差异, 降低同步成本, 同时允许按自用需求调整功能.

## 核心约束（最高优先级，与全局规范冲突时以本节为准）

版本与发布: `VERSION` 文件跟随上游, 不自行修改. 允许使用 `zen-v*` 作为 fork 专用发布 tag, 与官方 `v*` tag 区分. 保留现有 `zen-release.yml` 发布机制: 发布构建附加 `-zen` 标识, 手动构建附加 `-zen.dev.<commit>` 标识. 发布 tag 必须指向已验证的 `zen` 提交, 不移动或覆盖已有 tag.

分支与远端: `zen` 是长期开发、默认与发布分支. `origin` 指向 `XyzenSun/rclone`, 用于推送自用改动; `upstream` 指向 `rclone/rclone`, 仅用于获取更新并配置禁用 push 的 URL. 上游同步直接将 `upstream/master` merge 到 `zen`, 不要求经过本地 `master`, 不向上游提交 PR. 已发布的 `zen` 历史保持不变, 不通过 rebase 或强推整理同步历史.

历史 `master` 暂时保留, 它已有 fork 自用提交, 当前不作为纯上游镜像使用. 不自动 reset、删除或改写其历史. 将来需要整理它时另行确认.

兼容与改动范围: 默认保持官方 CLI、配置格式、环境变量语义、rc API 与现有 backend 行为. 自用需求确需调整这些行为或核心层时, 先与用户确认范围, 说明兼容性变化与后续同步成本, 获得批准后再实施. 优先在自用 backend 内解决问题, 避免扩大核心层差异.

同步前必须确认当前分支与工作区状态. 工作区有未提交改动时暂停 merge, 不自动 stash、reset 或丢弃改动. 合并冲突逐项分析, 不批量选择 ours/theirs. 合并后按实际影响范围构建与验证, 全量测试仍需用户批准. commit 与 push 分别遵循用户授权, 不因完成合并而自动执行.

## 目录结构

```
backend/   存储后端; all/all.go 是 blank import 聚合入口; 自用 backend 的主要改动区
cmd/       CLI 子命令, cmd/all 聚合; 调整前确认兼容性与同步成本
fs/        核心抽象(接口/Features/注册表/全局参数); 调整前确认兼容性与同步成本
lib/       基础库（rest/oauthutil/encoder/multipart/pool 等），新 backend 优先复用
fstest/    集成测试框架
vfs/       mount 虚拟文件系统; 调整前确认兼容性与同步成本
docs/      文档；data/backends/*.yaml 是 backend 元数据（被 embed 进二进制）
zen-docs/  fork 的私有笔记（已跟踪入库）
```

## 新增 backend 的常用触点

新增 backend 默认将改动集中在以下位置. 超出这些触点时先与用户确认理由与范围. fork 专用规则、维护文档、安装脚本与发布 workflow 也可按已批准的维护需求调整, 上游 `AGENTS.md` 与 `CLAUDE.md` 继续保留原样.

- 动手前先重读 `zen-docs/经验.md`，并 `grep -rn 踩坑 backend/` 扫一遍既有实现的注释，把历史教训变成检查清单（baidupcs 交付时重犯了 github 已记录的 encoder 全角点教训并造成目录被替换，教训见该文档）。
- `backend/<name>/`：全新目录，包含 `<name>.go`（init + fs.Register + Options struct + Fs/Object 实现）、`api/types.go`、`<name>_test.go`。实现规范遵循上游 CONTRIBUTING.md 的 "Writing a new backend"：目录型参考 box，桶型参考 b2；不拆 fs.go/object.go；HTTP 型优先用 lib/rest + fs/fshttp；路径编码用 lib/encoder；上传缓冲用 lib/multipart/lib/pool。
- `backend/all/all.go`：按字母序插入一行 blank import。
- `docs/data/backends/<name>.yaml`：必须创建，否则 fs.Register 启动时报 internal error。
- `docs/content/<name>.md`、`README.md`、`docs/content/docs.md`、`docs/content/_index.md`：按字母序追加条目。
- `fstest/test_all/config.yaml`：追加测试注册段。
- `zen-docs/`：笔记与设计文档。

## 核心层改动与同步成本

`fs/`、`fs/operations`、`fs/sync`、`fs/config/`、`vfs/`、`cmd/` 与上游已有 `backend/*` 变更频繁, 对它们的自用改动会增加后续合并与回归验证成本. 这些目录允许按已批准的需求修改, 实施前必须说明改动范围、兼容性影响与同步风险.

新能力优先通过自用 backend 的 Options 与 Features 表达, 共用能力优先复用现有库与接口. 参数默认值优先通过部署环境变量调整, 简单配置需求不引入核心代码差异.

参数默认值的调整方式：全局 flag 用 `RCLONE_<FLAG>`（如 `RCLONE_TRANSFERS=8`），backend 类型级默认用 `RCLONE_<BACKEND>_<OPTION>`（如 `RCLONE_BOX_UPLOAD_CUTOFF=100M`），具体 remote 实例用 `RCLONE_CONFIG_<REMOTE>_<OPTION>`。环境变量是默认值级别，命令行与配置文件中更具体的设置仍然优先。

## 构建与测试

```bash
go build                          # 简单构建
make                              # 带版本信息的构建（首选）
go test -v ./backend/<name>/      # 单个 backend 的测试（需配置 Test<Name> remote）
RCLONE_CONFIG="/notfound" go test ./...   # 全量单元测试，无需云凭据
golangci-lint run ./...           # lint，与 CI 一致
```

## 测试策略

- 单测克制：单元测试只覆盖纯算法与纯函数（hash 对拍、路径工具、流式拼接的边界等），禁止 mock HTTP 层或 API 行为——mock 出来的协议假设没有验证价值。
- 真实凭证优先：backend 的行为验证一律用真实凭证跑编译产物（`./rclone` + `RCLONE_CONFIG_<REMOTE>_<OPTION>` 环境变量传凭证，凭证不写入任何文件、不入库），配合 `-vv` 与 `--dump bodies` 从请求/响应日志反推代码缺陷。
- 协议存疑 curl 隔离：对上游协议行为有疑问时，先用 curl 复现最小请求序列确认事实（约束、缓存行为、错误语义），拿到结论再写代码；禁止在代码里边猜边试。
- fstests 即契约：动手写 backend 前先通读 `fstest/fstests/fstests.go` 中相关测试段，Mkdir("")/Move/DirMove/NewObject 等接口语义在测试里都有明确断言，以测试为准而非注释直觉。
- 配额预算：API 型 backend 的 fstests 单轮约消耗数千次请求，验收前先估算配额，完整验收控制在单个限流窗口内，避免多轮连跑打爆限额。

## 架构参考

涉及分层设计、包间引用关系、后端注册机制、环境变量优先级等疑问时，先读 `zen-docs/rclone架构与分层设计.md`，其中包含经源码验证的完整结论与关键文件路径。
