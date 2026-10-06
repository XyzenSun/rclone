# cmcloud backend 设计文档

本文是 `backend/cmcloud` 的实施设计。协议事实全部来自《cmcloud协议验证.md》的真实凭证实测，本文只引用结论不重复样本。实现规范遵循上游 CONTRIBUTING.md 的 "Writing a new backend"：目录型参考 `backend/box`，不拆 fs.go/object.go，HTTP 层用 `lib/rest` + `fs/fshttp`，路径编码用 `lib/encoder`。

## 1. 定位与边界

中国移动云盘（139云盘/和彩云）的 IDaaS 开放通道 backend。走 `openapi.yun.139.com` V3 网关 + `idaas.cmpassport.com` 认证，与网页版私有 API 无关。

核心边界（均为服务端强制，实测确认）：

- root 一律锚定智能体专属目录（`user/folder/query` 返回的 folderId），不做全盘读、不做全盘写。用户配置 `cmcloud:` 即专属目录，`cmcloud:子路径` 即其下路径
- 删除/移动/改名端点被服务端屏蔽（01000005），**不实现** Delete/Rmdir/Purge/Move/DirMove/Rename，Features 表如实声明
- 写天然被限制在专属目录内（越界 16110013），fstests 在随机子目录跑即天然安全，不会触碰用户真实数据

## 2. 目录结构与文件规划

```
backend/cmcloud/
  cmcloud.go        init + fs.Register + Options + Fs/Object 全部实现
  api/
    types.go        请求/响应结构体 + 错误码常量
    auth.go         preAuth 数字信封、取票、票据缓存（crypto 标准库）
    client.go       业务 API 封装（lib/rest）
  cmcloud_test.go   纯函数单测（信封加密对拍、分片规划、错误码映射）
```

其余触点按 fork 规范：`backend/all/all.go` 字母序 blank import、`docs/data/backends/cmcloud.yaml`、`docs/content/cmcloud.md`、`README.md`、`docs/content/docs.md`、`docs/content/_index.md`、`fstest/test_all/config.yaml`。

## 3. Options 设计

用户可见选项最小化。skill 注册凭据（devId/clientId/skillId/strategyId/templateId/scopeIds/公钥）全部硬编码在 `api/auth.go`，不作为 Options 暴露——它们是"这个 backend 作为技能客户端"的身份，用户无从获取也无须配置。

| Option | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `access_token` | string (obscured) | 空 | 长期票据，config 向导写入 |
| `claw_uid` | string | 空 | 设备身份，首次授权时生成并写入，与 access_token 绑定 |
| `upload_cutoff` | fs.SizeSuffix | 5MiB | 以下走单片直传（单片也走 create+PUT+complete 链路，只是 partInfos 只有一片） |
| `chunk_size` | fs.SizeSuffix | 32MiB | 分片大小，约束 5MiB~?（受 partNumber ≤10000 与单批 ≤100 片约束） |

不提供 `root_folder_id`：root 语义已固定为专属目录，提供该选项只会诱导越界写（必然 16110013 失败）。

## 4. 认证设计

不走 `lib/oauthutil`（非标准 OAuth2，无 refresh token 端点），自定义 token 流：

- **config 向导**（`fs.ConfigInput`/`fs.ConfigConfirm` 阶段）：生成 clawUid（uuid4）→ preAuth（不带 uuid）→ 拿到 authPageUrl 展示给用户 → 用户浏览器授权后按回车 → preAuth（带 uuid）→ 103000 → access_token 与 claw_uid 写入 config（obscured）
- **运行期**：creditToken 内存缓存（300 秒，提前 60 秒刷新），失效码（401/110044/130090/16110038/01000003）触发单次重取重试
- **headless 场景**（`rclone config create --non-interactive` 不适用）：文档引导用户先在有浏览器的机器上完成授权，再把 obscured token 复制过去；或 `rclone authorize cmcloud` 子命令形态（暂不实现，文档说明即可）

数字信封实现要点（Go）：

