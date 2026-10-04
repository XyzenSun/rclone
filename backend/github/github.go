// Package github provides an interface to GitHub repositories.
//
// A repository at a given ref (branch, tag or commit SHA) is exposed
// as a directory tree. Reads go through the REST contents API and
// raw.githubusercontent.com; writes are performed with the Git Data
// API (blobs -> trees -> commits -> refs), so every write operation
// produces a commit. Moving, renaming and copying entries are pure
// tree operations and never transfer file content.
package github

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/rclone/rclone/backend/github/api"
	"github.com/rclone/rclone/backend/github/gitsha1"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/encoder"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/readers"
	"github.com/rclone/rclone/lib/rest"
)

const (
	apiBase         = "https://api.github.com"
	rawBase         = "https://raw.githubusercontent.com"
	graphqlEndpoint = "https://api.github.com/graphql"

	// Git Blob API 单 blob 上限,上传前直接拒绝超限文件
	maxBlobSize = 100 * 1024 * 1024

	// contents API 单目录最多返回 1000 条,达到上限时回退 git trees API
	contentsMaxEntries = 1000

	// GraphQL 批量查询 mtime 的条目上限,超过则跳过精确时间
	mtimeMaxEntries = 200

	gitkeepName = ".gitkeep"
	blobMode    = "100644"
	treeMode    = "040000"

	retryAfterHeader     = "Retry-After"
	rateLimitResetHeader = "X-RateLimit-Reset"
	rateLimitUsedHeader  = "X-RateLimit-Remaining"

	minSleep      = 10 * time.Millisecond
	maxSleep      = 5 * time.Second
	decayConstant = 2

	// rateLimitWaitCap 限流等待的上限,避免 pacer 长时间阻塞
	rateLimitWaitCap = time.Minute
)

// gitSHA1Type 是注册进 rclone 全局 hash 表的 git blob sha 类型,
// 与 GitHub contents/trees API 返回的 sha 字段同源,可用于 --checksum 校验
var gitSHA1Type = hash.RegisterHash("gitsha1", "GitSHA1", 40, gitsha1.New)

var (
	retryErrorCodes = []int{429, 500, 502, 503, 504}

	// errBranchNotFound 表示分支引用不存在(空仓库或 ref 不是分支)
	errBranchNotFound = errors.New("branch ref not found")
	// errTreeTruncated 表示 git trees API 返回被截断
	errTreeTruncated = errors.New("git tree listing was truncated by the GitHub API")
	// errSubmodule 表示操作目标是一个 git submodule
	errSubmodule = errors.New("git submodules are not supported")
)

// Options 定义本 backend 的配置项
type Options struct {
	Token                string               `config:"token"`
	Owner                string               `config:"owner"`
	Repo                 string               `config:"repo"`
	Ref                  string               `config:"ref"`
	DownloadVia          string               `config:"download_via"`
	GhProxy              string               `config:"gh_proxy"`
	AccurateModifiedTime bool                 `config:"accurate_modified_time"`
	CommitterName        string               `config:"committer_name"`
	CommitterEmail       string               `config:"committer_email"`
	AuthorName           string               `config:"author_name"`
	AuthorEmail          string               `config:"author_email"`
	MkdirCommitMessage   string               `config:"mkdir_commit_message"`
	DeleteCommitMessage  string               `config:"delete_commit_message"`
	PutCommitMessage     string               `config:"put_commit_message"`
	RenameCommitMessage  string               `config:"rename_commit_message"`
	CopyCommitMessage    string               `config:"copy_commit_message"`
	MoveCommitMessage    string               `config:"move_commit_message"`
	Enc                  encoder.MultiEncoder `config:"encoding"`
}

// init 注册 backend
func init() {
	fs.Register(&fs.RegInfo{
		Name:        "github",
		Description: "GitHub (repositories as filesystems, via Git Data API)",
		NewFs:       NewFs,
		Options: []fs.Option{{
			Name:      "token",
			Help:      "GitHub personal access token with repo scope.",
			Required:  true,
			Sensitive: true,
		}, {
			Name:     "owner",
			Help:     "Owner of the repository (user or organization).",
			Required: true,
		}, {
			Name:     "repo",
			Help:     "Name of the repository.",
			Required: true,
		}, {
			Name: "ref",
			Help: "A branch, a tag or a commit SHA. Leave blank for the default branch.\n\n" +
				"Write operations require a branch. On an empty repository leave this blank\n" +
				"so the default branch is used and the first commit can be created.",
		}, {
			Name:    "download_via",
			Help:    "How files are downloaded.",
			Default: "raw",
			Examples: []fs.OptionExample{{
				Value: "raw",
				Help:  "Direct link from raw.githubusercontent.com (fast, CDN cached).",
			}, {
				Value: "api",
				Help:  "Contents API raw media type (always fresh, needed right after writes).",
			}},
		}, {
			Name:     "gh_proxy",
			Help:     "Replacement for https://raw.githubusercontent.com in download links, e.g. https://gh-proxy.com/raw.githubusercontent.com.\n\nWhen set, no Authorization header is sent to the proxy, so it only works for public repositories.",
			Advanced: true,
		}, {
			Name:     "accurate_modified_time",
			Help:     "Fetch the last commit time of each file via the GraphQL API during listings.\n\nThis is the only source of modification times, since git does not store them.\nAdds one request per listed directory (up to 200 entries) and falls back to zero\ntime on failure. Turn off for maximum listing speed if you don't need mtimes.",
			Default:  true,
			Advanced: true,
		}, {
			Name:     "committer_name",
			Help:     "Committer name. Must be set together with committer_email.",
			Advanced: true,
		}, {
			Name:     "committer_email",
			Help:     "Committer email. Must be set together with committer_name.",
			Advanced: true,
		}, {
			Name:     "author_name",
			Help:     "Author name. Must be set together with author_email.",
			Advanced: true,
		}, {
			Name:     "author_email",
			Help:     "Author email. Must be set together with author_name.",
			Advanced: true,
		}, {
			Name:     "mkdir_commit_message",
			Help:     "Commit message template for mkdir. Variables: ObjName, ObjPath, ParentName, ParentPath.",
			Default:  "rclone mkdir {{.ObjPath}}",
			Advanced: true,
		}, {
			Name:     "delete_commit_message",
			Help:     "Commit message template for delete. Variables: ObjName, ObjPath, ParentName, ParentPath.",
			Default:  "rclone delete {{.ObjPath}}",
			Advanced: true,
		}, {
			Name:     "put_commit_message",
			Help:     "Commit message template for upload. Variables: ObjName, ObjPath, ParentName, ParentPath.",
			Default:  "rclone upload {{.ObjPath}}",
			Advanced: true,
		}, {
			Name:     "rename_commit_message",
			Help:     "Commit message template for rename. Variables: ObjName, ObjPath, ParentName, ParentPath, TargetName, TargetPath.",
			Default:  "rclone rename {{.ObjPath}} to {{.TargetName}}",
			Advanced: true,
		}, {
			Name:     "copy_commit_message",
			Help:     "Commit message template for copy. Variables: ObjName, ObjPath, ParentName, ParentPath, TargetName, TargetPath.",
			Default:  "rclone copy {{.ObjPath}} to {{.TargetPath}}",
			Advanced: true,
		}, {
			Name:     "move_commit_message",
			Help:     "Commit message template for move. Variables: ObjName, ObjPath, ParentName, ParentPath, TargetName, TargetPath.",
			Default:  "rclone move {{.ObjPath}} to {{.TargetPath}}",
			Advanced: true,
		}, {
			Name:     config.ConfigEncoding,
			Help:     config.ConfigEncodingHelp,
			Advanced: true,
			Default: (encoder.EncodeZero |
				encoder.EncodeCtl |
				encoder.EncodeSlash |
				encoder.EncodeBackSlash |
				encoder.EncodeDoubleQuote |
				encoder.EncodeInvalidUtf8 |
				// git 禁止路径组件为孤立的 "." 或 "..",必须编码;
				// encoder 的 Standard 层会把这类名字表示为全角点,
				// 不加 EncodeDot 会在 FromStandard 时还原成非法的 ASCII 点
				encoder.EncodeDot),
		}},
	})
}

// Fs 代表一个 GitHub 仓库的某个 ref 下的某个子路径
type Fs struct {
	name       string       // remote 名称
	root       string       // 仓库内根路径(标准形式, 无前导斜杠)
	opt        Options      // 解析后的配置
	features   *fs.Features // 可选特性
	srv        *rest.Client // api.github.com JSON 客户端(带认证头)
	rawSrv     *rest.Client // 裸客户端, 用于 raw/api 两种下载通道
	pacer      *fs.Pacer    // API 限速与重试
	commitMu   sync.Mutex   // 串行化所有写操作的 tree -> commit -> ref 序列
	isOnBranch bool         // ref 是否为可写分支
	headMu     sync.Mutex   // 保护 headSha/headTime
	headSha    string       // 分支头 commit sha,用于锁定 raw 下载 URL
	headTime   time.Time    // headSha 的刷新时间
}

