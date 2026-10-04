# github backend 实施计划

本文档是 `backend/github` 的实施计划，已与 Xyzen 逐项确认设计决策。协议实现参考 OpenList 的 `drivers/github` 驱动（调研对象为 `pr-openlist-filen/OpenList`，分支 `feat(driver)/lark`，commit `ea10624`），但架构完全按 rclone 规范从零编写，不复制其代码。改动范围严格遵守 `AGENTS.override.md` 的"新增 backend 固定触点"，不触碰任何禁改区。

## 1. 目标与定位

新增一个名为 `github` 的原生 backend，把 GitHub 仓库（指定 ref）当作文件系统，全程直连 GitHub REST API（`api.github.com`）与 raw 直链（`raw.githubusercontent.com`），不经过任何中转服务。所有写操作通过 Git Data API（blobs → trees → commits → refs）完成，每次写入产生一个 commit；移动/复制/删除是纯 tree 操作，天然是服务端操作，不传输文件数据。

适用场景是文档、配置、小文件的仓库化管理与同步。不适合大文件：Git Blob API 单 blob 上限 100MB（本 backend 硬限制），base64 编码有 33% 传输开销，且每次写入需要 3-4 个 API 调用，受 GitHub 认证用户 5000 次/小时限流约束。

## 2. 已确认的设计决策

| 决策点 | 结论 |
|---|---|
| backend 名称 | `github` |
| 上传大小限制 | 上传前检查文件大小，超过 100MB 直接返回错误，不发起请求 |
| hash 支持 | 通过 `fs/hash.RegisterHash` 注册自定义 `gitsha1` 类型（`sha1("blob <size>\x00" + content)`），使 `--checksum` 同步与 `rclone check` 可用 |
| mtime 语义 | 提供 `accurate_modified_time` 选项（默认关闭），开启后 List 时附加一次 GraphQL 批量查询取每个路径的最后 commit 时间；关闭时返回零时间。`SetModTime` 返回 `fs.ErrorCantSetModTime` |
| 下载通道 | 提供 `download_via = raw | api` 选项（默认 `raw`）。raw 走 `download_url` 直链；api 走 contents 端点 + `Accept: application/vnd.github.raw` + Authorization 头，用于私有仓库和避免 raw CDN 缓存导致的写后读旧内容 |
| 空目录 | 沿用 OpenList 的 `.gitkeep` 占位策略：Mkdir 创建含 `.gitkeep` 的 tree；Remove 删空目录最后一条目时补 `.gitkeep`；List 时过滤 `.gitkeep` |
| GPG 签名 | v1 不做，配置结构不预留字段 |
| commit message | 沿用 text/template 模板方案，每种操作一个选项，默认值与 OpenList 一致（去掉 UserName 变量，rclone 无用户概念，固定为 `rclone`） |
| 并发写 | 全局 commit 互斥锁串行化"tree → commit → update ref"序列；ref 更新遇 non-fast-forward 时重新拉取 head 重试 |
| 只读 ref | `ref` 为 tag 或 commit SHA 时（非分支），所有写操作返回明确错误 |
| submodule | List 正常展示为条目，下载/移动/删除 submodule 返回明确错误 |

## 3. 配置项（Options）

| 选项 | 必填 | 说明 |
|---|---|---|
| `token` | 是（Sensitive） | GitHub personal access token，需 repo 权限 |
| `owner` | 是 | 仓库所有者 |
| `repo` | 是 | 仓库名 |
| `ref` | 否 | 分支、tag 或 commit SHA，默认主分支（NewFs 时通过 repo API 探测 default_branch） |
| `download_via` | 否 | `raw`（默认）/ `api` |
| `gh_proxy` | 否 | raw 域名替换，如 `https://gh-proxy.com/raw.githubusercontent.com` |
| `accurate_modified_time` | 否 | bool，默认 false |
| `committer_name` / `committer_email` | 否 | 需成对出现 |
| `author_name` / `author_email` | 否 | 需成对出现 |
| `mkdir_commit_message` 等 6 个模板 | 否 | text/template，默认值如 `rclone upload {{.ObjPath}}` |

认证实现照搬 webdav 的 bearer 模式：`srv.SetHeader("Authorization", "Bearer "+token)`，client 级统一带 `Accept: application/vnd.github+json` 与 `X-GitHub-Api-Version: 2022-11-28`。

## 4. 实现要点

### 4.1 文件结构

```
backend/github/
├── github.go        init/fs.Register、Options、NewFs、Fs 与 Object 全部实现（不拆 fs.go/object.go）
├── api/types.go     GitHub API 的请求/响应结构（对应 OpenList types.go 的内容）
└── github_test.go   fstests 标准测试 + 单元测试
```

