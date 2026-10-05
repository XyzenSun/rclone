# baidupcs backend 实施计划

本文档是 `backend/baidupcs`（百度网盘开放平台，pan.baidu.com/union）的实施计划。OpenList 在本项目中的定位只有一个：协议参考来源——百度网盘开放平台 API 的请求形态、上传编排、错误码语义已经由 OpenList 社区长期维护，我们参考其 `drivers/baidu_netdisk` 驱动（调研对象为 `pr-openlist-filen/OpenList`，commit `ea10624`）的协议行为，代码按 rclone 规范从零编写。百度网盘没有独立 SDK（OpenList 仓库为 AGPL，官方仅提供一个无明确许可证的示例 SDK 压缩包），因此本 backend 不引入任何第三方依赖，全部用 `lib/rest` + `fs/fshttp` 自写；参考 AGPL 源码重写的许可证争议在个人 fork 自用场景下不存在，若将来公开分发需另行评估。

与 wopan/github 两个 backend 不同，本计划的所有关键协议假设都已在调研阶段用真实开发者应用（Xyzen 的 WordAgent，AppID 121971461，已通过上线审核）通过 curl 实测闭环，见第 1 节。改动范围遵守 `AGENTS.override.md` 的"新增 backend 固定触点"，无 go.mod 变更。

backend 命名取 `baidupcs`：PCS 是百度个人云存储 API 体系的传统称呼（数据面域名 `d.pcs.baidu.com` 至今沿用），无下划线符合 rclone 命名惯例，且与将来可能移植的 baidu_photo 留出区分空间。

## 1. 调研已验证事实（curl 实测，2026-10）

以下事实全部来自真实调用，是本计划的直接依据：

| 项 | 实测结论 |
|---|---|
| OAuth 授权码模式 | `authorize`/`token` 端点均正常；`redirect_uri=oob` 与控制台登记的回调地址**并存可用**（伪造回调地址在登录前即被拒：`redirect_uri_mismatch`）；token 端点 GET/POST 均可 |
| token 生命周期 | access_token 30 天（`expires_in=2592000`）；refresh_token 10 年、**单次使用轮换**（每次刷新返回新的，刷新失败旧 refresh_token 作废需重新授权）；code 10 分钟单次有效 |
| 权限边界 | 该应用（老应用、已过审）对 `/` 根目录的 list/precreate/mkdir 全部 errno 0，全盘读写当前可用；但按官方《权限与配额》政策收敛 root 到 `/apps/{appname}` |
| 限频 | 已过审应用为白名单制：连续 15 次 list 无 20012；未审核应用文档标称 10 次/小时（未实测，面向其他自建应用的用户时需在文档中提示） |
| 下载链路 | `filemetas dlink=1` → dlink 拼 `&access_token` → 带 `User-Agent: pan.baidu.com` 请求 → 302 到 `nd*.baidupcs.com` 最终直链（`expires=8h`）→ Range 请求返回 206；大于约 20MB 的文件不带该 UA 会失败 |
| 上传链路 | precreate 到 `/apps` 之外也返回 errno 0 + uploadid（`return_type=1`）；三段式 precreate → superfile2 分片 → create |
| 错误码 | 目录不存在 → `errno -9`；fs_id 不存在 → `errno 0` + 空 list（需判空）；token 失效 → `errno 111 / -6`（OpenList 经验） |
| 应用名查询 | **无官方 API**（接口目录六大类均无应用信息查询；OAuth 响应不含应用名；仅控制台网页与授权页 HTML 有）→ root 由用户手填 |
| 用户信息 | `uinfo` 返回 `vip_type`（0/1/2 → 普通/会员/超会，决定上传分片规格）与 `uk`；`quota` 返回 total/used |

调研期间的临时目录 `/rclone_probe_dir` 已于 2026-10 清理完毕（filemanager delete 实测通过，顺带验证了 delete 语义）。

## 2. 设计决策