// NewFs 构造 Fs
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	opt := new(Options)
	if err := configstruct.Set(m, opt); err != nil {
		return nil, err
	}
	if (opt.CommitterName == "") != (opt.CommitterEmail == "") {
		return nil, errors.New("committer_name and committer_email must be set together")
	}
	if (opt.AuthorName == "") != (opt.AuthorEmail == "") {
		return nil, errors.New("author_name and author_email must be set together")
	}
	switch opt.DownloadVia {
	case "", "raw", "api":
		if opt.DownloadVia == "" {
			opt.DownloadVia = "raw"
		}
	default:
		return nil, fmt.Errorf("invalid download_via %q: must be \"raw\" or \"api\"", opt.DownloadVia)
	}
	// 提前校验 commit message 模板,配置错误在 NewFs 阶段暴露
	for op, tmpl := range map[string]string{
		"mkdir":  opt.MkdirCommitMessage,
		"delete": opt.DeleteCommitMessage,
		"put":    opt.PutCommitMessage,
		"rename": opt.RenameCommitMessage,
		"copy":   opt.CopyCommitMessage,
		"move":   opt.MoveCommitMessage,
	} {
		if _, err := commitMessage(tmpl, op, &messageTemplateVars{ObjPath: "check"}); err != nil {
			return nil, err
		}
	}

	httpClient := fshttp.NewClient(ctx)
	srv := rest.NewClient(httpClient).
		SetRoot(apiBase).
		SetHeader("Authorization", "Bearer "+opt.Token).
		SetHeader("X-GitHub-Api-Version", "2022-11-28").
		SetErrorHandler(parseGitHubError)
	rawSrv := rest.NewClient(httpClient)

	f := &Fs{
		name:   name,
		root:   strings.Trim(root, "/"),
		opt:    *opt,
		srv:    srv,
		rawSrv: rawSrv,
		pacer:  getPacer(ctx),
	}

	// 解析 ref:留空则取默认分支;显式 ref 先按分支探测,失败再按 tag/SHA 校验
	if f.opt.Ref == "" {
		repo, err := f.getRepo(ctx)
		if err != nil {
			return nil, err
		}
		f.opt.Ref = repo.DefaultBranch
		f.isOnBranch = true
	} else if _, err := f.getBranchHead(ctx); err == nil {
		f.isOnBranch = true
	} else if !errors.Is(err, errBranchNotFound) {
		return nil, err
	} else if err := f.checkRefResolvable(ctx); err != nil {
		return nil, err
	}

	// root 指向文件时,把 root 收缩到其父目录并按 rclone 约定返回 ErrorIsFile。
	// 注意这里直接用编码后的 root 本身,不能用 gitPath(它会把 root 再拼一遍)
	rootIsFile := false
	if f.root != "" {
		entry, err := f.getContents(ctx, f.opt.Enc.FromStandardPath(f.root))
		if err == nil {
			if entry.IsFile() {
				f.root = dirPath(f.root)
				rootIsFile = true
			}
		} else if !errors.Is(err, fs.ErrorObjectNotFound) {
			return nil, err
		}
	}

	f.features = (&fs.Features{
		CanHaveEmptyDirectories: true,
	}).Fill(ctx, f)
	if rootIsFile {
		return f, fs.ErrorIsFile
	}
	return f, nil
}

// ------------------------------------------------------------ 内部工具

// getPacer 构造本 remote 的 pacer,负责限流退避与低层重试
func getPacer(ctx context.Context) *fs.Pacer {
	return fs.NewPacer(ctx, pacer.NewDefault(
		pacer.MinSleep(minSleep),
		pacer.MaxSleep(maxSleep),
		pacer.DecayConstant(decayConstant),
	))
}

// parseGitHubError 把非 2xx 响应解析成可读错误,作为 rest 客户端的错误处理器
func parseGitHubError(resp *http.Response) error {
	if resp.Body == nil {
		return fmt.Errorf("github api error %s", resp.Status)
	}
	body, err := rest.ReadBody(resp)
	if err != nil {
		return fmt.Errorf("github api error %s: failed to read body: %w", resp.Status, err)
	}
	apiErr := &api.ErrorResponse{}
	if json.Unmarshal(body, apiErr) == nil && apiErr.Message != "" {
		return fmt.Errorf("github api error %s: %s", resp.Status, apiErr.Message)
	}
	return fmt.Errorf("github api error %s: %q", resp.Status, string(body))
}

// shouldRetry 判断响应是否值得重试,并处理 GitHub 的限流语义:
// 429 与 403(配额耗尽)都按 X-RateLimit-Reset 等待
func (f *Fs) shouldRetry(ctx context.Context, resp *http.Response, err error) (bool, error) {
	if fserrors.ContextError(ctx, &err) {
		return false, err
	}
	if resp != nil {
		rateLimited := resp.StatusCode == 429 ||
			(resp.StatusCode == 403 && resp.Header.Get(rateLimitUsedHeader) == "0")
		if rateLimited {
			// 主限流返回 403 且带 X-RateLimit-Reset;次级限流返回 429 且带 Retry-After
			wait := time.Second
			if reset := resp.Header.Get(rateLimitResetHeader); reset != "" {
				if unix, parseErr := strconv.ParseInt(reset, 10, 64); parseErr == nil {
					if untilReset := time.Until(time.Unix(unix, 0)) + time.Second; untilReset > wait {
						wait = untilReset
					}
				}
			} else if ra := resp.Header.Get(retryAfterHeader); ra != "" {
				if secs, parseErr := strconv.Atoi(ra); parseErr == nil {
					wait = time.Duration(secs) * time.Second
				}
			}
			// 等待时间设上限,超出部分交给上层重试机制
			if wait > rateLimitWaitCap {
				wait = rateLimitWaitCap
			}
			return true, pacer.RetryAfterError(err, wait)
		}
	}
	return fserrors.ShouldRetry(err) || fserrors.ShouldRetryHTTP(resp, retryErrorCodes), err
}

// callJSON 发起 JSON API 请求并在成功时解码响应。出错时 resp 也尽量返回,
// 调用方可以依据 StatusCode 做语义映射(如 404 -> fs.ErrorObjectNotFound)
func (f *Fs) callJSON(ctx context.Context, opts *rest.Opts, request, result any) (*http.Response, error) {
	var resp *http.Response
	var err error
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(ctx, opts, request, result)
		return f.shouldRetry(ctx, resp, err)
	})
	return resp, err
}

// ------------------------------------------------------------ 路径工具

// gitPath 把 rclone remote 路径转换为仓库内路径(已做编码, 无前导斜杠)
func (f *Fs) gitPath(remote string) string {
	return f.opt.Enc.FromStandardPath(path.Join(f.root, remote))
}

// dirPath 返回路径的父目录,根目录返回空串
func dirPath(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}

// leafName 返回路径的最后一段
func leafName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// escapePath 对斜杠分隔路径的每一段做 URL 转义,用于拼接 API 路径
func escapePath(p string) string {
	segments := strings.Split(p, "/")
	for i := range segments {
		segments[i] = url.PathEscape(segments[i])
	}
	return strings.Join(segments, "/")
}

// contentsPath 返回 contents API 的请求路径
func (f *Fs) contentsPath(gitPath string) string {
	return "/repos/" + f.opt.Owner + "/" + f.opt.Repo + "/contents/" + escapePath(gitPath)
}

// ------------------------------------------------------------ API 封装

// getRepo 获取仓库信息
func (f *Fs) getRepo(ctx context.Context) (*api.RepoResponse, error) {
	opts := &rest.Opts{Method: "GET", Path: "/repos/" + f.opt.Owner + "/" + f.opt.Repo}
	result := &api.RepoResponse{}
	if _, err := f.callJSON(ctx, opts, nil, result); err != nil {
		return nil, fmt.Errorf("failed to get repository info: %w", err)
	}
	return result, nil
}

// checkRefResolvable 校验 ref 能否解析为 tag 或 commit SHA
func (f *Fs) checkRefResolvable(ctx context.Context) error {
	opts := &rest.Opts{Method: "GET", Path: "/repos/" + f.opt.Owner + "/" + f.opt.Repo + "/commits/" + escapePath(f.opt.Ref)}
	resp, err := f.callJSON(ctx, opts, nil, &api.CommitResponse{})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("ref %q not found (if the repository is empty, leave ref blank to use the default branch)", f.opt.Ref)
		}
		return err
	}
	return nil
}

