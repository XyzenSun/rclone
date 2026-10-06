# 在 rclone 中实现 OpenList 的上游

本文档是把 OpenList 上游网盘移植为 rclone 原生 backend 的方法论，沉淀自 wopan 与 github 两个 backend 的调研过程。OpenList 在本 fork 中的定位只有一个：**协议参考来源**。各网盘的私有协议（请求签名、参数加密、上传编排、风控细节）已由 OpenList 社区逆向并持续维护，我们照抄协议行为以省去自己研究网盘协议的成本，但不考虑与 OpenList 的任何兼容性——凭证格式、挂载语义、API 语义一概以 rclone 自身规范为准。每个 backend 的具体实施见各自的实施计划（`github-backend实施计划.md`、`wopan-backend实施计划.md`，后续上游照此格式追加）。

## 1. 总体架构：哪些上游值得移植

不是所有 OpenList 上游都要做成原生 backend。决策依据是数据面形态：

- **rclone 已有原生 backend 的上游**（onedrive、s3、drive、dropbox、mega 等）绝不移植，直接用官方 backend。
- **OpenList 没有对应 driver 的服务**（如 TeraCloud）不需要移植，rclone 现有 backend（webdav 等）直连即可。
- **其余上游**（主要是国产网盘）分两种处理：需要上传直连/高性能的做成原生 backend；低频使用的继续走 "rclone webdav → OpenList"，注意把 OpenList 存储的 WebDAV 策略设为 `302_redirect`，下载即直连不经中转（上传仍中转，低频场景可接受）。

## 2. 评估一个上游：读 driver 的方法

评估的入口是 OpenList driver 的三个文件：`drivers/<name>/driver.go`（接口实现）、`util.go`（协议细节）、`meta.go`（配置与能力声明）。重点看四件事，每件都直接决定移植成本：

**第一，数据面认证形态。** 读 `Put` 与 `Link` 的实现，看上传/下载的每个数据请求靠什么通过认证。这是最重要的分类标准：

| 形态 | 判别特征 | 代表上游 | 对移植的影响 |
|---|---|---|---|
| 自包含直链 | PUT/GET 的 URL 自带签名，无需额外凭据 | aliyundrive_open、139（新 API）、quark_open（每片带签名 header） | 移植后 rclone 直连数据面，协议简单 |
| 委托凭证 | STS 临时密钥或 upload session | 115_open（OSS STS）、onedrive | 需在 rclone 侧实现 token 轮换 |
| 账号凭据绑定 | 每个数据请求带账号 token/cookie | 123_open、baidu_netdisk、wopan、github | rclone 必须持有并刷新账号凭据，风控压力最大 |

这个分类同时回答另一个问题：如果哪天想走 "OpenList 侧补直传" 路线（给 driver 实现 `DirectUploader` 接口），只有前两类可行——第三类直传等于把账号凭据交给客户端，失去 OpenList 的凭据隔离意义。本 fork 已决策走原生 backend 路线，此分类主要用于预估移植难度。

**第二，凭据与刷新机制。** 看 `Init` 与 driver 的 Addition 字段：凭据是什么（refresh_token、cookie、PAT）、怎么过期、SDK 或 driver 内有没有现成的刷新逻辑。rclone 侧要自己实现刷新并持久化（见第 4 节）。

**第三，上传编排复杂度。** 数一下 `Put` 里的 API 步骤（pre/create → 分片 → commit），是否要求预计算 hash（全文件/分片/前缀 hash，如百度的 slice-md5）、是否支持秒传、是否支持覆盖上传（`meta.go` 的 `NoOverwriteUpload`）。hash 要求要映射到 rclone 的 HashType 体系；不支持覆盖的用先删后传。

**第四，风控与耦合。** 看请求是否带特殊 UA/Origin/Referer、参数是否加密（如 wopan 的 `EncryptParam`）、是否有限流退避。同时确认依赖面：driver 依赖的是独立 SDK 还是 OpenList 的 `internal/` 框架代码——前者可直接 import，后者必须手工重写。

## 3. 依赖与许可证

移植时优先找独立 SDK。OpenList 生态有一批独立仓库的 SDK（Apache-2.0，与 rclone 的 MIT 兼容，可直接进 `go.mod`）：`wopan-sdk-go`、`115-sdk-go` 等，driver 只是它们的薄封装，移植成本最低。没有独立 SDK 的（139、baidu、github 等），协议代码长在 OpenList 的 AGPL 仓库里，只能参考其行为从零重写——个人 fork 自用没有法律问题，但若将来公开分发，参考 AGPL 源码重写的 backend 存在许可证争议，需另行评估。判断依据写进各 backend 实施计划的前言里。

## 4. rclone 侧的通用移植模式

以下模式在每个 backend 中复用，具体写法参照已实施的 backend：

**HTTP 栈注入。** 第三方 SDK 一律用 `SetHttpClient(fshttp.NewClient(ctx))` 注入 rclone 的 HTTP 客户端（参照 cloudinary 的做法），使全局 flag（超时、代理、TLS 设置）对 SDK 生效。自写协议的用 `lib/rest` + `fshttp`。

**凭据持久化。** SDK 的 token 刷新回调（如 wopan 的 `OnRefreshToken`）或自写的刷新逻辑里，用 `fs.ConfigFileSet(name, key, value)` 把新凭据写回配置文件，用户无感知。`refresh_token`/`access_token` 类字段用 `Hide: fs.OptionHideBoth` 隐藏，不进常规 Options 问答。