### 4.2 List

`GET /repos/{o}/{r}/contents{path}?ref={ref}`，目录返回 entries 数组。当 entries 达到 1000 条上限时回退 `GET /git/trees/{sha}`（GitHub contents API 对目录最多返回 1000 条），tree 截断（truncated）则报错。开启 `accurate_modified_time` 且条目数 ≤200 时，List 后追加一次 GraphQL 请求批量查询 `history(first: 1, path:)` 的 `committedDate`；失败降级为零时间，不阻断 List。

### 4.3 下载（Object.Open）

- `raw` 模式：List/get 时拿到的 `download_url`（可选经 `gh_proxy` 替换域名），`http.NewRequestWithContext` + `fs.FixRangeOption` + `fs.OpenOptionAddHTTPHeaders`（Range 直传上游），模板照 pikpak 的 `httpResponse` 写法。raw 支持 Range 请求。
- `api` 模式：`GET /contents/{path}?ref=` + `Accept: application/vnd.github.raw`，Authorization 头由 client 携带。该模式同样支持 Range。
- 上游 SHA 与本地记录不一致（分支被外部推进）时按重试策略刷新元数据。

### 4.4 上传（Put / Update）

1. 入口先检查 `src.Size() > 100MiB`，超限直接返回错误（错误信息写明 Git Blob API 上限）。
2. `POST /git/blobs`：请求体为 `{"encoding":"base64","content":"..."}`，用 `io.Pipe` + `base64.NewEncoder` 流式拼接前后缀，全程不落盘、不整读内存（照搬 OpenList `putBlob` 的思路，rclone 侧注意接入 `fs.Accounting` 统计与限速）。
3. 取父目录当前 tree，`POST /git/trees`（`base_tree` = 父 tree SHA，追加新 blob 条目；若父目录仅有 `.gitkeep` 则一并删除占位）。
4. `renewParentTrees`：自底向上重建祖先 tree 直到根。
5. `POST /git/commits` + `PATCH /git/refs/heads/{ref}`，遇 422（non-fast-forward）重新拉取 branch head 重试一次。

以上 3-5 步在 commit 互斥锁内执行。

### 4.5 Move / DirMove / Copy / Remove / Mkdir / Rmdir

全部是纯 tree 操作 + commit，逻辑参照 OpenList 驱动的 Move（三种路径关系：目标在源父目录下、源在目标目录下、无公共包含关系）、Rename、Copy（`copyWithoutRenewTree` + 祖先链重建）、Remove（删空补 `.gitkeep`）、MakeDir（`.gitkeep` 占位 tree）。这部分是移植工作量最大的部分，公共祖先计算 `getPathCommonAncestor` 直接按 OpenList 算法重写。

rclone 映射：Fs 实现 `Move`/`DirMove`/`Copy`（开启对应 Feature），`Remove` 用于文件删除，`Rmdir` 删除目录（递归检查为空后删 tree 条目）。

### 4.6 gitsha1 hash

在 `init()` 中调用 `fs/hash.RegisterHash("gitsha1", "GitSHA1", 40, newGitBlobHash)`，newFunc 返回的 hash.Hash 在 Write 前先写入 `blob <size>\x00` 头——由于头部依赖 size，实现为自定义 hash.Hash 结构，`Sum` 时按 git 规则计算；width 40 为 SHA-1 hex 长度。Object 的 `Hash(ctx, hash.Type)` 对 `gitsha1` 返回 contents API 给出的 `sha` 字段。需验证 `RegisterHash` 对已注册名的幂等性，重复注册（测试多次 import）要防 panic。

### 4.7 Features

- `Move`、`DirMove`、`Copy`：开（服务端操作）。
- `SetModTime` 不支持；`Precision` 按 GitHub commit 时间精度（秒级）。
- `Hashes` 集合含 `gitsha1`。
- `ListR`：可用 `GET /git/trees/{sha}?recursive=1` 实现递归列举（上限 10 万条目/7MB 响应，截断则回退逐目录 List），提升大仓库同步效率。作为 v1 的可选项，若实现复杂度可控则一并做。

### 4.8 错误处理与限流

实现 `shouldRetry`：HTTP 429、5xx、以及 GitHub 返回 `X-RateLimit-Remaining: 0` 的 403 走 pacer 退避；403 且响应体含 rate limit 字段时按 `X-RateLimit-Reset` 时间等待。参照现有 backend（pikpak/seafile）的 pacer 用法。

## 5. 改动触点清单