// getContents 调用 contents API 获取单个文件或目录(带 entries)。
// 404 映射为 fs.ErrorObjectNotFound
func (f *Fs) getContents(ctx context.Context, gitPath string) (*api.Entry, error) {
	opts := &rest.Opts{
		Method:       "GET",
		Path:         f.contentsPath(gitPath),
		Parameters:   url.Values{"ref": {f.opt.Ref}},
		ExtraHeaders: map[string]string{"Accept": "application/vnd.github.object+json"},
	}
	result := &api.Entry{}
	resp, err := f.callJSON(ctx, opts, nil, result)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return nil, fs.ErrorObjectNotFound
		}
		return nil, fmt.Errorf("failed to get contents of %q: %w", gitPath, err)
	}
	return result, nil
}

// getTree 按 sha 获取 git tree,recursive 为 true 时递归
func (f *Fs) getTree(ctx context.Context, sha string, recursive bool) (*api.TreeResponse, error) {
	opts := &rest.Opts{
		Method: "GET",
		Path:   "/repos/" + f.opt.Owner + "/" + f.opt.Repo + "/git/trees/" + escapePath(sha),
	}
	if recursive {
		opts.Parameters = url.Values{"recursive": {"1"}}
	}
	result := &api.TreeResponse{}
	resp, err := f.callJSON(ctx, opts, nil, result)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return nil, fs.ErrorObjectNotFound
		}
		return nil, fmt.Errorf("failed to get tree %s: %w", sha, err)
	}
	return result, nil
}

// getTreeByPath 获取路径指向目录的 tree 及其 sha
func (f *Fs) getTreeByPath(ctx context.Context, gitPath string) (*api.TreeResponse, string, error) {
	entry, err := f.getContents(ctx, gitPath)
	if err != nil {
		return nil, "", err
	}
	if entry.Entries == nil {
		return nil, "", fs.ErrorDirNotFound
	}
	tree, err := f.getTree(ctx, entry.Sha, false)
	if err != nil {
		return nil, "", err
	}
	if tree.Truncated {
		return nil, "", fmt.Errorf("%w: %s", errTreeTruncated, gitPath)
	}
	return tree, entry.Sha, nil
}

// findTreeEntry 在 tree 中按名字查找条目
func findTreeEntry(tree *api.TreeResponse, name string) *api.TreeEntry {
	for i := range tree.Tree {
		if tree.Tree[i].Path == name {
			return &tree.Tree[i]
		}
	}
	return nil
}

// shaString 把 TreeEntry 的 interface{} sha 安全转换为字符串
func shaString(sha any) string {
	if s, ok := sha.(string); ok {
		return s
	}
	return ""
}

// createTree 创建 git tree。baseSha 为空表示从零创建(用于空仓库或 .gitkeep 占位)
func (f *Fs) createTree(ctx context.Context, baseSha string, entries []api.TreeEntry) (string, error) {
	req := &api.TreeRequest{Tree: entries, BaseTree: baseSha}
	result := &api.TreeResponse{}
	opts := &rest.Opts{Method: "POST", Path: "/repos/" + f.opt.Owner + "/" + f.opt.Repo + "/git/trees"}
	if _, err := f.callJSON(ctx, opts, req, result); err != nil {
		return "", fmt.Errorf("failed to create tree: %w", err)
	}
	return result.Sha, nil
}

// gitkeepEntry 返回创建 .gitkeep 占位 blob 的 tree 条目。
// Content 必须是指向空串的指针:omitempty 会吞掉空字符串,
// 导致条目既无 sha 也无 content 而被 GitHub 拒绝
func gitkeepEntry() api.TreeEntry {
	return api.TreeEntry{Path: gitkeepName, Mode: blobMode, Type: "blob", Content: &emptyString}
}

var emptyString string

// getBranchHead 获取分支头 commit sha,分支不存在时返回 errBranchNotFound
func (f *Fs) getBranchHead(ctx context.Context) (string, error) {
	opts := &rest.Opts{Method: "GET", Path: "/repos/" + f.opt.Owner + "/" + f.opt.Repo + "/branches/" + escapePath(f.opt.Ref)}
	result := &api.BranchResponse{}
	resp, err := f.callJSON(ctx, opts, nil, result)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return "", errBranchNotFound
		}
		return "", fmt.Errorf("failed to get branch %q: %w", f.opt.Ref, err)
	}
	return result.Commit.Sha, nil
}

// headCacheTTL 限制 headSha 刷新频率,平衡外部并发提交的可见性与 API 开销
const headCacheTTL = 5 * time.Minute

// headCommit 返回分支头 commit sha,带 TTL 缓存。
// raw.githubusercontent.com 的 CDN 会缓存分支路径的内容,写后立即读会拿到旧数据;
// 用 commit sha 锁定的 raw URL 是不可变的,不受缓存影响。本进程提交后立即刷新,
// 外部提交量多时最多滞后一个 TTL
func (f *Fs) headCommit(ctx context.Context) (string, error) {
	f.headMu.Lock()
	defer f.headMu.Unlock()
	if f.headSha != "" && time.Since(f.headTime) < headCacheTTL {
		return f.headSha, nil
	}
	sha, err := f.getBranchHead(ctx)
	if err != nil {
		// 刷新失败时退回旧值,避免外部抖动影响下载
		if f.headSha != "" {
			return f.headSha, nil
		}
		return "", err
	}
	f.headSha, f.headTime = sha, time.Now()
	return sha, nil
}

// setHeadCommit 在本进程成功推进分支后立即刷新缓存的头 sha
func (f *Fs) setHeadCommit(sha string) {
	f.headMu.Lock()
	f.headSha, f.headTime = sha, time.Now()
	f.headMu.Unlock()
}

// commitTree 用新的根 tree sha 创建 commit 并推进分支引用。
// 空仓库(无任何 commit)时创建首个 commit 与分支引用;
// 分支被外部并发推进导致 422 时返回可重试错误,由上层整体重试
func (f *Fs) commitTree(ctx context.Context, message, rootSha string) error {
	head, err := f.getBranchHead(ctx)
	if err != nil && !errors.Is(err, errBranchNotFound) {
		return err
	}
	var parents []string
	if head != "" {
		parents = []string{head}
	}

	req := &api.CommitRequest{
		Message: message,
		Tree:    rootSha,
		Parents: parents,
	}
	if f.opt.CommitterName != "" {
		req.Committer = &api.Committer{Name: f.opt.CommitterName, Email: f.opt.CommitterEmail}
	}
	if f.opt.AuthorName != "" {
		req.Author = &api.Committer{Name: f.opt.AuthorName, Email: f.opt.AuthorEmail}
	}
	commit := &api.CommitResponse{}
	opts := &rest.Opts{Method: "POST", Path: "/repos/" + f.opt.Owner + "/" + f.opt.Repo + "/git/commits"}
	if _, err := f.callJSON(ctx, opts, req, commit); err != nil {
		return fmt.Errorf("failed to create commit: %w", err)
	}

	// 推进分支引用
	updateOpts := &rest.Opts{
		Method:     "PATCH",
		Path:       "/repos/" + f.opt.Owner + "/" + f.opt.Repo + "/git/refs/heads/" + escapePath(f.opt.Ref),
		NoResponse: true,
	}
	resp, err := f.callJSON(ctx, updateOpts, &api.UpdateRefRequest{Sha: commit.Sha}, nil)
	if err == nil {
		// 本进程刚推进了分支,立即刷新头 sha 缓存,
		// 保证后续 raw 下载用新 commit 锁定 URL(不受 CDN 缓存影响)
		f.setHeadCommit(commit.Sha)
		return nil
	}
	if resp != nil {
		switch resp.StatusCode {
		case http.StatusUnprocessableEntity:
			// non-fast-forward:分支被外部推进。不自动用旧 tree 重提交,
			// 避免覆盖并发改动;返回可重试错误让上层基于新状态重来
			return fserrors.RetryError(fmt.Errorf("branch %q was updated concurrently, retry the operation", f.opt.Ref))
		case http.StatusNotFound:
			if head == "" {
				// 空仓库:引用尚不存在,创建它
				createOpts := &rest.Opts{
					Method:     "POST",
					Path:       "/repos/" + f.opt.Owner + "/" + f.opt.Repo + "/git/refs",
					NoResponse: true,
				}
				if _, cerr := f.callJSON(ctx, createOpts, &api.CreateRefRequest{
					Ref: "refs/heads/" + f.opt.Ref,
					Sha: commit.Sha,
				}, nil); cerr != nil {
					return fmt.Errorf("failed to create branch ref: %w", cerr)
				}
				f.setHeadCommit(commit.Sha)
				return nil
			}
		}
	}
	return fmt.Errorf("failed to update branch ref: %w", err)
}

// dirEdit 记录单个目录的 tree 变更:以 baseSha 为 base_tree 应用 entries
type dirEdit struct {
	baseSha string
	entries []api.TreeEntry
}

