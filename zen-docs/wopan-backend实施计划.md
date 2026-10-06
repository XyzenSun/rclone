# wopan backend 实施计划

本文档是 `backend/wopan`（联通云盘，pan.wo.cn）的实施计划。OpenList 在本项目中的定位只有一个：协议参考来源——网盘的私有协议（签名、加密、上传编排）已经由 OpenList 社区逆向并维护，我们直接照抄协议行为，省去自己研究各网盘协议的成本，但**不考虑与 OpenList 的任何兼容性**（凭证格式、挂载语义等一概不管）。协议实现直接复用独立 SDK `github.com/OpenListTeam/wopan-sdk-go`（Apache-2.0，原作者 xhofe，即 AList 作者；OpenListTeam 名下的是 fork 托管），行为参考 OpenList 的 `drivers/wopan` 驱动（调研对象为 `pr-openlist-filen/OpenList`，commit `ea10624`），代码按 rclone 规范从零编写。由于 SDK 许可证为 Apache-2.0 且 driver 侧代码全部重写，本 backend 不涉及任何 AGPL 代码，将来即使公开分发也没有许可证障碍。

改动范围遵守 `AGENTS.override.md` 的"新增 backend 固定触点"，另有一处不可避免的新增依赖（`go.mod`/`go.sum` 追加 wopan-sdk-go），见第 5 节。

## 1. 目标与定位

新增一个名为 `wopan` 的原生 backend，把联通云盘当作文件系统，读写全程直连联通 API（`rest-pc.wocloud.cn` 等域名由 SDK 内部管理），不经过任何中转服务。上传走 SDK 的 `Upload2C`（8MB 分片顺序 form 上传），下载走 `GetDownloadUrlV2` 直链。移动/复制/删除是服务端操作，不传输文件数据。

已知限制：上传为 SDK 内部的顺序分片（无并发分片、无断点续传），大文件上传速度受单流限制；无 hash 能力（同步比较退化为 size + modtime）；不支持覆盖上传（同名文件需先删后传）。

## 2. 设计决策

| 决策点 | 结论 |
|---|---|
| backend 名称 | `wopan` |
| SDK 依赖 | 直接 import `github.com/OpenListTeam/wopan-sdk-go v0.1.5`，通过 `SetHttpClient(fshttp.NewClient(ctx))` 注入 rclone 的 HTTP 栈（超时/代理/keepalive 等全局 flag 生效），参照 cloudinary 注入第三方 SDK 的写法 |
| 空间选择 | 默认个人云；配置 `family_id` 后切换家庭云（与 OpenList 驱动一致：`family_id` 为空即个人空间） |
| 认证方式 | `rclone config` 向导入口先选择方式：① 交互式登录（手机号 + 密码 + 短信验证码，走 SDK 的 `PcWebLogin` → `PcLoginVerifyCode`）；② 直接粘贴 `refresh_token`（可选附带 `access_token`），面向自己抓包获取 token 的用户 |
| token 刷新 | SDK 的 `OnRefreshToken` 回调中用 `fs.ConfigFileSet` 把新 `access_token`/`refresh_token` 写回配置文件；SDK 内部在业务响应码非 0000 时自动刷新并重试一次，rclone 侧无需额外处理 401 |
| 覆盖上传 | wopan 不支持覆盖（OpenList 标记 `NoOverwriteUpload`）。`Update` 检测到目标已存在时先 `Remove` 再上传 |
| hash | 无（`fs.HashNone`） |
| mtime | 只读，取 API 的 `createTime`（格式 `20060102150405`，UTC+8，秒级精度）；`SetModTime` 返回 `fs.ErrorCantSetModTime`，`Precision` 返回 `fs.ModTimeNotSupported` |
| 排序 | 固定 `name_asc`，不暴露配置项。理由：rclone 客户端自行排序，服务端只需保证分页期间排序稳定 |
| 下载 | `GetDownloadUrlV2` 直链 + 标准 GET，Range 透传（是否真支持 Range 在验收时确认，见 6.3） |
| PublicLink | 待验收确认：若 `GetDownloadUrlV2` 返回的直链无需鉴权头即可从任意网络访问，则启用该 Feature；否则不启用 |
| 缩略图 | v1 不做（API 有 `thumbUrl` 字段，结构已预留） |
| 上传大小 | 无硬限制（SDK 按 8MB 分片循环上传） |