1. `backend/github/`（新增，三个文件）。
2. `backend/all/all.go`：按字母序在 `filelu`/`filescom` 附近插入 blank import。
3. `docs/data/backends/github.yaml`：新增（缺失会导致 `fs.Register` 启动报 internal error）。
4. `docs/content/github.md`：backend 文档页。
5. `README.md`、`docs/content/docs.md`、`docs/content/_index.md`：按字母序追加条目。
6. `fstest/test_all/config.yaml`：追加 `TestGithub` 注册段。

## 6. 测试与验收计划

### 6.1 原则

- **禁止 mock**：不 mock GitHub API、不 mock HTTP 层。凡涉及服务端行为的验证一律打真实 GitHub。
- **只允许逻辑单测**：纯算法/纯函数不触网，走 Go 标准单测。
- **真实凭证由 Xyzen 提供**：测试用的 GitHub token 与专用测试仓库由 Xyzen 提供，agent 不自行创建仓库或申请凭证。

### 6.2 逻辑单测（不触网）

- `getPathCommonAncestor` 三种路径关系与边界。
- `.gitkeep` 过滤与空目录占位逻辑（纯数据结构层面）。
- base64 流式长度计算 `calculateBase64Length` 与 `io.Pipe` 拼接体前缀/后缀的正确性（对拍 `base64.StdEncoding` 整读结果）。
- `gitsha1` hash 与 `git hash-object` 输出对拍（本地构造已知内容，git 命令行作为参照，不触网）。
- commit message 模板渲染（含变量缺失/模板语法错误的降级路径）。

### 6.3 验收方式（真实凭证，Xyzen 提供后执行）

Xyzen 提供：一个 GitHub PAT（repo 权限）+ 一个**专用测试仓库**（fstests 会真实写入产生大量 commit，必须用独立仓库）。凭证通过环境变量或临时 rclone config 传入，**不入库、不写进任何文件**。

1. **fstests 标准套件**：配置 `TestGithub` remote 后执行 `go test -v ./backend/github/`，跑完整集成测试（List/Put/Open/Move/Copy/Remove/Mkdir/Rmdir/hash 校验）。
2. **人工命令冒烟**（编译产物 `make` 后直接跑二进制，更贴近真实用法）：
   - `rclone lsl TestGithub:` / `rclone lsf --recursive` 列举
   - `rclone copy` 本地小文件 → 仓库，GitHub 页面确认 commit 产生、文件内容正确
   - `rclone copy` 仓库 → 本地，diff 校验内容一致
   - `rclone move` / `rclone copyto`（rename 语义）/ `rclone delete` / `rclone mkdir` / `rclone rmdir` 各操作后核对 commit 历史与 `.gitkeep` 行为
   - 上传 >100MB 文件，确认被拒绝且仓库无残留 commit
   - `rclone check --checksum`（gitsha1 校验路径）
   - `download_via=api` 模式重复下载冒烟
   - 并发上传（`--transfers=8`）验证 commit 互斥锁下无 non-fast-forward 失败
3. **无凭据回归**：`RCLONE_CONFIG="/notfound" go test ./...` 确认不影响其他包。
4. `golangci-lint run ./backend/github/` 与 CI 一致。

验收触发时机：M1 完成后先做只读冒烟（lsl/cat/check 下载方向）；M2、M3 完成后做写方向与完整 fstests；M4 后整体复验。每次验收前向 Xyzen 汇报测试范围，待批准后执行。

## 7. 里程碑

1. M1 骨架与只读路径：注册、Options、NewFs、List、NewObject、Open（raw/api 双通道）、hash、文档元数据 yaml。可先通过 `rclone lsl/lsf/cat` 人工验证。
2. M2 写路径：putBlob、tree/commit 序列、Put、Update、Mkdir、Remove、Rmdir、100MB 限制、commit 互斥锁与 ref 冲突重试。
3. M3 服务端操作：Move、DirMove、Copy、Rename 语义。
4. M4 打磨：mtime GraphQL 开关、ListR、限流退避完善、文档与 fstest 注册。

每个里程碑完成后编译通过（`make`）并做定向测试，不做全量测试（除非后续另行批准）。

### 实施状态（2026-10-03）

M1-M4 的代码已一次性完成并合入 `backend/github/`（实现过程中发现各阶段耦合紧密，拆阶段交付反而引入中间态缺陷，故整体实现后统一验证）。当前状态：