// applyEdits 自底向上应用所有目录变更并返回新的根 tree sha。
// 每次先处理最深的目录:重建它得到新 sha,把新 sha 作为父目录变更中
// 对应条目的 sha 向上传递,直到根目录。base_tree 机制保证同一目录的
// 多个子树更新会在一次 createTree 中合并
func (f *Fs) applyEdits(ctx context.Context, edits map[string]*dirEdit) (string, error) {
	for len(edits) > 0 {
		// 选最深的目录先处理;根目录("")必须最后处理,否则会在还有
		// 同深度的顶层目录变更未应用时提前返回,静默丢失变更
		deepest := ""
		depth := -1
		for p := range edits {
			if p == "" {
				continue
			}
			if d := strings.Count(p, "/"); d > depth {
				depth, deepest = d, p
			}
		}
		e := edits[deepest]
		delete(edits, deepest)
		newSha, err := f.createTree(ctx, e.baseSha, e.entries)
		if err != nil {
			return "", err
		}
		if deepest == "" {
			// 只剩根目录,处理完即最终根 tree sha
			return newSha, nil
		}
		parent, childName := dirPath(deepest), leafName(deepest)
		pe, ok := edits[parent]
		if ok {
			// 父目录已有变更:更新其中 childName 条目的 sha,没有则追加
			found := false
			for i := range pe.entries {
				if pe.entries[i].Path == childName {
					pe.entries[i].Sha = newSha
					found = true
					break
				}
			}
			if found {
				continue
			}
		}
		// 需要父目录当前 tree 来获得 child 条目的 mode/type
		tree, _, err := f.getTreeByPath(ctx, parent)
		if err != nil {
			return "", fmt.Errorf("failed to get tree of %q: %w", parent, err)
		}
		entry := findTreeEntry(tree, childName)
		if entry == nil {
			return "", fs.ErrorObjectNotFound
		}
		renewEntry := api.TreeEntry{Path: childName, Mode: entry.Mode, Type: entry.Type, Sha: newSha}
		if ok {
			pe.entries = append(pe.entries, renewEntry)
		} else {
			edits[parent] = &dirEdit{baseSha: tree.Sha, entries: []api.TreeEntry{renewEntry}}
		}
	}
	return "", errors.New("github: internal error: edits map unexpectedly empty")
}

// findDeepestExistingDir 沿路径向上找到最深已存在的目录。
// 返回:祖先路径、其 tree、tree sha,以及缺失的目录链 chain(从浅到深)。
// 路径中间存在文件时返回不可重试错误;空仓库返回空 tree 与空 sha
func (f *Fs) findDeepestExistingDir(ctx context.Context, gitPath string) (base string, baseTree *api.TreeResponse, baseSha string, chain []string, err error) {
	p := gitPath
	for {
		tree, sha, terr := f.getTreeByPath(ctx, p)
		if terr == nil {
			return p, tree, sha, chain, nil
		}
		if errors.Is(terr, fs.ErrorDirNotFound) {
			// 路径指向文件,无法作为目录链的一部分
			return "", nil, "", nil, fserrors.NoRetryError(fmt.Errorf("%q already exists as a file", p))
		}
		if !errors.Is(terr, fs.ErrorObjectNotFound) {
			return "", nil, "", nil, terr
		}
		if p == "" {
			// 空仓库:根 tree 尚不存在,从零创建
			return "", &api.TreeResponse{}, "", chain, nil
		}
		chain = append([]string{p}, chain...)
		p = dirPath(p)
	}
}

// placeEntryEdits 构造把新条目 entry 放到 base 之下的变更集合。
// 中间缺失目录链 chain(从浅到深)在同一 commit 中补建;
// baseTree 用于判断挂载点是否只有 .gitkeep 占位需要移除
func (f *Fs) placeEntryEdits(ctx context.Context, base, baseSha string, baseTree *api.TreeResponse, chain []string, entry api.TreeEntry) (map[string]*dirEdit, error) {
	// 自深向浅逐层包裹:最深的缺失目录直接包含 entry 本身(保留其 mode/type),
	// 向上每层的 tree 只含下一层目录的 tree 引用;chain 为空时 entry 直接挂在 base 上
	var subSha string
	for i := len(chain) - 1; i >= 0; i-- {
		var entries []api.TreeEntry
		if i == len(chain)-1 {
			entries = []api.TreeEntry{entry}
		} else {
			entries = []api.TreeEntry{{Path: leafName(chain[i+1]), Mode: treeMode, Type: "tree", Sha: subSha}}
		}
		var err error
		subSha, err = f.createTree(ctx, "", entries)
		if err != nil {
			return nil, err
		}
	}
	mountEntry := entry
	if len(chain) > 0 {
		mountEntry = api.TreeEntry{Path: leafName(chain[0]), Mode: treeMode, Type: "tree", Sha: subSha}
	}
	entries := []api.TreeEntry{mountEntry}
	if len(baseTree.Tree) == 1 && baseTree.Tree[0].Path == gitkeepName {
		// 挂载点此前只有占位文件,现在有真实内容了,移除占位
		entries = append(entries, api.TreeEntry{Path: gitkeepName, Mode: blobMode, Type: "blob", Sha: nil})
	}
	return map[string]*dirEdit{base: {baseSha: baseSha, entries: entries}}, nil
}

// ------------------------------------------------------------ Fs 接口

// Name of the remote (as passed into NewFs)
func (f *Fs) Name() string { return f.name }

// Root of the remote (as passed into NewFs)
func (f *Fs) Root() string { return f.root }

// String converts this Fs to a string
func (f *Fs) String() string {
	if f.root == "" {
		return fmt.Sprintf("github repo %s/%s ref %s", f.opt.Owner, f.opt.Repo, f.opt.Ref)
	}
	return fmt.Sprintf("github repo %s/%s ref %s path %s", f.opt.Owner, f.opt.Repo, f.opt.Ref, f.root)
}

// Precision of the ModTimes in this Fs
func (f *Fs) Precision() time.Duration {
	return fs.ModTimeNotSupported
}

// Hashes returns the supported hash sets
func (f *Fs) Hashes() hash.Set {
	return hash.NewHashSet(gitSHA1Type)
}

// Features returns the optional features of this Fs
func (f *Fs) Features() *fs.Features {
	return f.features
}

// listItem 是 List 过程中的中间条目,统一 contents 与 tree 两种来源
type listItem struct {
	gitPath string // 仓库内绝对路径(存储形式)
	name    string // 显示名(存储形式)
	typ     string // contents: "file"/"dir"/"symlink"/"submodule"; trees: "blob"/"tree"/"commit"
	size    int64
	sha     string
}

// isDir 报告条目是否为目录
func (i *listItem) isDir() bool {
	return i.typ == "dir" || i.typ == "tree"
}

// List 列出目录
func (f *Fs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	gitDir := f.gitPath(dir)
	entry, err := f.getContents(ctx, gitDir)
	if err != nil {
		if errors.Is(err, fs.ErrorObjectNotFound) {
			// 空仓库的根目录 contents 会 404,此时应返回空列表而非报错
			if dir == "" {
				if _, berr := f.getBranchHead(ctx); errors.Is(berr, errBranchNotFound) {
					return fs.DirEntries{}, nil
				}
			}
			return nil, fs.ErrorDirNotFound
		}
		return nil, err
	}
	if entry.Entries == nil {
		return nil, fs.ErrorDirNotFound
	}

	var items []listItem
	if len(entry.Entries) >= contentsMaxEntries {
		// contents API 截断,回退 git trees API
		tree, err := f.getTree(ctx, entry.Sha, false)
		if err != nil {
			return nil, err
		}
		if tree.Truncated {
			return nil, fmt.Errorf("%w: %s", errTreeTruncated, dir)
		}
		for i := range tree.Tree {
			t := &tree.Tree[i]
			items = append(items, treeToListItem(gitDir, t.Path, t.Type, t.Size, shaString(t.Sha)))
		}
	} else {
		for i := range entry.Entries {
			e := &entry.Entries[i]
			items = append(items, treeToListItem(gitDir, e.Name, e.Type, e.Size, e.Sha))
		}
	}

	// 过滤 .gitkeep 占位与 submodule
	filtered := items[:0]
	for _, item := range items {
		if item.name == gitkeepName || item.isSubmodule() {
			continue
		}
		filtered = append(filtered, item)
	}
	items = filtered

	// 可选的精确 mtime
	var mtimes map[string]time.Time
	if f.opt.AccurateModifiedTime && len(items) > 0 && len(items) <= mtimeMaxEntries && f.opt.Token != "" {
		mtimes = f.fetchAccurateModifiedTimes(ctx, gitDir, items)
	}

	entries := make(fs.DirEntries, 0, len(items))
	for _, item := range items {
		remote := path.Join(dir, f.opt.Enc.ToStandardName(item.name))
		modTime := time.Time{}
		if mtimes != nil {
			if t, ok := mtimes[item.gitPath]; ok {
				modTime = t
			}
		}
		if item.isDir() {
			entries = append(entries, fs.NewDir(remote, modTime))
		} else {
			entries = append(entries, &Object{
				fs:      f,
				remote:  remote,
				gitPath: item.gitPath,
				sha:     item.sha,
				size:    item.size,
				modTime: modTime,
			})
		}
	}
	return entries, nil
}

