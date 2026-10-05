# AGENTS.override.md

本文件是 XyzenSun/rclone fork 的项目规则，由 pi 优先加载并取代同目录上游 `AGENTS.md`（该文件保留原样不改动，以避免与上游 merge 冲突；`CLAUDE.md` 同理保留）。上游 AGENTS.md 中对本 fork 仍有效的构建与测试命令已吸收到本文。

## 项目定位

这是对 rclone/rclone 的个人 fork，长期目标只有一个：在保持与官方最大兼容的前提下新增自用 backend。所有工程决策都服务于"官方更新时可以直接 merge"这一前提。

## 核心约束（最高优先级，与全局规范冲突时以本节为准）

版本号与官方保持一致：`VERSION` 文件跟随上游，不自行修改，不打自定义 tag，构建版本信息完全由上游状态决定。

尽量同步官方：上游同步统一通过 GitHub 进行（fork 页面 Sync fork 或 PR），本地不添加 upstream remote。master 分支保持与上游一致，魔改一律在长期分支（如 `zen`）上进行，需要发布时再合入。

尽量兼容官方：改动面最小化是硬性要求。上游高频变更的核心代码一律不碰，具体禁区见下文。

CLI 命令完全不变：不新增、修改、删除任何子命令、全局 flag、配置文件格式、环境变量语义与 rc API；现有 backend 的行为保持原样。用户侧配置与命令行用法与官方二进制完全一致。

## 目录结构

```
backend/   70 个存储后端；all/all.go 是 blank import 聚合入口；新增 backend 的唯一主要改动区
cmd/       CLI 子命令，cmd/all 聚合；本 fork 禁改
fs/        核心抽象（接口/Features/注册表/全局参数）；本 fork 禁改
lib/       基础库（rest/oauthutil/encoder/multipart/pool 等），新 backend 优先复用
fstest/    集成测试框架
vfs/       mount 虚拟文件系统；本 fork 禁改
docs/      文档；data/backends/*.yaml 是 backend 元数据（被 embed 进二进制）
zen-docs/  fork 的私有笔记（已跟踪入库）
```

## 允许的改动范围（新增 backend 的固定触点）

新增 backend 时只触碰以下位置，除此之外的改动需要先与用户确认：

- 动手前先重读 `zen-docs/经验.md`，并 `grep -rn 踩坑 backend/` 扫一遍既有实现的注释，把历史教训变成检查清单（baidupcs 交付时重犯了 github 已记录的 encoder 全角点教训并造成目录被替换，教训见该文档）。
- `backend/<name>/`：全新目录，包含 `<name>.go`（init + fs.Register + Options struct + Fs/Object 实现）、`api/types.go`、`<name>_test.go`。实现规范遵循上游 CONTRIBUTING.md 的 "Writing a new backend"：目录型参考 box，桶型参考 b2；不拆 fs.go/object.go；HTTP 型优先用 lib/rest + fs/fshttp；路径编码用 lib/encoder；上传缓冲用 lib/multipart/lib/pool。
- `backend/all/all.go`：按字母序插入一行 blank import。
- `docs/data/backends/<name>.yaml`：必须创建，否则 fs.Register 启动时报 internal error。
- `docs/content/<name>.md`、`README.md`、`docs/content/docs.md`、`docs/content/_index.md`：按字母序追加条目。
- `fstest/test_all/config.yaml`：追加测试注册段。
- `zen-docs/`：笔记与设计文档。

## 禁改区与替代方案

`fs/`、`fs/operations`、`fs/sync`、`fs/config/`、`vfs/`、`cmd/` 及所有现有 `backend/*` 均为禁改区，这些是上游高频变更区或行为兼容承诺的载体。需要新能力时，通过新 backend 自身的 Options 与 Features 表达，而不是修改核心层；需要调整参数默认值时，通过环境变量在部署侧完成，而不是改代码。

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