- 编译、`go vet`、`golangci-lint`、逻辑单测全部通过。
- 触点全部完成：`backend/all/all.go`、`docs/data/backends/github.yaml`、`docs/content/github.md`、`README.md`、`docs/content/docs.md`、`docs/content/_index.md`、`fstest/test_all/config.yaml`。
- 与原计划的两处有意偏差：
  1. tree 变更不再用 OpenList 的三段式路径分析（`getPathCommonAncestor` + `renewParentTrees`），改为通用的“变更集合自底向上重建”算法（`applyEdits`）：收集受影响目录的 tree 变更，每次重建最深的目录并把新 sha 向父目录传递。该算法正确覆盖了 OpenList 会 panic 的嵌套路径场景（如 `aa/1 -> aa/bb/1`），且天然支持一次 commit 内合并多处变更。
  2. ref 冲突（non-fast-forward 422）不自动用旧 tree 重提交，而是返回可重试错误让上层基于新状态整体重试，避免静默覆盖外部并发提交（OpenList 存在此风险）。
- 待办：等待 Xyzen 提供真实凭证后执行 6.3 节验收（fstests + 人工命令冒烟）。

### 验收结果（2026-10-04，真实凭证）

Xyzen 提供了真实凭证（PAT + 专用测试仓库 dev-xizhen55555/public，分支 main），完成全部验收：

- **fstests 完整套件通过**：`TestIntegration` 全部子测试 PASS（约 372 秒），包括全部 FsEncoding 编码用例、Put/Move/Copy/DirMove/Remove/Rmdir、ListR、哈希校验、写后读一致性等。
- **人工命令冒烟全部通过**：lsl/lsf/cat（含 --head Range）、copy 双向往返内容一致、mkdir/rmdir/.gitkeep 行为、moveto/rename/move 目录/copyto/delete/purge、100MiB 拒绝（错误信息清晰且不发请求）、raw/api 双下载通道内容一致、--transfers=8 并发上传 commit 互斥锁无冲突。
- **gitsha1 与 git hash-object 对拍一致**。
- 逻辑单测、golangci-lint、全仓编译、无凭据定向回归全部通过。

验收过程中发现并修复的问题（均已验证）：

1. `NewFs` 的 root 文件检查误用 `gitPath(f.root)` 导致双重路径拼接，且提前 return 跳过了 Features 初始化。
2. rclone 不保证先 Mkdir 再 Put（单文件 copy 直达不存在路径）：Update/Mkdir/Move/Copy 现在都会用 `findDeepestExistingDir` + `placeEntryEdits` 在同一个 commit 内补建缺失的父目录链。
3. `Mkdir("")`/`Rmdir("")` 的 Fs 根语义：`rclone mkdir remote:dir` 会以 dir 为 Fs root 调 `Mkdir("")`，原先直接 no-op 导致目录未创建。
4. trees API 的序列化约束：content 条目必须完全省略 sha 字段，删除条目必须显式 `"sha": null`（`TreeEntry` 实现了条件 MarshalJSON）；空字符串 content 必须用指针避免被 omitempty 吞掉。
5. 编码集必须含 `EncodeDot`：encoder 的 Standard 层把孤立 `.`/`..` 表示为全角点，不加 EncodeDot 会在 FromStandard 时还原成 git 非法的 ASCII 点组件。
6. `applyEdits` 的最深优先选择在同深度时依赖 map 随机迭代顺序，根编辑可能先于同深度的顶层目录变更被处理并提前 return，静默丢弃变更（约 50% 概率失败，FsDirMove 暴露）。修复为根目录永远最后处理。
7. raw.githubusercontent.com 的 CDN 会缓存分支路径内容，写后立即读返回旧数据（fstests ObjectUpdate 暴露）。修复为 raw 下载 URL 一律用分支头 commit sha 锁定（不可变、不受缓存影响），头 sha 带 TTL 缓存（5 分钟）且本进程提交后立即刷新。
8. modtime：git 不存储 mtime，零时间无法通过 fstests 的 100 年精度检查。`accurate_modified_time` 默认改为开启（List/ListR/NewObject 各一次 GraphQL 批量查询，GraphQL 配额与核心 REST 配额独立），Update 后用当前时间近似 commit 时间。
9. Move/Copy/DirMove 的路径语义：src 路径用源 Fs 的 root 计算、dst 用目标 Fs 的 root 计算（两者可能是同仓库不同实例）；DirMove 按接口契约在目标已存在时返回 `ErrorDirExists`；Move/Copy 自动创建缺失的目标目录链。

注意：fstests 全套约消耗 2000 次核心 REST 请求，连续多轮运行会耗尽 5000/hr 配额（表现为 403 + X-RateLimit-Reset 等待），单轮完整验收在配额内。