// treeToListItem 把 contents entry 或 tree entry 转换为中间条目
func treeToListItem(gitDir, name, typ string, size int64, sha string) listItem {
	return listItem{
		gitPath: path.Join(gitDir, name),
		name:    name,
		typ:     typ,
		size:    size,
		sha:     sha,
	}
}

// isSubmodule 报告条目是否为 git submodule
func (i *listItem) isSubmodule() bool {
	return i.typ == "submodule" || i.typ == "commit"
}

// NewObject 查找 remote 处的文件对象
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	gitPath := f.gitPath(remote)
	entry, err := f.getContents(ctx, gitPath)
	if err != nil {
		return nil, err
	}
	if entry.IsDir() {
		return nil, fs.ErrorIsDir
	}
	if entry.IsSubmodule() {
		return nil, fs.ErrorNotAFile
	}
	modTime := time.Time{}
	// 单个对象也提供精确 mtime(一次 GraphQL 查询)
	if f.opt.AccurateModifiedTime && f.opt.Token != "" {
		if mtimes := f.fetchAccurateModifiedTimes(ctx, dirPath(gitPath), []listItem{{gitPath: gitPath}}); mtimes != nil {
			if t, ok := mtimes[gitPath]; ok {
				modTime = t
			}
		}
	}
	return &Object{
		fs:      f,
		remote:  remote,
		gitPath: gitPath,
		sha:     entry.Sha,
		size:    entry.Size,
		modTime: modTime,
	}, nil
}

// Put 上传文件
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	o := &Object{
		fs:      f,
		remote:  src.Remote(),
		gitPath: f.gitPath(src.Remote()),
		size:    src.Size(),
		modTime: time.Time{},
	}
	err := o.Update(ctx, in, src, options...)
	return o, err
}

// Mkdir 创建目录。git 不支持空目录,用 .gitkeep 占位
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	if !f.isOnBranch {
		return fmt.Errorf("cannot write to non-branch reference %q", f.opt.Ref)
	}
	// dir 为空表示创建 Fs 根目录本身;仓库根目录恒存在,直接成功
	if dir == "" && f.root == "" {
		return nil
	}
	gitDir := f.gitPath(dir)
	// 已存在则直接成功
	if entry, err := f.getContents(ctx, gitDir); err == nil {
		if entry.IsDir() {
			return nil
		}
		return fmt.Errorf("%q already exists as a file", dir)
	} else if !errors.Is(err, fs.ErrorObjectNotFound) {
		return err
	}

	f.commitMu.Lock()
	defer f.commitMu.Unlock()

	parent, name := dirPath(gitDir), leafName(gitDir)
	// rclone 不保证先 Mkdir 父目录再 Mkdir 本目录,缺失的中间链在同一 commit 补建
	base, baseTree, baseSha, chain, err := f.findDeepestExistingDir(ctx, parent)
	if err != nil {
		return err
	}
	subSha, err := f.createTree(ctx, "", []api.TreeEntry{gitkeepEntry()})
	if err != nil {
		return err
	}
	edits, err := f.placeEntryEdits(ctx, base, baseSha, baseTree, chain, api.TreeEntry{
		Path: name,
		Mode: treeMode,
		Type: "tree",
		Sha:  subSha,
	})
	if err != nil {
		return err
	}

	rootSha, err := f.applyEdits(ctx, edits)
	if err != nil {
		return err
	}
	message, err := commitMessage(f.opt.MkdirCommitMessage, "mkdir", &messageTemplateVars{
		ObjName:    leafName(dir),
		ObjPath:    dir,
		ParentName: path.Base(dirPath(dir)),
		ParentPath: dirPath(dir),
	})
	if err != nil {
		return err
	}
	return f.commitTree(ctx, message, rootSha)
}

// Rmdir 删除空目录(仅剩 .gitkeep 视为空)
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	if !f.isOnBranch {
		return fmt.Errorf("cannot write to non-branch reference %q", f.opt.Ref)
	}
	// dir 为空表示删除 Fs 根目录;仓库根目录不可删除
	if dir == "" && f.root == "" {
		return errors.New("can't remove the repository root")
	}
	gitDir := f.gitPath(dir)

	f.commitMu.Lock()
	defer f.commitMu.Unlock()

	entry, err := f.getContents(ctx, gitDir)
	if err != nil {
		if errors.Is(err, fs.ErrorObjectNotFound) {
			return fs.ErrorDirNotFound
		}
		return err
	}
	if entry.Entries == nil {
		return fs.ErrorDirNotFound
	}
	// 目录必须为空(只允许 .gitkeep 占位)
	for i := range entry.Entries {
		if entry.Entries[i].Name != gitkeepName {
			return fs.ErrorDirectoryNotEmpty
		}
	}

	parent, name := dirPath(gitDir), leafName(gitDir)
	parentTree, parentSha, err := f.getTreeByPath(ctx, parent)
	if err != nil {
		return err
	}
	entries := []api.TreeEntry{{Path: name, Mode: treeMode, Type: "tree", Sha: nil}}
	if len(parentTree.Tree) == 1 {
		// 删掉唯一条目会让父目录变空,补 .gitkeep 占位
		entries = append(entries, gitkeepEntry())
	}
	edits := map[string]*dirEdit{parent: {baseSha: parentSha, entries: entries}}
	rootSha, err := f.applyEdits(ctx, edits)
	if err != nil {
		return err
	}
	message, err := commitMessage(f.opt.DeleteCommitMessage, "remove", &messageTemplateVars{
		ObjName:    leafName(dir),
		ObjPath:    dir,
		ParentName: path.Base(dirPath(dir)),
		ParentPath: dirPath(dir),
	})
	if err != nil {
		return err
	}
	return f.commitTree(ctx, message, rootSha)
}

// moveEntry 把 srcRemote 处的条目移动到 dstRemote(单个 commit 完成),
// 文件与目录通用。核心是收集受影响目录的 tree 变更,自底向上重建
// moveEntry 把 srcPath 处的条目移动到 dstPath(仓库内绝对路径,单个 commit),
// 文件与目录通用。srcPath 与 dstPath 可能各自相对不同的 Fs 实例计算,
// 因此必须传入绝对路径。目标父目录不存在时自动补建目录链
// (rclone 不保证 Move 前目标目录已存在)。srcRemote/dstRemote 仅用于 commit message
func (f *Fs) moveEntry(ctx context.Context, srcPath, dstPath, srcRemote, dstRemote string) error {
	if srcPath == dstPath {
		return nil
	}
	if dstPath == dirPath(srcPath) || strings.HasPrefix(dstPath, srcPath+"/") {
		return errors.New("cannot move a directory into itself")
	}
	srcParent, srcName := dirPath(srcPath), leafName(srcPath)
	dstParent, dstName := dirPath(dstPath), leafName(dstPath)

	srcTree, srcParentSha, err := f.getTreeByPath(ctx, srcParent)
	if err != nil {
		return err
	}
	srcEntry := findTreeEntry(srcTree, srcName)
	if srcEntry == nil {
		return fs.ErrorObjectNotFound
	}
	if srcEntry.Type == "commit" {
		return fmt.Errorf("%w: %s", errSubmodule, srcRemote)
	}

	// 目标侧:允许父目录缺失,自动补建目录链
	dstBase, dstBaseTree, dstBaseSha, dstChain, err := f.findDeepestExistingDir(ctx, dstParent)
	if err != nil {
		return err
	}

	edits := map[string]*dirEdit{}
	// 源侧删除条目(父目录变空则补 .gitkeep)
	srcEntries := []api.TreeEntry{{Path: srcName, Mode: srcEntry.Mode, Type: srcEntry.Type, Sha: nil}}
	if len(srcTree.Tree) == 1 {
		srcEntries = append(srcEntries, gitkeepEntry())
	}

	if srcParent == dstParent && len(dstChain) == 0 {
		// 同目录重命名:删除旧名 + 新名指向原 sha,一次 tree 变更完成
		edits[srcParent] = &dirEdit{baseSha: srcParentSha, entries: append(srcEntries,
			api.TreeEntry{Path: dstName, Mode: srcEntry.Mode, Type: srcEntry.Type, Sha: srcEntry.Sha})}
	} else {
		// 目标侧挂载(可能带目录链),再合并源侧删除
		dstEdits, err := f.placeEntryEdits(ctx, dstBase, dstBaseSha, dstBaseTree, dstChain, api.TreeEntry{
			Path: dstName,
			Mode: srcEntry.Mode,
			Type: srcEntry.Type,
			Sha:  srcEntry.Sha,
		})
		if err != nil {
			return err
		}
		for dirPath, e := range dstEdits {
			edits[dirPath] = e
		}
		if existing, ok := edits[srcParent]; ok {
			// srcParent 与目标挂载点是同一目录时合并变更
			existing.entries = append(existing.entries, srcEntries...)
		} else {
			edits[srcParent] = &dirEdit{baseSha: srcParentSha, entries: srcEntries}
		}
	}

	rootSha, err := f.applyEdits(ctx, edits)
	if err != nil {
		return err
	}
	message, err := commitMessage(f.opt.MoveCommitMessage, "move", &messageTemplateVars{
		ObjName:    leafName(srcRemote),
		ObjPath:    srcRemote,
		ParentName: path.Base(dirPath(srcRemote)),
		ParentPath: dirPath(srcRemote),
		TargetName: leafName(dstRemote),
		TargetPath: dstRemote,
	})
	if err != nil {
		return err
	}
	return f.commitTree(ctx, message, rootSha)
}