| 决策点 | 结论 |
|---|---|
| backend 名称 | `baidupcs` |
| 依赖 | 无第三方 SDK，`lib/rest` + `fs/fshttp` 自写全部协议 |
| 认证方式 | OAuth 2.0 授权码模式，用户自填 AppKey/SecretKey（程序不内置任何凭据）。向导支持两种授权路径：`oob`（默认，控制台零配置，headless 友好）与 `callback`（本机 53682 端口临时 HTTP 服务，需用户先在控制台登记 `http://localhost:53682/`，无浏览器环境提示 `ssh -L 53682:localhost:53682`，超时后允许粘贴完整跳转 URL 兜底） |
| token 管理 | 不用 `lib/oauthutil`，自写（原因：token 走 query 参数而非 Authorization header；token 端点为 GET 语义；refresh_token 单次轮换 + 失败即作废需要精确串行控制，oauthutil 的 golang.org/x/oauth2 自动刷新在轮换语义下有并发踩踏风险）。刷新时机为响应式（errno 111/-6 触发），刷新成功后 `fs.ConfigFileSet` 回写两个 token；刷新用 `sync.Mutex` 串行化防止并发双刷互相作废 |
| root | 用户手填，文档引导填 `/apps/{产品名称}`（如 `/apps/WordAgent`）。`Init` 时 root 不存在则自动 `create isdir=1` 建出（幂等，全新应用首次配置即可用） |
| hash | `fs.HashNone`（百度列表返回的 md5 字段不可信，OpenList 明确不使用）。上传时的秒传判定用 `fs.Hash(ctx, src, hash.MD5)` 取源对象 MD5，取不到则跳过秒传 |
| mtime | 上传时通过 `local_mtime`/`local_ctime` 传给 precreate/create（秒级）；`SetModTime` 返回 `fs.ErrorCantSetModTime`，`Precision` 返回 `fs.ModTimeNotSupported` |
| 覆盖上传 | precreate/create 传 `rtype=3`（同名覆盖），无需先删后传 |
| 空文件 | 百度拒绝空文件上传，`Update` 对 size==0 直接报错（文档明示，同 wopan 零字节限制的处理方式） |
| 上传分片 | 大小由 `vip_type` 决定：0→4MB、1→16MB、2→32MB；分片数上限取官方文档的 1024（OpenList 实测 2048 可用但按文档收敛；1024×4MB=4GB 恰好等于普通用户单文件上限，自洽）；分片并发可配（`upload_concurrency`，默认 3，1..32） |
| 上传域名 | 默认 `https://d.pcs.baidu.com`；每次上传会话先调 `locateupload` 探测动态域名，失败回退默认值 |
| 下载 | `Object.Open` 内部完成：filemetas 取 dlink → 拼 access_token → 带 UA `pan.baidu.com` 的 GET（跟随 302）→ Range 透传；403/410/429/5xx 时经 pacer 重试并重新取 dlink |
| Features | `Mover`/`Copier`/`DirMover`（filemanager 对目录同样生效，已实测 mkdir，move 语义实现期确认）、`Abouter`（quota）；无 PublicLink、无 CleanUp（回收站 API 未调研） |
| 排序 | list 固定 `order=name` + `asc`，保证分页期间排序稳定，客户端自行排序 |

## 3. 配置项（Options）

| 选项 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `app_key` | 是 | — | 控制台应用的 AppKey（client_id） |
| `app_secret` | 是 | — | SecretKey（client_secret），`fs.OptionHidePassword` 掩码 |
| `root` | 是 | `/apps/` | 应用目录路径，向导引导补全为 `/apps/{产品名称}` |
| `upload_concurrency` | 否 | `3` | 分片上传并发数，1..32 |

（`access_token`/`refresh_token` 为向导管理的隐藏字段，`fs.OptionHideBoth`。）

`rclone config` 向导流程（实现 `fs.Info.Config`，ConfigIn/ConfigOut 状态机，参照 onedrive）：