- AES-256-GCM：`crypto/aes` + `cipher.NewGCM`，随机 12 字节 IV，输出 = ciphertext+tag
- RSA-OAEP-SHA256：`crypto/rsa` + `rsa.EncryptOAEP(sha256.New(), ...)`
- MD5 签名：`crypto/md5`（仅协议签名用途）
- payload JSON 序列化注意：skill 用 `ensure_ascii=False`（中文原样），Go `json.Marshal` 会转义中文为 `\uXXXX`。服务端解密后是合法 JSON 可正常解析，但为完全复刻（排除变量），用 `json.Encoder` + `SetEscapeHTML(false)` 仍不够——需要自定义或直接手拼。实施时先用 `\u` 转义版本实测，通过则不折腾；不通过再手拼
- configDelivery 前置调用保留（复刻调用序列，签名 = base64(sha256(traceId+timestamp+"2.0"))）

## 5. Fs / Object 接口映射

| rclone 接口 | API 映射 | 说明 |
| --- | --- | --- |
| NewFs | `user/folder/query` | 取 folderId 作为 root；`cmcloud:` 直接指向它 |
| List / ListR | `file/list` 翻页 | pageSize 100，nextPageCursor 游标；ListR 递归下钻（目录入队） |
| NewObject | List 父目录找名（无按名查询接口） | 找不到返回 fs.ErrorObjectNotFound |
| Mkdir | `file/createFolder` | 逐级建（parentPath 自动建链语义未实测，实施时先验证再决定是否用） |
| Put / Update | `file/create` → PUT → `file/complete` | 见上传设计 |
| Open | `file/getDownloadUrl` → GET | 预签名 URL 直接流式读，支持 Range（S3 预签名天然支持） |
| Copy | `file/batchCopy` + `task/get` 轮询 | 服务端复制；同名冲突服务端自动加时间戳后缀（rclone Copy 语义要求目标不存在，需先检查并返回 fs.ErrorCantCopy 或提前报已存在） |
| Hashes | SHA256 | 服务端 contentHash；上传时本地计算并提交，complete 后以服务端返回为准 |
| ModTime | 只读 | updatedAt；SetModTime 不实现（update 被屏蔽），Precision = fs.ModTimeNotSupported 或毫秒精度只读 |
| Rmdir / Delete / Move / DirMove / Rename / Purge | 不实现 | Features 声明 false |

Object 元数据映射：`fileId`（内部 ID）、`name`、`size`、`updatedAt`（modtime）、`contentHash`（sha256）。fileId 不对外暴露，rclone 路径即身份，NewObject 时按路径解析。

## 6. 上传设计

分片规划（对齐 skill 的 `_plan_parts` 算法）：

- 片数 = min(ceil(size/chunk_size), 100, 10000)，片大小 = max(chunk_size, ceil(size/片数))
- 单批 ≤100 片：create 响应直接给全部分片地址；超出部分按批调 `getUploadUrl` 补取
- 非末片 ≥5MiB（服务端约束），chunk_size 默认 32MiB 天然满足
- 串行上传（parallelUpload 并行模式涉及 parallelHashCtx 哈希上下文协议，复杂且收益存疑，首版不实现）
- 空文件（size=0）：skill 标注分片链路不支持，实施时实测 formUpload 或单片 size=0 的 create 行为，再定方案（fstests 必测空文件）
- 同名冲突：默认 `fileRenameMode: refuse`（rclone 语义：目标已存在应报错而非静默改名）；complete 后以响应的 name 为准更新 Object
- 秒传（proofCode）：首版不实现，contentHash 正常提交（服务端可能自动命中 rapidUpload，命中时跳过传输，代码需处理该分支）

上传缓冲：`lib/pool` 复用分片读缓冲；未知大小流式上传（io.Reader 无 Size）需先落临时文件或 spool（参考 box 的做法）。

## 7. 错误处理设计