// copyEntry 把 srcRemote 处的条目复制到 dstRemote(纯 tree 操作,不传内容)
// copyEntry 把 srcPath 处的条目复制到 dstPath(仓库内绝对路径,纯 tree 操作不传内容)。
// srcPath 与 dstPath 可能各自相对不同的 Fs 实例计算,因此必须传入绝对路径;
// 目标父目录不存在时自动补建目录链。srcRemote/dstRemote 仅用于 commit message
func (f *Fs) copyEntry(ctx context.Context, srcPath, dstPath, srcRemote, dstRemote string) error {
	if srcPath == dstPath {
		return nil
	}
	if strings.HasPrefix(dstPath, srcPath+"/") {
		return errors.New("cannot copy a directory into itself")
	}
	srcParent, srcName := dirPath(srcPath), leafName(srcPath)
	dstParent, dstName := dirPath(dstPath), leafName(dstPath)

	srcTree, _, err := f.getTreeByPath(ctx, srcParent)
	if err != nil {
		return err
	}
	srcEntry := findTreeEntry(srcTree, srcName)
	if srcEntry == nil {
		return fs.ErrorObjectNotFound
	}
	if srcEntry.Type == "commit" {
		return fmt.Errorf("%w: %s", errSubmodule, srcRemote)
	}

	dstBase, dstBaseTree, dstBaseSha, dstChain, err := f.findDeepestExistingDir(ctx, dstParent)
	if err != nil {
		return err
	}
	edits, err := f.placeEntryEdits(ctx, dstBase, dstBaseSha, dstBaseTree, dstChain, api.TreeEntry{
		Path: dstName,
		Mode: srcEntry.Mode,
		Type: srcEntry.Type,
		Sha:  srcEntry.Sha,
	})
	if err != nil {
		return err
	}
	rootSha, err := f.applyEdits(ctx, edits)
	if err != nil {
		return err
	}
	message, err := commitMessage(f.opt.CopyCommitMessage, "copy", &messageTemplateVars{
		ObjName:    leafName(srcRemote),
		ObjPath:    srcRemote,
		ParentName: path.Base(dirPath(srcRemote)),
		ParentPath: dirPath(srcRemote),
		TargetName: leafName(dstRemote),
		TargetPath: dstRemote,
	})
	if err != nil {
		return err
	}
	return f.commitTree(ctx, message, rootSha)
}

// sameRepo 报告两个 Fs 是否指向同一个仓库(tree sha 才能互用)
func sameRepo(a, b *Fs) bool {
	return a.opt.Owner == b.opt.Owner && a.opt.Repo == b.opt.Repo && a.opt.Ref == b.opt.Ref
}

// Move 把 src 移动到 remote 处(服务端操作)
func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	srcObj, ok := src.(*Object)
	if !ok || !sameRepo(srcObj.fs, f) {
		return nil, fs.ErrorCantMove
	}
	if !f.isOnBranch {
		return nil, fmt.Errorf("cannot write to non-branch reference %q", f.opt.Ref)
	}
	f.commitMu.Lock()
	defer f.commitMu.Unlock()
	// src 路径相对源 Fs 计算,dst 路径相对本 Fs 计算,两者可能是不同实例
	if err := f.moveEntry(ctx, srcObj.gitPath, f.gitPath(remote), srcObj.Remote(), remote); err != nil {
		return srcObj, err
	}
	return &Object{
		fs:      f,
		remote:  remote,
		gitPath: f.gitPath(remote),
		sha:     srcObj.sha,
		size:    srcObj.size,
		modTime: srcObj.modTime,
	}, nil
}

// Copy 把 src 复制到 remote 处(服务端操作,不传输内容)
func (f *Fs) Copy(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	srcObj, ok := src.(*Object)
	if !ok || !sameRepo(srcObj.fs, f) {
		return nil, fs.ErrorCantCopy
	}
	if !f.isOnBranch {
		return nil, fmt.Errorf("cannot write to non-branch reference %q", f.opt.Ref)
	}
	f.commitMu.Lock()
	defer f.commitMu.Unlock()
	if err := f.copyEntry(ctx, srcObj.gitPath, f.gitPath(remote), srcObj.Remote(), remote); err != nil {
		return nil, err
	}
	return &Object{
		fs:      f,
		remote:  remote,
		gitPath: f.gitPath(remote),
		sha:     srcObj.sha,
		size:    srcObj.size,
		modTime: srcObj.modTime,
	}, nil
}

// DirMove 把 srcRemote 目录移动到 dstRemote(服务端操作)。
// src 与 f 可能是同仓库的不同实例(各自 root 不同),路径各自相对自己的 Fs 计算
func (f *Fs) DirMove(ctx context.Context, src fs.Fs, srcRemote, dstRemote string) error {
	srcFs, ok := src.(*Fs)
	if !ok || !sameRepo(srcFs, f) {
		return fs.ErrorCantDirMove
	}
	if !f.isOnBranch {
		return fmt.Errorf("cannot write to non-branch reference %q", f.opt.Ref)
	}
	// 接口契约:目标已存在时返回 ErrorDirExists
	if _, err := f.getContents(ctx, f.gitPath(dstRemote)); err == nil {
		return fs.ErrorDirExists
	} else if !errors.Is(err, fs.ErrorObjectNotFound) {
		return err
	}
	f.commitMu.Lock()
	defer f.commitMu.Unlock()
	return f.moveEntry(ctx, srcFs.gitPath(srcRemote), f.gitPath(dstRemote), srcRemote, dstRemote)
}

// ListR 递归列出。优先用 recursive git tree 一次拿全,
// 被截断(超过 10 万条目或 7MB 响应)时回退逐目录 List
func (f *Fs) ListR(ctx context.Context, dir string, callback fs.ListRCallback) error {
	gitDir := f.gitPath(dir)
	// 根目录可以直接用 ref 作为 tree 标识,子目录需要先取其 tree sha
	treeID := f.opt.Ref
	if gitDir != "" {
		entry, err := f.getContents(ctx, gitDir)
		if err != nil {
			if errors.Is(err, fs.ErrorObjectNotFound) {
				return fs.ErrorDirNotFound
			}
			return err
		}
		if entry.Entries == nil {
			return fs.ErrorDirNotFound
		}
		treeID = entry.Sha
	}
	tree, err := f.getTree(ctx, treeID, true)
	if err != nil {
		if errors.Is(err, fs.ErrorObjectNotFound) {
			// 空仓库
			if gitDir == "" {
				if _, berr := f.getBranchHead(ctx); errors.Is(berr, errBranchNotFound) {
					return callback(nil)
				}
			}
			return fs.ErrorDirNotFound
		}
		return err
	}
	if tree.Truncated {
		return f.listRWalk(ctx, dir, callback)
	}

	// 收集中间条目,便于统一做 mtime 批量查询
	type rItem struct {
		remote  string
		gitPath string
		isDir   bool
		sha     string
		size    int64
	}
	items := make([]rItem, 0, len(tree.Tree))
	for i := range tree.Tree {
		t := &tree.Tree[i]
		// .gitkeep 占位与 submodule 在任意层级都要过滤
		if leafName(t.Path) == gitkeepName || t.Type == "commit" {
			continue
		}
		items = append(items, rItem{
			remote:  path.Join(dir, f.opt.Enc.ToStandardPath(t.Path)),
			gitPath: path.Join(gitDir, t.Path),
			isDir:   t.Type == "tree",
			sha:     shaString(t.Sha),
			size:    t.Size,
		})
	}

	// 可选的精确 mtime(与 List 同样的条目上限)
	var mtimes map[string]time.Time
	if f.opt.AccurateModifiedTime && len(items) > 0 && len(items) <= mtimeMaxEntries && f.opt.Token != "" {
		listItems := make([]listItem, 0, len(items))
		for _, it := range items {
			listItems = append(listItems, listItem{gitPath: it.gitPath})
		}
		mtimes = f.fetchAccurateModifiedTimes(ctx, gitDir, listItems)
	}

	entries := make(fs.DirEntries, 0, len(items))
	for _, it := range items {
		modTime := time.Time{}
		if mtimes != nil {
			if t, ok := mtimes[it.gitPath]; ok {
				modTime = t
			}
		}
		if it.isDir {
			entries = append(entries, fs.NewDir(it.remote, modTime))
		} else {
			entries = append(entries, &Object{
				fs:      f,
				remote:  it.remote,
				gitPath: it.gitPath,
				sha:     it.sha,
				size:    it.size,
				modTime: modTime,
			})
		}
	}
	return callback(entries)
}