1. Options 常规问答（app_key、app_secret、root 等）。
2. `ConfigChooseExclusive` 选授权方式：oob（默认）/ 本机回调。
3. oob 路径：拼授权 URL（`response_type=code&client_id&redirect_uri=oob&scope=basic,netdisk`）显示在提问文案中 → 用户浏览器完成授权 → `ConfigInput` 粘贴页面显示的 code → GET token 端点换 token（`redirect_uri=oob`）。
4. callback 路径：起 53682 临时服务 → 显示授权 URL（`redirect_uri=http://localhost:53682/`）与 `ssh -L` 提示 → 等待回调接 code（超时 5 分钟）→ 超时后 `ConfigInput` 允许粘贴浏览器地址栏里的完整跳转 URL，从中解析 code → 换 token。
5. `ConfigSet` 写入 `access_token`/`refresh_token`，向导结束。
6. 首次 `NewFs`：`uinfo` 验证 token（顺带取 vip_type）→ root 不存在则自动 mkdir。

token 刷新（运行期）：任何 API 响应 `errno 111/-6` → 加锁 → GET token 端点 `grant_type=refresh_token` → 成功则回写两个 token 并重试原请求一次；失败则报错并提示需重新执行 `rclone config` 授权（旧 refresh_token 已作废，不能循环重试）。

## 4. 实现要点

### 4.1 文件结构

```
backend/baidupcs/
├── baidupcs.go              init/fs.Register、Options、Config 向导、token 管理、NewFs、Fs/Object 实现（不拆 fs.go/object.go）
├── api.go                    API 请求封装（rest client、errno 解析、各端点的请求/响应类型）
├── upload.go                 上传编排（hash 计算、precreate/superfile2/create、分片并发、uploadid 过期重传）
├── util.go                   纯逻辑（分片规格推导、block_list 组装、错误分类、时间/路径处理）
└── baidupcs_test.go          fstests 标准集成测试（真实凭据）
```

### 4.2 NewFs 与 List

百度网盘是**路径寻址**（list 的 `dir` 参数直接传路径），不需要 DirCache——这比 wopan 的 ID 寻址简单一档。`NewFs`：`uinfo`（验证 token + 取 vip_type）→ root 存在性检查（list 一次，`errno -9` 则 mkdir）→ root 指向文件时按 rclone 约定返回"父目录 Fs + 单文件 Object"。

`List`：`GET /xpan/file?method=list&dir={path}&start&limit=1000&order=name&web=web` 分页循环，返回条数小于 limit 即终止。`File` 映射：`isdir==1` 为目录；名称取 `server_filename`（空则取 path 末段）；mtime 取 `server_mtime`（0 则回退 `local_mtime`）；Object 保存 `fs_id`（下载用）与 `path`（服务端操作用）。

### 4.3 下载（Object.Open）

`GET /xpan/multimedia?method=filemetas&fsids=[{fs_id}]&dlink=1` → 取 `list[0].dlink`（空 list 映射 `fs.ErrorObjectNotFound`）→ `dlink + "&access_token="` 发 GET，Header `User-Agent: pan.baidu.com`（必须，>20MB 文件硬性要求），http client 默认跟随 302 到 `nd*.baidupcs.com`。`fs.FixRangeOption` 处理 Range 选项并透传（实测 206 可用）。直链 8 小时有效，403/410/429/5xx 时 pacer 重试并重新取 dlink。模板照 wopan 的 `Open` 写法。

### 4.4 上传（Update）

百度 precreate 要求在上传开始前就提交全量 hash（`block_list` 每片 MD5、`content-md5` 全文件 MD5、`slice-md5` 前 256KB MD5），因此必须先把整个输入读一遍：

1. size==0 直接报错（百度不接受空文件）。
2. spool：`io.MultiWriter`（全文件 MD5、前 256KB MD5、各分片 MD5、临时文件）单遍流式读完。源对象有 MD5 时先单独尝试一次秒传 precreate（命中即返回，省掉 spool）。
3. precreate（`rtype=3`、`autoinit=1`、`block_list`、`content-md5`、`slice-md5`、`local_mtime`/`local_ctime`）：`return_type=2` 为秒传命中，直接构造 Object 返回（注意用本地时间覆盖返回的 ctime/mtime——百度 create/precreate 响应时间恒为当前时间，OpenList 已踩坑）。
4. `locateupload` 探测上传域名（失败用 `d.pcs.baidu.com`）。
5. 分片并发上传：`POST {域名}/rest/2.0/pcs/superfile2`，query 带 `method=upload&type=tmpfile&path&uploadid&partseq&access_token`，body 为 multipart 单文件字段（从临时文件 `io.NewSectionReader` 读取）。每片经 pacer 重试（3 次退避）。响应含 "uploadid" + "invalid/expired/not found" 判定为 uploadid 过期：重新 precreate（不带 md5）并全量重传所有分片。
6. create（`rtype=3`、`uploadid`、`block_list`、`local_mtime`/`local_ctime`）→ 构造新 Object，删除临时文件。