- 统一响应解析：HTTP 200 + `success:false` + `code` → 映射为 fs 错误
- `16110013` → fs.ErrorPermissionDenied（越界写，理论上 root 锚定后不应出现）
- `01000005` → fs.ErrorNotImplemented（屏蔽端点，防御性）
- 票据失效码 → 自动换票重试一次（在 client.go 的请求包装层统一做）
- `task/get` 的 `PartialSucceed` → 返回部分成功错误并附 batchFileResults 明细

## 8. Features 表（最终形态）

```go
f.features = &fs.Features{
    CaseInsensitive:         false,
    CanHaveEmptyDirectories: true,   // 目录是独立对象（createFolder），实测空目录可列出
    ServerSideCopy:          true,   // batchCopy
    ServerSideMove:          false,  // 无 move 端点
    Copy:                    true,
    Move:                    false,
    DirMove:                 false,
    Purge:                   false,
    Rmdir:                   false,
    Delete:                  false,
    CleanUp:                 false,
    // Tier、Link 等按 box 模式默认
}
```

fstests 会自动跳过未声明能力的测试段。`fstest/test_all/config.yaml` 注册时注意：Move/DirMove/Rmdir 相关用例依赖 `fstests.InternalTest` 的 feature 探测，无需特殊处理。

## 9. 实施顺序与验证计划

1. `api/auth.go`：数字信封 + preAuth + 取票缓存。单测：与 skill Python 输出对拍（同一 payload 的密文不可比，对拍解密侧/签名侧）；真实验证：用文档第 7 节的 accessToken 直接换 creditToken
2. `api/client.go` + types：list/get/createFolder 三件套，`-vv --dump bodies` 实测
3. Fs/Object 骨架 + NewFs 锚定专属目录 + List
4. 上传链路（先单片小文件，再分片、空文件、同名冲突）
5. 下载 + Copy + task 轮询
6. fstests 全套（单轮，注意配额：API 型 backend 单轮数千请求，控制在一个限流窗口内）
7. 文档与注册触点收尾

每步用真实凭证验证后再进下一步；协议存疑处先 curl 隔离实验（遵循 fork 测试策略）。未实测项清单见《cmcloud协议验证.md》第 8 节，其中空文件、parentPath 建链、fileRenameMode 语义三项在步骤 2/4 落地前必须先验证。

## 10. 实施记录（2026-10-05）

代码已按本文实现（backend/cmcloud/，含 api/ 子包与全部注册触点），编译、
vet、golangci-lint、纯函数单测全部通过。实施中相对本文的偏差与修正：

- **数字信封参数修正**：通读 skill 源码（/workspace/cm-cloud-manage-2.0.0）
  发现 `_generate_aes_key` 实际生成 **16 字节**密钥（AES-128-GCM），
  IV = key 前 12 字节（由双方从 key 派生，不随密文传输），输出
  ct||tag。本文第 4 节与《协议验证文档》2.1 节写的"32 字节 AES-256-GCM
  + 随机 IV"来自 SDK 的陈旧 docstring，以源码实测行为为准。Go 实现
  按 16 字节 + key[:12] IV 复刻。
- **getUploadUrl 不实现**：分片规划（片数 = min(ceil(size/chunk), 100)）
  保证总片数恒不超过 100，create 响应必然携带全部分片地址，补取路径
  不可达，实现它是死代码。
- **Copy 不支持改名**：file/batchCopy 请求体只有 fileIds + toParentFileId，
  无名字字段，只能原名复制到别的目录。目标叶名与源不同时返回
  fs.ErrorCantCopy 让上层回退到上传复制（fstests FsCopy 因此 skip）。
- **覆盖语义**：平台 fileRenameMode 无 overwrite 选项且删除端点被屏蔽，
  Update 命中已存在目标（refuse → exist=true 无上传任务）时返回明确
  报错。fstests 的 ObjectUpdate 等用例进 ignore 清单。
- **fstests ignore 清单**：config.yaml 里的清单是代码分析得出的首轮
  估计（删除端点被屏蔽导致 FsRmdirEmpty/ObjectRemove/FsPurge/
  FsPutChunked 等失败），真实凭证验收时按实际失败增补。