// listRWalk 是 ListR 的回退实现:逐目录 List 并递归
func (f *Fs) listRWalk(ctx context.Context, dir string, callback fs.ListRCallback) error {
	entries, err := f.List(ctx, dir)
	if err != nil {
		return err
	}
	if err := callback(entries); err != nil {
		return err
	}
	for _, e := range entries {
		if d, ok := e.(fs.Directory); ok {
			if err := f.listRWalk(ctx, d.Remote(), callback); err != nil {
				return err
			}
		}
	}
	return nil
}

// ------------------------------------------------------------ 上传

// putBlob 把内容流式上传为 git blob(base64 内嵌 JSON,io.Pipe 不落盘),
// 返回 blob sha。请求体是单次流,不做低层重试,失败由上层整体重试
func (f *Fs) putBlob(ctx context.Context, in io.Reader, size int64) (string, error) {
	body, length := base64BlobBody(in, size)
	opts := &rest.Opts{
		Method:        "POST",
		Path:          "/repos/" + f.opt.Owner + "/" + f.opt.Repo + "/git/blobs",
		Body:          body,
		ContentType:   "application/json",
		ContentLength: &length,
	}
	result := &api.BlobResponse{}
	var resp *http.Response
	err := f.pacer.CallNoRetry(func() (bool, error) {
		var err error
		resp, err = f.srv.CallJSON(ctx, opts, nil, result)
		return f.shouldRetry(ctx, resp, err)
	})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusRequestEntityTooLarge {
			return "", fserrors.NoRetryError(fmt.Errorf("file of %d bytes exceeds the GitHub blob API limit", size))
		}
		return "", fmt.Errorf("failed to create blob: %w", err)
	}
	return result.Sha, nil
}

// base64BlobBody 构造 blob API 的 JSON 请求体:
// {"encoding":"base64","content":"<base64>"}。
// 内容通过 io.Pipe 流式编码,同时返回精确的 Content-Length
func base64BlobBody(in io.Reader, size int64) (io.Reader, int64) {
	const before = `{"encoding":"base64","content":"`
	const after = `"}`
	contentReader, contentWriter := io.Pipe()
	go func() {
		encoder := base64.NewEncoder(base64.StdEncoding, contentWriter)
		if _, err := io.Copy(encoder, in); err != nil {
			_ = contentWriter.CloseWithError(err)
			return
		}
		if err := encoder.Close(); err != nil {
			_ = contentWriter.CloseWithError(err)
			return
		}
		_ = contentWriter.Close()
	}()
	length := int64(len(before)) + calculateBase64Length(size) + int64(len(after))
	return io.MultiReader(strings.NewReader(before), contentReader, strings.NewReader(after)), length
}

// calculateBase64Length 计算 base64 编码后的长度
func calculateBase64Length(inputLength int64) int64 {
	return 4 * ((inputLength + 2) / 3)
}

// ------------------------------------------------------------ commit message

// messageTemplateVars 是 commit message 模板的可用变量
type messageTemplateVars struct {
	UserName   string // 固定为 "rclone",仅为兼容 OpenList 风格模板
	ObjName    string
	ObjPath    string
	ParentName string
	ParentPath string
	TargetName string
	TargetPath string
}

// commitMessage 渲染 commit message。模板渲染失败时返回兜底消息与错误,
// 与 OpenList 行为一致:调用方决定是否中断操作
func commitMessage(templateText, op string, vars *messageTemplateVars) (string, error) {
	fallback := "rclone " + op + " " + vars.ObjPath
	tmpl, err := template.New("commitMessage").Parse(templateText)
	if err != nil {
		return fallback, fmt.Errorf("invalid %s commit message template: %w", op, err)
	}
	var sb strings.Builder
	if err := tmpl.Execute(&sb, vars); err != nil {
		return fallback, fmt.Errorf("failed to render %s commit message: %w", op, err)
	}
	return sb.String(), nil
}

// ------------------------------------------------------------ 精确 mtime

// quoteGraphQLString 把字符串转为 GraphQL 字面量
func quoteGraphQLString(value string) string {
	quoted, _ := json.Marshal(value)
	return string(quoted)
}

// buildMtimeQuery 构造批量查询每个路径最后 commit 时间的 GraphQL 查询
func buildMtimeQuery(owner, repo, ref string, gitPaths []string) string {
	histories := make([]string, 0, len(gitPaths))
	for i, p := range gitPaths {
		histories = append(histories, fmt.Sprintf(`p%d: history(first: 1, path: %s) { nodes { committedDate } }`, i, quoteGraphQLString(p)))
	}
	return fmt.Sprintf(`query { repository(owner: %s, name: %s) { commit: object(expression: %s) { ... on Commit { %s } } } }`,
		quoteGraphQLString(owner), quoteGraphQLString(repo), quoteGraphQLString(ref+"^{commit}"), strings.Join(histories, " "))
}

// fetchAccurateModifiedTimes 批量获取条目的最后 commit 时间。
// 失败时静默降级为零时间,不阻断 List
func (f *Fs) fetchAccurateModifiedTimes(ctx context.Context, gitDir string, items []listItem) map[string]time.Time {
	gitPaths := make([]string, 0, len(items))
	for _, item := range items {
		gitPaths = append(gitPaths, item.gitPath)
	}
	query := buildMtimeQuery(f.opt.Owner, f.opt.Repo, f.opt.Ref, gitPaths)
	opts := &rest.Opts{
		Method:      "POST",
		RootURL:     graphqlEndpoint,
		ContentType: "application/json",
	}
	result := &api.MtimeResponse{}
	resp, err := f.callJSON(ctx, opts, &api.MtimeRequest{Query: query}, result)
	if err != nil {
		fs.Debugf(f, "accurate mtime query failed for %q: %v", gitDir, err)
		return nil
	}
	if resp != nil && resp.StatusCode != http.StatusOK || len(result.Errors) > 0 {
		fs.Debugf(f, "accurate mtime query rejected for %q", gitDir)
		return nil
	}
	mtimes := make(map[string]time.Time, len(gitPaths))
	commit := result.Data.Repository.Commit
	for i, p := range gitPaths {
		history, ok := commit[fmt.Sprintf("p%d", i)]
		if !ok || len(history.Nodes) == 0 {
			continue
		}
		mtimes[p] = history.Nodes[0].CommittedDate
	}
	return mtimes
}

// ------------------------------------------------------------ Object

// Object 描述 github 上的一个文件
type Object struct {
	fs      *Fs       // 所属 Fs
	remote  string    // rclone 路径(相对 root)
	gitPath string    // 仓库内路径(存储形式)
	sha     string    // git blob sha
	size    int64     // 字节数
	modTime time.Time // 修改时间(默认零值)
}

// String 格式化为字符串
func (o *Object) String() string {
	if o == nil {
		return "<nil>"
	}
	return o.remote
}

// Remote 返回 rclone 路径
func (o *Object) Remote() string { return o.remote }

// ModTime 返回修改时间
func (o *Object) ModTime(context.Context) time.Time { return o.modTime }

// Size 返回大小
func (o *Object) Size() int64 { return o.size }

// Fs 返回所属 Fs
func (o *Object) Fs() fs.Info { return o.fs }

// Hash 返回指定类型的校验和,gitsha1 直接取服务端 sha
func (o *Object) Hash(ctx context.Context, ty hash.Type) (string, error) {
	if ty == gitSHA1Type && o.sha != "" {
		return o.sha, nil
	}
	return "", hash.ErrUnsupported
}

// Storable 表明对象可存储
func (o *Object) Storable() bool { return true }

// SetModTime 不支持设置修改时间
func (o *Object) SetModTime(ctx context.Context, t time.Time) error {
	return fs.ErrorCantSetModTime
}