**config 向导。** 需要交互式登录的（短信验证码、扫码）实现 `fs.Info.Config`，用 `fs/backend_config.go` 的 ConfigIn/ConfigOut 状态机（`ConfigInput`/`ConfigPassword`/`ConfigChooseExclusive`/`ConfigSet`），参照 onedrive 的 `Config` 函数。向导入口提供"账号密码登录"与"直接粘贴 token"（面向抓包用户）两条路径。

**ID 型目录解析。** 国产网盘几乎都是 ID 寻址而非路径寻址，用 `fs.NewDirCache` 维护路径→ID 映射（参照 linkbox），写操作成功后 Invalidate 对应目录，遇到 `fs.ErrorDirNotFound` 时回溯刷新重试。

**上传。** `Update` 的 reader 直接交给协议层（分片上传天然顺序读，无需缓冲整文件）；进度统计依赖 rclone 对 reader 的 accounting 包装。不支持覆盖的上游先 `Remove` 再传。

**错误处理。** 所有协议调用包 `fs/pacer`，`shouldRetry` 判定网络错误、5xx/429、直链过期；对象/目录不存在映射为 `fs.ErrorObjectNotFound`/`fs.ErrorDirNotFound`。SDK 返回的字符串错误集中在一个纯函数里分类，便于单测。

**能力声明。** 按 driver 实际能力开 Feature：服务端移动/复制开 `Mover`/`Copier`/`DirMover`；空间查询开 `Abouter`；hash 无则 `fs.HashNone`，有则映射到 rclone HashType（自定义类型用 `fs/hash.RegisterHash`，如 github 的 gitsha1）；`SetModTime` 不支持就返回 `fs.ErrorCantSetModTime` 并让 `Precision` 返回 `fs.ModTimeNotSupported`。

## 5. 改动触点（每个 backend 固定套路）

1. `backend/<name>/`：主文件（不拆 fs.go/object.go）、`util.go`（纯逻辑）、`<name>_internal_test.go`（逻辑单测）、`<name>_test.go`（fstests）。
2. `backend/all/all.go`：按字母序插一行 blank import。
3. `go.mod`/`go.sum`：仅当引入独立 SDK 时追加（超出标准触点，需单独确认）。
4. `docs/data/backends/<name>.yaml`：必须创建，否则 `fs.Register` 启动报 internal error。
5. `docs/content/<name>.md`、`README.md`、`docs/content/docs.md`、`docs/content/_index.md`：按字母序追加。
6. `fstest/test_all/config.yaml`：追加 `Test<Name>` 注册段。
7. `zen-docs/<name>-backend实施计划.md`：实施计划。

## 6. 测试与验收方法

原则：**禁止 mock**（不搭假服务器、不 stub SDK 接口），单元测试只覆盖纯逻辑（类型转换、时间解析、分页终止、错误分类、Options 默认值），一切涉及网络的验证用真实凭证做真实调用。验收分两层：

- fstests 标准套件（`go test -v ./backend/<name>/`，配置 `Test<Name>` remote 后运行）本身就是真实 API 集成测试，覆盖 Put/Move/Copy/Remove 等标准用例。
- 手工验收清单按里程碑执行（配置向导、列目录、分片边界文件上传下载对拍、Range 下载、token 刷新回写、服务端操作无本地流量、sync 二次判等），命令与通过标准写在各 backend 的实施计划里。

每个里程碑完成后 `make` 编译通过并做定向验收，不做全量测试（除非另行批准）。

## 7. 已调研上游档案

调研基于 `pr-openlist-filen/OpenList`（commit `ea10624`），结论如下，后续新上游的调研结果追加到此表：

| 上游 | 数据面形态 | SDK | 移植评估 |
|---|---|---|---|
| wopan 联通云盘 | 账号凭据绑定（表单带 accessToken，参数加密） | `wopan-sdk-go`（Apache-2.0） | 已立项，见 `wopan-backend实施计划.md` |
| github | 账号凭据绑定（PAT），无独立数据面 | 无 | 已立项，见 `github-backend实施计划.md`；API 稳定无风控，成本低 |
| 139 移动云盘 | 自包含直链（新 API：create/complete 编排 + 每片签名 URL） | 无 | 候选；需 SHA256 全文件 hash，建议先做个人云 |
| baidu_netdisk | 账号凭据绑定（query 带 access_token）+ 多重 MD5 | 无 | 已立项，见 `baidupcs-backend实施计划.md`；OAuth/权限边界/上传下载链路已用真实开发者应用 curl 实测闭环，root 按官方政策收敛到 /apps/{appname}，零第三方依赖 |
| aliyundrive_open | 自包含直链（每片 OSS 签名 URL） | 无 | 候选；create/complete 编排 + proof_code |
| quark_open | 自包含直链（每片 URL + 每片签名 header） | 无 | 候选；pre/commit 编排 + md5/sha1 |
| 115_open | 委托凭证（OSS STS + multipart） | `115-sdk-go`（Apache-2.0） | 候选；SDK 可直接复用，需实现 STS 轮换 |
| 123_open | 账号凭据绑定（每片 Bearer token + 每片 MD5） | 无 | 候选；协议需整体搬到 rclone |
| teracloud | —（OpenList 无 driver） | — | 不移植，rclone webdav 直连 |