## 3. 配置项（Options）

| 选项 | 必填 | 说明 |
|---|---|---|
| `family_id` | 否 | 家庭云 ID；留空使用个人云 |
| `root_folder_id` | 否 | 根目录 ID，默认 `0`（联通的根目录约定） |

（`refresh_token`/`access_token` 为向导管理的隐藏字段，见下文。）

`refresh_token` 与 `access_token` 是向导管理的隐藏字段（`Hide: fs.OptionHideBoth`），不出现在常规 Options 问答中，只能通过向导写入（登录自动填入或用户粘贴）。`access_token` 可省略——仅有 `refresh_token` 时 SDK 会在首次请求失败后自动刷新获取。`rclone config` 向导流程（实现 `fs.Info.Config`，用 `fs/backend_config.go` 的 ConfigIn/ConfigOut 状态机，参照 onedrive 的 `Config` 函数）：

1. Options 问答结束后进入向导，`ConfigChooseExclusive` 选择认证方式：账号密码登录 / 直接粘贴 token。
2. 账号密码登录：`ConfigInput` 询问手机号 → `ConfigPassword` 询问密码 → 调 `PcWebLogin`（触发联通下发短信验证码）→ `ConfigInput` 询问短信验证码 → 调 `PcLoginVerifyCode` 拿到 `access_token`/`refresh_token`。
3. 粘贴 token：`ConfigInput`（或 `ConfigPassword` 掩码显示）粘贴 `refresh_token`，可选粘贴 `access_token`。
4. 两条路径最终都 `ConfigSet` 写入配置。API 报错则向导报错退出，可重新执行 `rclone config` 重试。

## 4. 实现要点

### 4.1 文件结构

```
backend/wopan/
├── wopan.go              init/fs.Register、Options、Config 向导、NewFs、Fs 与 Object 全部实现（不拆 fs.go/object.go）
├── util.go               File→Object 转换、时间解析、spaceType/排序映射、错误分类等纯逻辑
├── wopan_internal_test.go 逻辑单元测试（无网络、无 mock）
└── wopan_test.go         fstests 标准集成测试（真实凭据，见 6.2）
```

### 4.2 NewFs 与目录缓存

ID 型后端标准做法（参照 linkbox）：`fs.NewDirCache` 维护路径→目录 ID 映射，`FindRoot` 从 `root_folder_id`（默认 `0`）逐级解析 root 路径；root 指向文件时按 rclone 约定返回"父目录 Fs + 单文件 Object"。初始化序列照搬 OpenList 驱动的 `Init`：`DefaultWithRefreshToken` → `SetAccessToken` → 注册 `OnRefreshToken` 回调 → `FamilyUserCurrentEncode`（取 `defaultFamilyId`，个人云建目录时需要）→ `InitData`（初始化手机号/分类规则/上传 zone URL，SDK 加密与上传域名依赖这些状态）。

### 4.3 List

`QueryAllFiles` 分页循环：`pageNum` 从 0 起、`pageSize` 100，返回条数小于 100 即终止。`File` 映射：`Type == 0` 为目录；`CreateTime` 按 `20060102150405` 解析为 UTC+8 时间；`Fid`/`Id` 均保留在 Object 上（下载用 `Fid`，其余操作用 `Id`，与 OpenList 驱动一致）。目录变更操作（Mkdir/Move/Remove 等）成功后调用 `dirCache` 对应的 Flush/Invalidate。

### 4.4 下载（Object.Open）

`GetDownloadUrlV2([]string{fid})` 取直链，`http.NewRequestWithContext` 发起 GET，`fs.FixRangeOption` 处理 Range 选项并透传 `Range` 头（模板照 pikpak 的 `httpResponse` 写法）。直链有有效期，遇到 403/410 时重新取链接重试一次（包在 pacer 里）。

### 4.5 上传（Update）