进度统计的已知限制：accounting 只覆盖 spool 阶段的读取（读完即 100%），分片上传阶段无逐片进度，与 flickr 等 spool 型 backend 一致。

### 4.5 服务端操作

- `Mkdir`：`create isdir=1`（实测可用）。
- `Move`/`Copy`/`DirMove`：`POST /xpan/file?method=filemanager&opera=move|copy`，form 传 JSON filelist（`path/dest/newname`），`async=0`。目录与文件同一接口。`ondup` 取值（fail/overwrite）在实现期 curl 实测后对齐 rclone 覆盖语义。
- `Remove`/`Rmdir`：`opera=delete`。**Rmdir 必须先 list 确认目录为空**——百度 delete 会递归删除非空目录，直接调用会破坏 rclone 的空目录删除语义。
- `About`：`GET /api/quota` → total/used。

### 4.6 错误处理与重试

所有 API 调用包 `fs/pacer`。errno 分类集中在 `util.go` 纯函数：`111/-6` → 刷新 token 后重试一次；`-9` → `fs.ErrorDirNotFound`；`31023`/空 filemetas list → `fs.ErrorObjectNotFound`；`20012` → 限频，指数退避重试；`20013`/`20011` → 权限/用户数超限，不可重试直接报错；HTTP 层 5xx/429 与网络错误照常重试。errno 语义参考 OpenList 驱动与官方错误码文档，未覆盖的 errno 原样透出数字并附文档链接。

## 5. 改动触点清单

1. `backend/baidupcs/`（新增，四个文件）。
2. `backend/all/all.go`：按字母序插入 blank import。
3. `docs/data/backends/baidupcs.yaml`：新增（缺失会致 `fs.Register` 启动报 internal error）。
4. `docs/content/baidupcs.md`：backend 文档页（含"自建应用需在控制台完成开发者认证、未审核应用有限频"的提示，以及 root 填 `/apps/{产品名称}` 的引导）。
5. `README.md`、`docs/content/docs.md`、`docs/content/_index.md`：按字母序追加条目。
6. `fstest/test_all/config.yaml`：追加 `TestBaidupcs` 注册段。
7. 无 go.mod/go.sum 变更（零第三方依赖）。

## 6. 测试与验收

原则：禁止 mock；按用户决策**不编写单元测试**，一切验证直接用真实凭据（WordAgent 应用 + Xyzen 账号）做真实调用——分片规格、三重 MD5、errno 分类等纯逻辑的正确性均通过 6.2 手工验收的真实行为覆盖。

### 6.1 集成验收（fstests + 手工）

fstests 标准套件（配置 `TestBaidupcs` remote，root 指向 `/apps/WordAgent/rclone-test`）：

```bash
go test -v ./backend/baidupcs/
```

已知预期失败：空文件上传用例（百度硬性拒绝，在测试文件中跳过并注明原因）。

凭证供给方式：调研会话（2026-10）已通过 oob 授权拿到一组有效 token，其 refresh_token（10 年有效）由 Xyzen 自行留存，测试时使用。按项目规则凭证不写入任何文件、不入库，运行时经环境变量传入：`RCLONE_CONFIG_BAIDUPCS_TYPE`/`RCLONE_CONFIG_BAIDUPCS_APP_KEY`/`RCLONE_CONFIG_BAIDUPCS_APP_SECRET`/`RCLONE_CONFIG_BAIDUPCS_REFRESH_TOKEN`/`RCLONE_CONFIG_BAIDUPCS_ROOT`。refresh_token 失效（刷新失败即作废）时重新走一次 oob 授权即可。

### 6.2 手工验收清单（按里程碑执行）

**M1 只读路径：**