// openViaRaw 走 raw.githubusercontent.com 直链下载。
// URL 用分支头 commit sha 锁定:分支路径的 raw URL 会被 CDN 缓存,
// 写后立即读会拿到旧内容,而 commit 锁定的 URL 内容不可变、不受缓存影响
func (o *Object) openViaRaw(ctx context.Context, options []fs.OpenOption) (io.ReadCloser, error) {
	head, err := o.fs.headCommit(ctx)
	if err != nil {
		return nil, err
	}
	downloadURL := rawBase + "/" + o.fs.opt.Owner + "/" + o.fs.opt.Repo + "/" + escapePath(head) + "/" + escapePath(o.gitPath)
	if proxy := strings.TrimSpace(o.fs.opt.GhProxy); proxy != "" {
		downloadURL = strings.Replace(downloadURL, rawBase, proxy, 1)
	}
	opts := &rest.Opts{
		Method:  "GET",
		RootURL: downloadURL,
		Options: options,
	}
	// 直连 raw 时带认证(私有仓库必需);经代理替换后 URL 不再以 raw 域名开头,
	// 此时不能发送 Authorization,避免凭证泄露给第三方代理
	if strings.HasPrefix(downloadURL, rawBase) {
		opts.ExtraHeaders = map[string]string{"Authorization": "Bearer " + o.fs.opt.Token}
	}
	var resp *http.Response
	err = o.fs.pacer.Call(func() (bool, error) {
		var err error
		resp, err = o.fs.rawSrv.Call(ctx, opts)
		return o.fs.shouldRetry(ctx, resp, err)
	})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return nil, fs.ErrorObjectNotFound
		}
		return nil, fmt.Errorf("failed to download %q: %w", o.remote, err)
	}
	return resp.Body, nil
}

// openViaAPI 走 contents API raw media type 下载
func (o *Object) openViaAPI(ctx context.Context, options []fs.OpenOption) (io.ReadCloser, error) {
	opts := &rest.Opts{
		Method:       "GET",
		RootURL:      apiBase,
		Path:         o.fs.contentsPath(o.gitPath),
		Parameters:   url.Values{"ref": {o.fs.opt.Ref}},
		ExtraHeaders: map[string]string{"Accept": "application/vnd.github.raw"},
		Options:      options,
	}
	var resp *http.Response
	err := o.fs.pacer.Call(func() (bool, error) {
		var err error
		resp, err = o.fs.rawSrv.Call(ctx, opts)
		return o.fs.shouldRetry(ctx, resp, err)
	})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return nil, fs.ErrorObjectNotFound
		}
		return nil, fmt.Errorf("failed to download %q: %w", o.remote, err)
	}
	return resp.Body, nil
}

// rangeWindow 从 options 解析出请求的字节窗口 [start, end) 与是否为部分请求
func rangeWindow(options []fs.OpenOption, size int64) (start, end int64, partial bool) {
	start, end = 0, size
	for _, option := range options {
		switch x := option.(type) {
		case *fs.SeekOption:
			start = x.Offset
			partial = true
		case *fs.RangeOption:
			if x.Start >= 0 {
				start = x.Start
				if x.End > 0 && x.End < size {
					end = x.End + 1
				}
			} else {
				// {-1, N} 表示取最后 N 字节
				start = max(size-x.End, 0)
			}
			partial = true
		default:
			if option.Mandatory() {
				fs.Logf(nil, "Unsupported mandatory option: %v", option)
			}
		}
	}
	return start, end, partial
}

// Open 打开文件读取,支持 Range。
// raw 通道由 CDN 保证支持 Range;api 通道不保证,若请求了部分内容,
// 丢弃窗口之前的内容并限制读取长度,保证 Range 语义
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	start, end, partial := rangeWindow(options, o.size)

	var body io.ReadCloser
	if o.fs.opt.DownloadVia == "raw" {
		b, err := o.openViaRaw(ctx, options)
		if err == nil {
			body = b
		} else if !errors.Is(err, fs.ErrorObjectNotFound) {
			return nil, err
		}
		// 404 时对象可能在缓存的头 commit 之后被创建,回退 api 通道
	}
	if body == nil {
		var err error
		body, err = o.openViaAPI(ctx, options)
		if err != nil {
			return nil, err
		}
		if partial {
			if start > 0 {
				if _, err := io.CopyN(io.Discard, body, start); err != nil {
					return nil, fmt.Errorf("failed to discard %d bytes of %q: %w", start, o.remote, err)
				}
			}
			return readers.NewLimitedReadCloser(body, end-start), nil
		}
	}
	return body, nil
}

// Update 上传内容到对象路径
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	if !o.fs.isOnBranch {
		return fserrors.NoRetryError(fmt.Errorf("cannot write to non-branch reference %q", o.fs.opt.Ref))
	}
	size := src.Size()
	if size < 0 {
		return fserrors.NoRetryError(errors.New("the github backend needs the file size to be known before upload"))
	}
	if size > maxBlobSize {
		return fserrors.NoRetryError(fmt.Errorf("file %s is %s which exceeds the GitHub Git Blob API limit of %s", o.remote, fs.SizeSuffix(size), fs.SizeSuffix(maxBlobSize)))
	}

	// 上传的同时计算本地 git blob sha 用于校验
	h := gitsha1.NewSized(size)
	blobSha, err := o.fs.putBlob(ctx, io.TeeReader(in, h), size)
	if err != nil {
		return err
	}
	if localSha := hex.EncodeToString(h.Sum(nil)); localSha != blobSha {
		return fserrors.NoRetryError(fmt.Errorf("blob sha mismatch for %s: local %s, server %s", o.remote, localSha, blobSha))
	}

	o.fs.commitMu.Lock()
	defer o.fs.commitMu.Unlock()

	parent, name := dirPath(o.gitPath), leafName(o.gitPath)
	// rclone 不保证先 Mkdir 目标目录再 Put(单文件 copy 直达不存在路径),
	// 缺失的父目录链在同一 commit 中补建
	base, baseTree, baseSha, chain, err := o.fs.findDeepestExistingDir(ctx, parent)
	if err != nil {
		return err
	}
	edits, err := o.fs.placeEntryEdits(ctx, base, baseSha, baseTree, chain, api.TreeEntry{
		Path: name,
		Mode: blobMode,
		Type: "blob",
		Sha:  blobSha,
	})
	if err != nil {
		return err
	}
	rootSha, err := o.fs.applyEdits(ctx, edits)
	if err != nil {
		return err
	}
	message, err := commitMessage(o.fs.opt.PutCommitMessage, "upload", &messageTemplateVars{
		ObjName:    leafName(o.remote),
		ObjPath:    o.remote,
		ParentName: path.Base(dirPath(o.remote)),
		ParentPath: dirPath(o.remote),
	})
	if err != nil {
		return err
	}
	if err := o.fs.commitTree(ctx, message, rootSha); err != nil {
		return err
	}

	o.sha = blobSha
	o.size = size
	// commit 刚发生,用当前时间近似 commit 时间;
	// 精确值由后续 NewObject/List 的 GraphQL 查询提供
	o.modTime = time.Now()
	return nil
}

// Remove 删除对象
func (o *Object) Remove(ctx context.Context) error {
	if !o.fs.isOnBranch {
		return fmt.Errorf("cannot write to non-branch reference %q", o.fs.opt.Ref)
	}
	o.fs.commitMu.Lock()
	defer o.fs.commitMu.Unlock()

	parent, name := dirPath(o.gitPath), leafName(o.gitPath)
	tree, treeSha, err := o.fs.getTreeByPath(ctx, parent)
	if err != nil {
		return err
	}
	entry := findTreeEntry(tree, name)
	if entry == nil {
		return fs.ErrorObjectNotFound
	}
	if entry.Type == "commit" {
		return fmt.Errorf("%w: %s", errSubmodule, o.remote)
	}
	entries := []api.TreeEntry{{Path: name, Mode: entry.Mode, Type: entry.Type, Sha: nil}}
	if len(tree.Tree) == 1 {
		// 删掉唯一条目会让父目录变空,补 .gitkeep 占位
		entries = append(entries, gitkeepEntry())
	}
	edits := map[string]*dirEdit{parent: {baseSha: treeSha, entries: entries}}
	rootSha, err := o.fs.applyEdits(ctx, edits)
	if err != nil {
		return err
	}
	message, err := commitMessage(o.fs.opt.DeleteCommitMessage, "remove", &messageTemplateVars{
		ObjName:    leafName(o.remote),
		ObjPath:    o.remote,
		ParentName: path.Base(dirPath(o.remote)),
		ParentPath: dirPath(o.remote),
	})
	if err != nil {
		return err
	}
	return o.fs.commitTree(ctx, message, rootSha)
}

// 接口断言
var (
	_ fs.Fs       = (*Fs)(nil)
	_ fs.Mover    = (*Fs)(nil)
	_ fs.Copier   = (*Fs)(nil)
	_ fs.DirMover = (*Fs)(nil)
	_ fs.ListRer  = (*Fs)(nil)
	_ fs.Object   = (*Object)(nil)
)