1. 目标已存在时先 `Remove`（覆盖语义）。
2. `Upload2C`：`spaceType`/`familyId` 按 `family_id` 配置推导；`Upload2CFile{Name, Size, Content, ContentType}` 直接取 `src` 的 reader 与 `MimeType`。SDK 内部按 8MB 顺序分片，从 reader 顺序 `LimitReader` 读取，rclone 侧无需缓冲整文件。
3. 进度统计依赖 rclone 的 accounting 包装（Update 的 reader 消耗即计数），SDK 的 `OnProgress` 回调不额外接。
4. 上传成功后 `dirCache.Invalidate` 父目录，并按返回的 `fid` 构造新 Object。

### 4.6 服务端操作

- `Mkdir`：`CreateDirectory`（个人云时 familyId 传 `FamilyUserCurrentEncode` 得到的 `defaultFamilyId`，照搬 OpenList 驱动行为）。
- `Move`/`Copy`：`MoveFile`/`CopyFile`，按对象是否目录拆 `dirList`/`fileList`，启用 `Mover`/`Copier` Feature。
- `DirMove`：`MoveFile` 天然支持目录 ID，启用 `DirMover` Feature。
- `Rename`：`RenameFileOrDirectory`（`_type`：目录 0 / 文件 1）。
- `Remove`/`Rmdir`：`DeleteFile`（目录进 `dirList`，文件进 `fileList`）。
- `About`：`QueryCloudUsageInfo`（`ByteTotalSize`/`ByteUsedSize`），启用 `Abouter`。
- `CleanUp`（M3 可选）：`EmptyRecycleData` 清空回收站。

### 4.7 错误处理与重试

`fs/pacer` 包装所有 SDK 调用。`shouldRetry` 判定：网络错误（`net.Error`/`io.EOF`）、SDK 错误串中的 HTTP 5xx/429、下载直链过期（403/410）。SDK 的业务错误是 `fmt.Errorf` 字符串（`request failed with status: ...` / `rsp_code: ...`），分类逻辑集中在 `util.go` 的纯函数里便于单测。对象不存在映射为 `fs.ErrorObjectNotFound`，目录不存在映射为 `fs.ErrorDirNotFound`（触发 dirCache 回溯刷新）。

## 5. 改动触点清单

1. `backend/wopan/`（新增，四个文件）。
2. `backend/all/all.go`：按字母序在 `webdav` 之前插入 blank import。
3. `go.mod` / `go.sum`：追加 `github.com/OpenListTeam/wopan-sdk-go v0.1.5`。这是标准触点之外的一处必要改动（SDK 依赖 resty v2 与 golang.org/x/net，rclone 已有，无新增传递依赖），需要单独确认。
4. `docs/data/backends/wopan.yaml`：新增（缺失会导致 `fs.Register` 启动报 internal error）。
5. `docs/content/wopan.md`：backend 文档页。
6. `README.md`、`docs/content/docs.md`、`docs/content/_index.md`：按字母序追加条目。
7. `fstest/test_all/config.yaml`：追加 `TestWopan` 注册段。

## 6. 测试与验收

原则：禁止 mock（不搭假服务器、不 stub SDK 接口）；单元测试只覆盖纯逻辑；一切涉及网络的验证都用真实凭证做真实调用。

### 6.1 逻辑单元测试（`wopan_internal_test.go`，无网络）

1. 时间解析：`20060102150405` 正常值、非法值、边界（闰日/年末）。
2. `File`→`Object` 转换：文件/目录判定（`Type` 0/1）、size、名称、异常 `CreateTime` 的错误传播。
3. 分页终止条件：返回条数等于/小于 `pageSize` 时的循环退出判定。
4. `spaceType`/`familyId` 推导：个人云/家庭云组合。
5. `shouldRetry` 错误分类：网络错误、5xx/429 字符串、业务错误、直链过期。
6. Options 解析与默认值（`root_folder_id` 缺省 `0`）。

### 6.2 集成验收（真实凭据，Xyzen 提供）

凭证形态：手机号 + 密码（向导登录，验收时需配合提供短信验证码），以及一组可粘贴的 `refresh_token`（验收粘贴通道用，可由向导登录后从配置文件取出）。测试在账号内使用专用目录 `rclone-test/`，验收后清理。

fstests 标准套件本身就是真实 API 调用（非 mock），配置 `TestWopan` remote 后执行：