| 步骤 | 命令 | 通过标准 |
|---|---|---|
| 向导（oob） | `rclone config` 走 oob 路径 | code 换 token 成功，两个 token 写入配置 |
| 向导（callback） | `rclone config` 走本机回调路径 | 浏览器授权后自动接 code；不登记回调时按提示报错清晰 |
| root 自动创建 | 配置全新 root `/apps/WordAgent` | Init 自动建目录，网页端"我的应用数据"下可见 |
| 列目录 | `rclone lsd baidupcs:` / `rclone ls baidupcs:` | 与网页端一致 |
| 详细列表 | `rclone lsl baidupcs:` | size 与 mtime（秒级）正确 |
| 下载小文件 | `rclone copyto baidupcs:rclone-test/small.bin /tmp/` | md5sum 与本地源一致 |
| Range 下载 | `rclone cat baidupcs:rclone-test/mid.bin --offset 1048576 --count 4096` | 与 `dd` 截取片段 md5 一致 |
| 大文件下载（>20MB） | `rclone copyto baidupcs:rclone-test/big.bin /tmp/` | UA 逻辑生效，md5 一致 |
| 空间信息 | `rclone about baidupcs:` | total/used 与网页端一致 |
| token 刷新 | 配置中篡改 access_token 后执行任意命令 | errno 111 触发刷新，配置回写新 token，命令成功 |

**M2 写路径：**

| 步骤 | 命令 | 通过标准 |
|---|---|---|
| 建目录 | `rclone mkdir baidupcs:rclone-test/d1` | 网页端可见 |
| 上传小文件（<4MB 单片） | `rclone copyto /tmp/small.bin baidupcs:rclone-test/` | size/md5 一致 |
| 上传中文件（20MB 多片） | `rclone copyto /tmp/mid.bin baidupcs:rclone-test/` | size/md5 一致 |
| 上传大文件（200MB） | `rclone copyto /tmp/big.bin baidupcs:rclone-test/` | size/md5 一致 |
| 秒传 | 删除后重新上传同一文件（源带 MD5） | precreate return_type=2，无分片流量（`-vv` 日志确认） |
| 覆盖上传 | 对同名文件再次 copyto | rtype=3 生效，旧文件被替换 |
| 空文件 | `rclone copyto /tmp/empty.bin baidupcs:rclone-test/` | 报错信息明确（非静默失败） |
| 删除 | `rclone deletefile` / `rclone rmdir` | 网页端确认；rmdir 对非空目录拒绝 |

**M3 服务端操作：**

| 步骤 | 命令 | 通过标准 |
|---|---|---|
| 移动 | `rclone moveto baidupcs:rclone-test/a.bin baidupcs:rclone-test/d1/a.bin` | 服务端完成，无本地流量 |
| 目录移动 | `rclone move baidupcs:rclone-test/d1 baidupcs:rclone-test/d2` | 同上 |
| 复制 | `rclone copyto baidupcs:rclone-test/d2/a.bin baidupcs:rclone-test/a-copy.bin` | 服务端完成 |
| 同步判等 | `rclone sync /tmp/src baidupcs:rclone-test/sync` 连跑两次 | 第二次无传输（size+modtime 判等） |

**M4 收尾：**

| 步骤 | 命令 | 通过标准 |
|---|---|---|
| fstests 套件 | `go test -v ./backend/baidupcs/` | 除空文件已知例外全部通过 |
| 无凭据回归 | `RCLONE_CONFIG="/notfound" go test ./...` | 不影响其他包 |
| lint | `golangci-lint run ./backend/baidupcs/` | 与 CI 一致 |

## 7. 里程碑

1. M1 骨架与只读：注册、Options、config 向导（双路径）、token 刷新回写、NewFs（root 自动创建）、List、Open、About、文档元数据。人工验收 6.2-M1。
2. M2 写路径：Update（spool + 三重 MD5 + 三段式上传 + 秒传 + 覆盖）、Mkdir、Remove、Rmdir。人工验收 6.2-M2。
3. M3 服务端操作：Move、DirMove、Copy。人工验收 6.2-M3。
4. M4 收尾：fstest 注册与标准套件、回归、lint、文档页完善。