- **单测位置**：信封加解密对拍放在 api/auth_test.go（需要替换包内
  公钥做私钥对拍），cmcloud_test.go 保留分片规划/时间格式/哈希准备
  的纯函数测试与 fstests 入口（含 SetUploadChunkSize/Cutoff）。
- **ModTime 取值**：list/get 返回 localCreatedAt/localUpdatedAt（上传
  时设置），itemModTime 优先 localUpdatedAt、回退 updatedAt，
  Precision = 1ms。localUpdatedAt 是否真实往返待实测确认，不行则
  切换 ModTimeNotSupported。

待真实凭证验证的检查点（对应第 9 节实施顺序）：

1. preAuth：用协议验证文档第 7 节的 accessToken 直接换 creditToken
   （clawUid 必须用文档值，服务端绑定了设备身份）
2. Go json.Marshal 的 \uXXXX 中文转义是否被服务端接受（skill 用
   ensure_ascii=False；设计文档预定先试转义版）
3. 空文件 formUpload 链路（分片链路不支持 size=0）
4. refuse 模式命中同名文件的实际响应形态（exist 字段语义）
5. localCreatedAt/localUpdatedAt 的往返一致性
6. complete 响应是否直接携带完整元数据（协议验证文档说携带，
   skill 适配器注释说未定义）

## 11. 真实凭证验收记录（2026-10-05）

用第 7 节凭证（环境变量注入，不入库）完成全链路验收，全部通过：

- **认证与锚定**：preAuth 换票（Go json.Marshal 的 \uXXXX 中文转义被服务端
  接受，无须手拼原样输出）、folder/query 锚定专属目录、list 与文档记录的
  测试数据一致
- **上传**：单片小文件、3 片多分片（12MB/5MiB）、空文件（formUpload 链路
  实测可用，协议验证文档第 8 节的未实测项落地）、sha256 全部与本地对拍
- **ModTime**：localCreatedAt/localUpdatedAt 毫秒精度完整往返
  （2021-05-06 07:08:09.123 精确保留），Precision=1ms 成立
- **下载**：预签名 GET 流式读 + Range（bytes=0-9）正确
- **复制**：同名跨目录走 batchCopy + task/get（"Copied (server-side copy)"，
  modtime 保留）；改名复制正确回退上传
- **票据**：同进程间隔 260 秒两次调用，creditToken 过期后静默换票无报错
- **fstests 全套**（随机子目录，单轮约 26 分钟）：45 个失败用例逐一根因
  确认全部源于两条已知平台硬限制（删除端点屏蔽 → 清理失败连锁；
  无覆盖语义 → FsPutFiles 更新用例），失败输出中的文件名均与预期一致
  （含全角点/全角斜杠/控制字符/标点的完整往返，无 baidupcs 式目录替换）；
  ignore 清单已按实测失败全量写入 config.yaml

验收中发现并修复两个实现缺陷：

1. **access_token 被 obscure.Reveal 打碎**：最初按 obscured 存储 + Reveal
   失败回退明文的启发式实现，但该 token 恰好能被 obscure 算法"成功"解码
   成乱码字节，payload 里 accessToken 变成二进制垃圾 → 服务端回 103001。
   对照实验（Python SDK 同凭据返回 103000）定位。修复：放弃 obscured
   存储，明文 + Sensitive 标记（与 baidupcs 一致），设计文档第 3 节的
   "(obscured)" 决策作废
2. **服务端裁剪前导空格**：fstests FsEncoding/leading_space 实测发现
   " leading space" 上传后被存成 "leading space"（findObject 失败）。
   修复：默认编码集补 EncodeLeftSpace（尾部空格/尾部点原本已编码，
   前导点、前导控制字符实测不受裁剪）

遗留说明：fstests 两轮在专属目录留下两个随机测试目录（无删除 API 无法
清理，与既有 verify-dir 同类）；FsPutRetry 的故障注入未命中任何上传请求
（"no upload request could be faulted"），重试路径由 pacer 单元语义与
FsPutShortEOF 的短读检测间接覆盖。