```bash
go test -v ./backend/wopan/
```

### 6.3 手工验收清单（按里程碑执行）

**M1 只读路径：**

| 步骤 | 命令 | 通过标准 |
|---|---|---|
| 配置向导（登录） | `rclone config`（走手机号+密码+短信码） | 配置生成，`refresh_token`/`access_token` 写入 |
| 配置向导（粘贴） | `rclone config` 选择粘贴 token，填入 rt（不填 at） | 配置生成，首次命令触发自动刷新并回写 `access_token` |
| 列目录 | `rclone lsd wopan:` / `rclone ls wopan:rclone-test` | 与网页端一致 |
| 详细列表 | `rclone lsl wopan:rclone-test` | size 与 mtime（秒级）正确 |
| 下载小文件 | `rclone copyto wopan:rclone-test/small.bin /tmp/` | md5sum 与本地源一致 |
| Range 下载 | `rclone cat wopan:rclone-test/mid.bin --offset 1048576 --count 4096` | 与 `dd` 截取的本地片段 md5 一致（确认直链支持 Range） |
| 空间信息 | `rclone about wopan:` | total/used 与网页端一致 |
| token 刷新 | 删除配置中 `access_token` 后执行任意命令 | 自动刷新成功且配置文件回写新 token |

**M2 写路径：**

| 步骤 | 命令 | 通过标准 |
|---|---|---|
| 建目录 | `rclone mkdir wopan:rclone-test/d1` | 网页端可见 |
| 上传小文件（<8MB，单片） | `rclone copyto /tmp/small.bin wopan:rclone-test/` | size/md5 一致 |
| 上传中文件（20MB，3 片） | `rclone copyto /tmp/mid.bin wopan:rclone-test/` | size/md5 一致 |
| 上传大文件（200MB，26 片） | `rclone copyto /tmp/big.bin wopan:rclone-test/` | size/md5 一致，进度条持续推进 |
| 覆盖上传 | 对同名文件再次 copyto | 旧文件被替换，不出现重复文件 |
| 删除 | `rclone deletefile` / `rclone rmdir` | 网页端确认删除 |

**M3 服务端操作：**

| 步骤 | 命令 | 通过标准 |
|---|---|---|
| 移动 | `rclone moveto wopan:rclone-test/a.bin wopan:rclone-test/d1/a.bin` | 服务端完成，本地无流量（`-P` 观察无传输进度） |
| 目录移动 | `rclone move wopan:rclone-test/d1 wopan:rclone-test/d2` | 同上 |
| 复制 | `rclone copyto wopan:rclone-test/d2/a.bin wopan:rclone-test/a-copy.bin` | 服务端完成 |
| 重命名 | `rclone moveto wopan:rclone-test/a-copy.bin wopan:rclone-test/a-rename.bin` | 名称变更 |
| PublicLink 判定 | `rclone link wopan:rclone-test/small.bin` | 若返回的直链在无鉴权头环境下可下载则启用该 Feature 并通过；否则确认不启用 |

**M4 收尾：**

| 步骤 | 命令 | 通过标准 |
|---|---|---|
| fstests 套件 | `go test -v ./backend/wopan/` | 全部通过（含 Put/Move/Copy/Remove 标准用例） |
| 无凭据回归 | `RCLONE_CONFIG="/notfound" go test ./...` | 不影响其他包 |
| lint | `golangci-lint run ./backend/wopan/` | 与 CI 一致 |
| 同步演练 | `rclone sync /tmp/src wopan:rclone-test/sync` 两次 | 第二次无传输（size+modtime 判等生效） |

## 7. 里程碑

1. M1 骨架与只读：注册、Options、config 向导、token 刷新回写、NewFs、List、Open、About、文档元数据 yaml。人工验收 6.3-M1。
2. M2 写路径：Update（分片上传、覆盖语义）、Mkdir、Remove、Rmdir。人工验收 6.3-M2。
3. M3 服务端操作：Move、DirMove、Copy、Rename、PublicLink 判定、CleanUp。人工验收 6.3-M3。
4. M4 收尾：fstest 注册与标准套件、回归、lint、文档页完善。

每个里程碑完成后 `make` 编译通过并做定向验收，不做全量测试（除非另行批准）。