每个里程碑完成后 `make` 编译通过并做定向验收，不做全量测试（除非另行批准）。

## 8. 实施验收记录（2026-10-04，真实凭据实测）

fstests 标准套件通过（唯一失败 FsPutZeroLength 为已知例外，已在 fstest/test_all/config.yaml ignore）。验收过程中发现多处与计划假设不符的协议事实，均已按实测修正代码：

**create 端点的反直觉行为（事故级）**

- 对已存在路径调用 create（isdir=1）不返回 -8，而是返回 errno 0 并在同一父目录额外创建一个空的 `{name}_{timestamp}` 副本（系统目录如 /apps 则返回 errno 102）。逐段盲建目录会在网盘根目录刷出大量垃圾目录，mkdirServer 必须先向上探测已存在前缀、只对缺失段调用 create。
- create 不会自动补齐父目录，深层缺失路径返回 -7 或 31500。
- list 对指向文件的 dir 参数返回 errno 0 加空列表，与空目录无法区分，探测存在性必须查父目录。

**路径清洗事故（数据损失根因）**

encoder 的 FromStandardName 会先做 Standard.Decode（全角→ASCII）。标准名 `．`（全角点）被解码成 ASCII `.` 后，`path.Join` 的 Clean 会把该段直接消掉，create 于是落在父目录路径上——服务端用 100 字节的文件**替换掉了整个父目录**（fstests 的 dot/dot_dot 编码测试触发，/WordAgent 测试目录被毁，数据为一次性测试数据，用户个人文件未受影响）。同理全角斜杠会解码成 `/` 导致路径分裂。因此默认编码必须包含 EncodeDot 与 EncodeSlash（github backend 有同款踩坑注释）。百度对文件名的实际接受度很宽（前导空格/波浪号/各类符号/全角字符均接受且往返一致），仅尾部空格与尾部点会被服务端裁剪（需 EncodeRightSpace/EncodeRightPeriod）、原始控制字符被拒（-7）、文档声明的 `\ / : * ? " < > |` 被拒。

**秒传不生效（与计划假设不符）**

该应用（已过审老应用）下秒传完全不触发：官方客户端上传的现存内容、开放平台刚上传的内容、全零常见内容三种来源的 precreate 均返回 return_type=1；OpenList 式的 create+block_list 秒传恒返 errno 31500。已删除 create 秒传尝试（每次上传白耗一次调用），保留 precreate return_type=2 分支（零成本，百度将来放开即自动生效）。

**其他实测结论**

- filemanager move 会把 server_mtime 重置为移动时间但保留 local_mtime，fileModTime 必须优先 local_mtime（官方客户端上传的文件同样如此：local_mtime 是真实修改时间，server_mtime 是上传时间）。
- filemanager move 目标冲突返回 errno 12 + info 内 -8，且会留下空的时间戳重命名目录；DirMove 需预检目标存在并映射 fs.ErrorDirExists 让上层回退逐文件移动。目标父目录不存在时 move 也不会自动补齐，需先 mkdirServer。
- access_token 格式错误返回 errno 20017（非 111），已并入刷新触发集；token 过期为 111。
- 环境变量覆盖选项时 fs.NewFs 会把 configName 改成带 {hash} 后缀的缓存名，token 回写必须走 NewFs 传入的 configmap（其 setter 绑定真实配置段），用 fs.ConfigFileSet(f.name, ...) 会写错段。
- 列表/下载的 md5 字段是混淆值（含非 hex 字符），印证"md5 不可信"。
- 非会员上传约 450KB/s、下载限速更严（冷备份场景可接受）；分片上传/下载/Range/覆盖上传（rtype=3）/token 刷新轮换回写均验证通过。

**回调模式验收补充（2026-10-05）**

回调登记必须与发送值精确一致：`http://localhost:53682/`（含结尾斜杠），少一个斜杠即 redirect_uri_mismatch。异机器场景（浏览器与 rclone 不在同机）自动回调不可达属预期，向导的"粘贴完整跳转 URL"兜底路径实测可用（extractCode 提取 code → token 交换成功）；再次授权产生独立 token 会话，不会作废已有 remote 的 refresh_token。
