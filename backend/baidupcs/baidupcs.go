// Package baidupcs provides an interface to Baidu Netdisk (百度网盘)
// via the open platform API (pan.baidu.com/union).
//
// 协议行为参考 OpenList 社区驱动 baidu_netdisk 的长期验证结论，
// 代码按 rclone 规范从零编写；不引入任何第三方 SDK。
package baidupcs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/encoder"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/rest"
)

const (
	minSleep = 200 * time.Millisecond
	maxSleep = 2 * time.Second
)

func init() {
	fs.Register(&fs.RegInfo{
		Name:        "baidupcs",
		Description: "Baidu Netdisk (百度网盘)",
		NewFs:       NewFs,
		Config:      Config,
		Options: []fs.Option{{
			Name:     "app_key",
			Help:     "AppKey (client_id) of your Baidu Netdisk open platform application.\n\nCreate an application in the open platform console (https://pan.baidu.com/union/console) and paste its AppKey here.",
			Required: true,
		}, {
			Name:      "app_secret",
			Help:      "SecretKey (client_secret) of your Baidu Netdisk open platform application.",
			Required:  true,
			Sensitive: true,
		}, {
			Name: "root",
			Help: `Application directory path on the server.

Fill in the full path including the /apps/ prefix, for example
/apps/WordAgent where WordAgent is the 产品名称 (product name) of
your application. Entering just the app name would create a
top-level directory outside the application area - that works
today while the API still allows full access, but Baidu's
permission policy is expected to confine applications to
/apps/{产品名称} and may tighten in the future.

The directory is created automatically on first use if it doesn't
exist.`,
			Default: "/apps/",
			Examples: []fs.OptionExample{{
				Value: "/apps/WordAgent",
				Help:  "Application directory (recommended: /apps/ + 产品名称 from the console)",
			}},
		}, {
			Name:     "upload_concurrency",
			Help:     "Number of concurrent slice uploads.\n\nEach file upload is split into slices (4/16/32MB depending on account type) which are uploaded concurrently. 1 to 32.",
			Default:  3,
			Advanced: true,
		}, {
			Name:      "access_token",
			Help:      "OAuth access token - set by the config wizard, may be empty.",
			Hide:      fs.OptionHideBoth,
			Sensitive: true,
		}, {
			Name:      "refresh_token",
			Help:      "OAuth refresh token - set by the config wizard.",
			Hide:      fs.OptionHideBoth,
			Sensitive: true,
		}, {
			Name:     config.ConfigEncoding,
			Help:     config.ConfigEncodingHelp,
			Advanced: true,
			Default: (encoder.EncodeWin | // :?"*<>| 百度禁止的文件名字符
				encoder.EncodeBackSlash |
				// 全角点/斜杠会被 encoder 的 Standard 层解码成 ASCII 的 . 和 /,
				// 前者被 path.Join 清洗后会让 create 落在父目录上直接替换掉整个
				// 目录（实测踩坑）,后者会分裂路径,必须编码
				encoder.EncodeDot |
				encoder.EncodeSlash |
				// 尾部空格与尾部点会被服务端裁剪,不编码则往返不一致
				encoder.EncodeRightSpace |
				encoder.EncodeRightPeriod |
				encoder.EncodeCtl |
				encoder.EncodeDel |
				encoder.EncodeInvalidUtf8),
		}},
	})
}

// Options defines the configuration for this backend
type Options struct {
	AppKey            string               `config:"app_key"`
	AppSecret         string               `config:"app_secret"`
	Root              string               `config:"root"`
	UploadConcurrency int                  `config:"upload_concurrency"`
	AccessToken       string               `config:"access_token"`
	RefreshToken      string               `config:"refresh_token"`
	Enc               encoder.MultiEncoder `config:"encoding"`
}

// Fs stores the interface to the Baidu Netdisk files
type Fs struct {
	name     string
	root     string // rclone 相对根路径（不含首尾斜杠）
	baseSrv  string // 根目录的服务端绝对路径（来自 root 配置，原样使用）
	opt      Options
	m        configmap.Mapper // 配置回写通道（绑定真实配置段，见 refreshToken 注释）
	features *fs.Features
	srv      *rest.Client // pan.baidu.com 业务 API
	client   *http.Client // 下载直链与分片上传（跟随 302）
	pacer    *fs.Pacer
	vipType  int // 0 普通/1 会员/2 超会，决定上传分片规格
	tokenMu  sync.Mutex
}

// Object is a remote object that has been stat'd
type Object struct {
	fs      *Fs
	remote  string // rclone 相对路径
	srvPath string // 服务端绝对路径（服务端操作用）
	fsID    int64  // 下载用
	size    int64
	modTime time.Time
}

// Config is the state machine for `rclone config`，支持两种授权路径：
// oob（默认，控制台零配置，headless 友好）与本机 53682 端口回调
// （需先在控制台登记 http://localhost:53682/）。
func Config(ctx context.Context, name string, m configmap.Mapper, conf fs.ConfigIn) (*fs.ConfigOut, error) {
	switch conf.State {
	case "":
		return fs.ConfigChooseExclusiveFixed("auth_method", "config_auth_method", "Select the authorization method", []fs.OptionExample{{
			Value: "oob",
			Help:  "Open a URL in a browser and paste the authorization code it shows (no console configuration needed)",
		}, {
			Value: "callback",
			Help:  "Run a temporary web server on http://localhost:53682/ and wait for the redirect (register http://localhost:53682/ as a redirect URI in the console first)",
		}})
	case "auth_method":
		appKey, _ := m.Get("app_key")
		appSecret, _ := m.Get("app_secret")
		if appKey == "" || appSecret == "" {
			return nil, errors.New("app_key and app_secret must be set before authorizing")
		}
		switch conf.Result {
		case "oob":
			return fs.ConfigInput("oob_code", "config_oob_code", fmt.Sprintf(`Open the following URL in a browser, sign in with the Baidu account that owns the files and approve the application:

%s

Then paste the authorization code shown on the resulting page below. The code is single-use and valid for 10 minutes.`, authorizeURL(appKey, "oob")))
		case "callback":
			if _, err := startCallbackServer(name); err != nil {
				return nil, err
			}
			return fs.ConfigInputOptional("callback_code", "config_callback_code", fmt.Sprintf(`Open the following URL in a browser, sign in with the Baidu account that owns the files and approve the application:

%s

After approval the browser redirects to http://localhost:53682/ and rclone captures the code automatically. If you are configuring rclone on a remote machine, forward the port first, for example:

    ssh -L 53682:localhost:53682 <remote>

Press Enter once the browser shows the success page. If the redirect fails, paste the full URL from the browser address bar instead (it contains the code).`, authorizeURL(appKey, callbackURL)))
		default:
			return nil, fmt.Errorf("unknown authorization method %q", conf.Result)
		}
	case "oob_code":
		appKey, _ := m.Get("app_key")
		appSecret, _ := m.Get("app_secret")
		res, err := fetchToken(ctx, configClient(ctx), url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {conf.Result},
			"client_id":     {appKey},
			"client_secret": {appSecret},
			"redirect_uri":  {"oob"},
		})
		if err != nil {
			return fs.ConfigError("oob_code", "Token exchange failed: "+err.Error())
		}
		m.Set("access_token", res.AccessToken)
		m.Set("refresh_token", res.RefreshToken)
		return nil, nil
	case "callback_code":
		appKey, _ := m.Get("app_key")
		appSecret, _ := m.Get("app_secret")
		code := ""
		if v, ok := callbackServers.Load(name); ok {
			st := v.(*callbackState)
			if msg := st.getError(); msg != "" {
				return fs.ConfigError("callback_code", "Authorization failed: "+msg)
			}
			code = st.getCode()
		}
		if code == "" {
			code = extractCode(conf.Result)
		}
		if code == "" {
			return fs.ConfigError("callback_code", "No authorization code received yet. Finish the authorization in your browser and press Enter again, or paste the full URL the browser redirected to.")
		}
		res, err := fetchToken(ctx, configClient(ctx), url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {code},
			"client_id":     {appKey},
			"client_secret": {appSecret},
			"redirect_uri":  {callbackURL},
		})
		if err != nil {
			return fs.ConfigError("callback_code", "Token exchange failed: "+err.Error())
		}
		stopCallbackServer(name)
		m.Set("access_token", res.AccessToken)
		m.Set("refresh_token", res.RefreshToken)
		return nil, nil
	}
	return nil, fmt.Errorf("unknown state %q", conf.State)
}

// authorizeURL 构造 OAuth 授权页 URL。
func authorizeURL(appKey, redirectURI string) string {
	return fmt.Sprintf("https://openapi.baidu.com/oauth/2.0/authorize?response_type=code&client_id=%s&redirect_uri=%s&scope=basic,netdisk",
		url.QueryEscape(appKey), url.QueryEscape(redirectURI))
}

// configClient 为向导阶段的 token 请求构造独立 client。
func configClient(ctx context.Context) *rest.Client {
	return rest.NewClient(fshttp.NewClient(ctx)).SetRoot(tokenBaseURL)
}

// callbackServers 记录每个 remote 的回调监听实例，
// 使服务在向导两次状态调用之间保持存活。
var callbackServers sync.Map

// callbackState 是本机回调监听的状态。
type callbackState struct {
	mu     sync.Mutex
	code   string
	errMsg string
	srv    *http.Server
}

func (s *callbackState) getCode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.code
}

func (s *callbackState) getError() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.errMsg
}

func (s *callbackState) setCode(code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.code = code
}

func (s *callbackState) setError(errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errMsg = errMsg
}

// startCallbackServer 在 127.0.0.1:53682 起临时 HTTP 服务等待授权回调，
// 5 分钟超时自动关闭，防止遗留监听进程。
func startCallbackServer(name string) (*callbackState, error) {
	stopCallbackServer(name)
	st := &callbackState{}
	ln, err := net.Listen("tcp", callbackAddr)
	if err != nil {
		return nil, fmt.Errorf("can't listen on %s: %w - use the oob authorization method instead", callbackAddr, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			st.setError(e + ": " + q.Get("error_description"))
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "Authorization failed: "+e)
			return
		}
		if c := q.Get("code"); c != "" {
			st.setCode(c)
			_, _ = io.WriteString(w, "Authorization complete. You can close this tab and return to rclone.")
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "no authorization code in request")
	})
	st.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		_ = st.srv.Serve(ln)
	}()
	time.AfterFunc(callbackTimeout, func() { stopCallbackServer(name) })
	callbackServers.Store(name, st)
	return st, nil
}

// stopCallbackServer 关闭并移除回调监听实例（不存在时为空操作）。
func stopCallbackServer(name string) {
	if v, ok := callbackServers.LoadAndDelete(name); ok {
		_ = v.(*callbackState).srv.Close()
	}
}

// NewFs creates a new Fs object from the name and root
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	opt := new(Options)
	if err := configstruct.Set(m, opt); err != nil {
		return nil, err
	}
	if opt.AppKey == "" {
		return nil, errors.New("baidupcs: app_key is required - create an application in the Baidu Netdisk open platform console")
	}
	if opt.AppSecret == "" {
		return nil, errors.New("baidupcs: app_secret is required")
	}
	if opt.RefreshToken == "" {
		return nil, errors.New("baidupcs: refresh_token is required - run `rclone config` to set up this remote")
	}
	opt.Root = normalizeRootPath(opt.Root)
	opt.UploadConcurrency = clampInt(opt.UploadConcurrency, 1, 32)

	f := &Fs{
		name:    name,
		root:    strings.Trim(root, "/"),
		baseSrv: opt.Root,
		opt:     *opt,
		m:       m,
	}
	f.srv = rest.NewClient(fshttp.NewClient(ctx)).SetRoot(apiBaseURL)
	// 非 2xx 的响应包装为带状态码的 httpError，供 pacer 判断 429/5xx 重试
	f.srv.SetErrorHandler(func(resp *http.Response) error {
		body, _ := rest.ReadBody(resp)
		return &httpError{
			StatusCode: resp.StatusCode,
			err:        fmt.Errorf("HTTP error %d (%s) returned body: %q", resp.StatusCode, resp.Status, body),
		}
	})
	f.client = fshttp.NewClient(ctx)
	f.pacer = fs.NewPacer(ctx, pacer.NewDefault(pacer.MinSleep(minSleep), pacer.MaxSleep(maxSleep)))
	f.features = (&fs.Features{
		CanHaveEmptyDirectories: true,
	}).Fill(ctx, f)

	// access_token 为空时先刷新一次（向导可能只存了 refresh_token）
	if f.opt.AccessToken == "" {
		if err := f.refreshToken(ctx, f.opt.RefreshToken); err != nil {
			return nil, fmt.Errorf("baidupcs: %w", err)
		}
	}
	// uinfo 验证 token 并取会员类型（决定上传分片规格）
	if err := f.userInfo(ctx); err != nil {
		return nil, err
	}
	if err := f.initRoot(ctx); err != nil {
		if !errors.Is(err, fs.ErrorIsFile) {
			return nil, err
		}
		// 根指向文件：按 rclone 约定返回指向父目录的 Fs + ErrorIsFile
		return f, err
	}
	return f, nil
}

// currentAccessToken 加锁读取当前 access token，避免与刷新轮换的写入竞争。
func (f *Fs) currentAccessToken() string {
	f.tokenMu.Lock()
	defer f.tokenMu.Unlock()
	return f.opt.AccessToken
}

// currentRefreshToken 加锁读取当前 refresh token。
func (f *Fs) currentRefreshToken() string {
	f.tokenMu.Lock()
	defer f.tokenMu.Unlock()
	return f.opt.RefreshToken
}

// refreshToken 轮换 access_token/refresh_token 并回写配置文件。
// refresh_token 单次使用且失败即作废，必须串行执行；usedRefreshToken
// 用于识别"其他请求已完成轮换"的情况，避免用已消费的 token 重复刷新
// （并发双刷会互相作废，这是不用 oauthutil 的原因）。
func (f *Fs) refreshToken(ctx context.Context, usedRefreshToken string) error {
	f.tokenMu.Lock()
	defer f.tokenMu.Unlock()
	if f.opt.RefreshToken != usedRefreshToken {
		// 另一个请求已经完成轮换，直接用新 token 重试原请求即可
		return nil
	}
	res, err := fetchToken(ctx, f.srv, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {usedRefreshToken},
		"client_id":     {f.opt.AppKey},
		"client_secret": {f.opt.AppSecret},
	})
	if err != nil {
		return fmt.Errorf("token refresh failed: %w - if this persists, re-run `rclone config` to re-authorize (a failed refresh invalidates the old refresh_token)", err)
	}
	f.opt.AccessToken = res.AccessToken
	f.opt.RefreshToken = res.RefreshToken
	// 回写配置，避免重启后使用已轮换的旧 token。必须走 NewFs 传入的
	// configmap 而不是 fs.ConfigFileSet(f.name, ...)：环境变量覆盖选项时
	// fs.NewFs 会把 f.name 改成带 {hash} 后缀的缓存名，直接按名写会写进
	// 错误的配置段；而 configmap 的 setter 在改名前就已绑定真实配置段
	f.m.Set("access_token", res.AccessToken)
	f.m.Set("refresh_token", res.RefreshToken)
	return nil
}

// initRoot 确保 Fs 根目录可用：配置根（rclone root 为空）不存在时自动
// 创建（幂等，全新应用首次配置即可用）；子目录路径不自动创建，由
// Mkdir/Update 按需建，否则 rclone 的 Rmdir-不存在应报错的语义会被
// 破坏。根指向文件时退到父目录并返回 fs.ErrorIsFile。
// 实测百度 list 对指向文件的 dir 参数返回 errno 0 加空列表，与空目录
// 无法区分，因此空列表时必须查父目录才能判定。
func (f *Fs) initRoot(ctx context.Context) error {
	rootSrv := f.srvPath("")
	entries, err := f.listPage(ctx, rootSrv, 0, 1)
	if err != nil && !errors.Is(err, fs.ErrorDirNotFound) {
		return err
	}
	if err == nil && len(entries) > 0 {
		// 非空目录，确认存在
		return nil
	}
	parentSrv, leafSrv := splitServerPath(rootSrv)
	if parentSrv == rootSrv {
		// 服务端根目录 "/" 本身
		return nil
	}
	parentFiles, err := f.listAll(ctx, parentSrv)
	if err != nil {
		if errors.Is(err, fs.ErrorDirNotFound) {
			// 父目录不存在
			if f.root == "" {
				return f.mkdirServer(ctx, rootSrv)
			}
			return nil
		}
		return err
	}
	for i := range parentFiles {
		if parentFiles[i].serverName() == leafSrv {
			if parentFiles[i].Isdir == 1 {
				// 空目录
				return nil
			}
			return f.retreatRootToFile(ctx, leafSrv)
		}
	}
	// 父目录中无此名字：不存在
	if f.root == "" {
		return f.mkdirServer(ctx, rootSrv)
	}
	return nil
}

// retreatRootToFile 把根退到父目录以容纳单文件 Object（rclone 约定）。
func (f *Fs) retreatRootToFile(ctx context.Context, leafSrv string) error {
	if f.root != "" {
		f.root, _ = splitRemotePath(f.root)
	} else {
		f.baseSrv, _ = splitServerPath(f.baseSrv)
	}
	leaf := f.opt.Enc.ToStandardName(leafSrv)
	if _, err := f.NewObject(ctx, leaf); err != nil {
		if errors.Is(err, fs.ErrorObjectNotFound) {
			// 竞态：文件刚被删除，当作普通根目录处理
			return nil
		}
		return err
	}
	return fs.ErrorIsFile
}

// srvPath 把 rclone 相对路径转换为服务端绝对路径。
// baseSrv 来自 root 配置，本身就是服务端路径，原样使用；
// root 与 remote 的每一段做编码转换。
func (f *Fs) srvPath(remote string) string {
	p := path.Join(f.baseSrv, f.encodeRemotePath(f.root), f.encodeRemotePath(remote))
	if p == "" || p == "." {
		return "/"
	}
	return p
}

// encodeRemotePath 逐段编码 rclone 相对路径。
func (f *Fs) encodeRemotePath(remote string) string {
	if remote == "" {
		return ""
	}
	segs := strings.Split(remote, "/")
	for i, seg := range segs {
		if seg == "" {
			continue
		}
		segs[i] = f.opt.Enc.FromStandardName(seg)
	}
	return path.Join(segs...)
}

// fileStandardName 返回条目的 rclone 标准名字。
func (f *Fs) fileStandardName(file *apiFile) string {
	return f.opt.Enc.ToStandardName(file.serverName())
}

// fileToObject 把服务端条目转换为 Object。
func (f *Fs) fileToObject(remote string, file *apiFile) *Object {
	return &Object{
		fs:      f,
		remote:  remote,
		srvPath: file.Path,
		fsID:    file.FsID,
		size:    file.Size,
		modTime: fileModTime(file),
	}
}

// List the objects and directories in dir into entries
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	files, err := f.listAll(ctx, f.srvPath(dir))
	if err != nil {
		return nil, err
	}
	for i := range files {
		remote := path.Join(dir, f.fileStandardName(&files[i]))
		if files[i].Isdir == 1 {
			entries = append(entries, fs.NewDir(remote, fileModTime(&files[i])))
			continue
		}
		entries = append(entries, f.fileToObject(remote, &files[i]))
	}
	return entries, nil
}

// NewObject finds the Object at remote
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	parent, leaf := splitRemotePath(remote)
	if leaf == "" {
		return nil, fs.ErrorObjectNotFound
	}
	files, err := f.listAll(ctx, f.srvPath(parent))
	if err != nil {
		if errors.Is(err, fs.ErrorDirNotFound) {
			return nil, fs.ErrorObjectNotFound
		}
		return nil, err
	}
	for i := range files {
		if files[i].Isdir == 0 && f.fileStandardName(&files[i]) == leaf {
			return f.fileToObject(remote, &files[i]), nil
		}
	}
	return nil, fs.ErrorObjectNotFound
}

// Mkdir creates the container if it doesn't exist
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	return f.mkdirServer(ctx, f.srvPath(dir))
}

// mkdirServer 按服务端路径创建目录。
// 百度 create 不会自动补齐父目录，必须逐段创建；但对已存在的路径调用
// create 会返回 errno 0 并额外创建一个空的 {name}_{timestamp} 副本
// （实测行为），因此必须先向上探测已存在的前缀，只对缺失的段调用
// create。已存在的完整路径不触发任何 create，满足 rclone 对 Mkdir
// 的幂等要求。
func (f *Fs) mkdirServer(ctx context.Context, srvPath string) error {
	if srvPath == "" || srvPath == "/" {
		return nil
	}
	// 从完整路径向上找到最深的存在前缀，missing 自底向上收集
	var missing []string
	cur := srvPath
	for {
		if _, err := f.listPage(ctx, cur, 0, 1); err == nil {
			// 存在（目录或文件；若是文件，后续 create 会失败并报错）
			break
		} else if !errors.Is(err, fs.ErrorDirNotFound) {
			return err
		}
		parent, leaf := splitServerPath(cur)
		if parent == cur {
			// 到服务端根仍未找到，从根开始建
			break
		}
		missing = append(missing, leaf)
		cur = parent
	}
	// 自顶向下逐段创建缺失部分
	for i := len(missing) - 1; i >= 0; i-- {
		cur = cur + "/" + missing[i]
		_, err := f.createEntry(ctx, cur, 0, 1, "", "", 0)
		if err != nil {
			// 并发下已被其他请求创建视为成功
			if errno := asErrno(err); errno == errnoFileExists || errno == errnoAlreadyExists {
				continue
			}
			return err
		}
	}
	return nil
}

// Rmdir removes the directory if it is empty
//
// 百度 delete 会递归删除非空目录，直接调用会破坏 rclone 的空目录
// 删除语义，因此必须先 list 确认为空。
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	srvPath := f.srvPath(dir)
	files, err := f.listAll(ctx, srvPath)
	if err != nil {
		return err
	}
	if len(files) > 0 {
		return fmt.Errorf("baidupcs: directory %q is not empty", dir)
	}
	return f.fileManager(ctx, "delete", []string{srvPath})
}

// Remove an object
func (o *Object) Remove(ctx context.Context) error {
	return o.fs.fileManager(ctx, "delete", []string{o.srvPath})
}

// moveOrCopy 服务端移动/复制：文件与目录同一接口。
func (f *Fs) moveOrCopy(ctx context.Context, opera, srcSrvPath, dstParentSrvPath, dstLeafSrv string) error {
	return f.fileManager(ctx, opera, []fileManagerEntry{{
		Path:    srcSrvPath,
		Dest:    dstParentSrvPath,
		Newname: dstLeafSrv,
	}})
}

// Move src to this remote using server-side move operations
func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (dst fs.Object, err error) {
	srcObj, ok := src.(*Object)
	if !ok {
		fs.Debugf(src, "Can't move - not same remote type")
		return nil, fs.ErrorCantMove
	}
	dstParent, dstLeaf := splitRemotePath(remote)
	if err := f.moveOrCopy(ctx, "move", srcObj.srvPath, f.srvPath(dstParent), f.opt.Enc.FromStandardName(dstLeaf)); err != nil {
		return nil, err
	}
	srcObj.remote = remote
	srcObj.srvPath = f.srvPath(remote)
	return srcObj, nil
}

// Copy src to this remote using server-side copy operations
func (f *Fs) Copy(ctx context.Context, src fs.Object, remote string) (dst fs.Object, err error) {
	srcObj, ok := src.(*Object)
	if !ok {
		fs.Debugf(src, "Can't copy - not same remote type")
		return nil, fs.ErrorCantCopy
	}
	dstParent, dstLeaf := splitRemotePath(remote)
	if err := f.moveOrCopy(ctx, "copy", srcObj.srvPath, f.srvPath(dstParent), f.opt.Enc.FromStandardName(dstLeaf)); err != nil {
		return nil, err
	}
	// 服务端副本存在短暂可见性延迟，重读元数据时重试几次
	var copied fs.Object
	for range 3 {
		copied, err = f.NewObject(ctx, remote)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrorObjectNotFound) {
			return nil, fmt.Errorf("failed to locate copied file: %w", err)
		}
		if err := sleepContext(ctx, 500*time.Millisecond); err != nil {
			return nil, err
		}
	}
	if err != nil {
		return nil, fmt.Errorf("failed to locate copied file: %w", err)
	}
	return copied, nil
}

// DirMove moves src, srcRemote to this remote at dstRemote using server-side
// move operations
//
// 目标路径必须按服务端全路径拆父目录与末段：dstRemote 为空时（把源 Fs
// 的根整体改名为目标 Fs 的根，sync.MoveDir 的调用形态）按 rclone 相对
// 路径拆分会得到空末段，导致“移动进目标目录”而非改名。
// 目标已存在时返回 fs.ErrorDirExists，让上层回退到逐文件移动。
func (f *Fs) DirMove(ctx context.Context, src fs.Fs, srcRemote, dstRemote string) error {
	srcFs, ok := src.(*Fs)
	if !ok {
		fs.Debugf(src, "Can't move directory - not same remote type")
		return fs.ErrorCantDirMove
	}
	targetSrv := f.srvPath(dstRemote)
	parentSrv, leafSrv := splitServerPath(targetSrv)
	if leafSrv == "" {
		return fs.ErrorCantDirMove
	}
	// 目标父目录不存在时先补齐（filemanager move 不会自动创建目标路径，
	// fstests 的 FsDirMove 会把目录移到不存在的多级新路径下）
	if err := f.mkdirServer(ctx, parentSrv); err != nil {
		return err
	}
	// 预检目标是否已存在：直接调用 move 的话，百度在目标冲突时除了返回
	// -8 还会留下一个空的时间戳重命名目录（实测行为），必须提前拦下
	files, err := f.listAll(ctx, parentSrv)
	if err != nil {
		return err
	}
	for i := range files {
		if files[i].serverName() == leafSrv {
			return fs.ErrorDirExists
		}
	}
	err = f.moveOrCopy(ctx, "move", srcFs.srvPath(srcRemote), parentSrv, leafSrv)
	if err != nil {
		// 兜底预检与执行之间的竞态
		if asErrno(err) == errnoFileExists {
			return fs.ErrorDirExists
		}
		return err
	}
	return nil
}

// sleepContext 等待指定时长，可被 ctx 取消中断。
func sleepContext(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// About gets quota information
func (f *Fs) About(ctx context.Context) (usage *fs.Usage, err error) {
	total, used, err := f.quota(ctx)
	if err != nil {
		return nil, err
	}
	return &fs.Usage{
		Total: fs.NewUsageValue(total),
		Used:  fs.NewUsageValue(used),
		Free:  fs.NewUsageValue(total - used),
	}, nil
}

// getDlink 取下载直链。fs_id 不存在时服务端返回 errno 0 加空 list，
// 需判空映射为 fs.ErrorObjectNotFound。
func (o *Object) getDlink(ctx context.Context) (string, error) {
	files, err := o.fs.filemetas(ctx, o.fsID, true)
	if err != nil {
		if asErrno(err) == errnoFileNotExists {
			return "", fs.ErrorObjectNotFound
		}
		return "", err
	}
	if len(files) == 0 || files[0].Dlink == "" {
		return "", fs.ErrorObjectNotFound
	}
	return files[0].Dlink, nil
}

// Open an object for read
//
// 链路：filemetas 取 dlink → 拼 access_token → 带 UA pan.baidu.com 的
// GET（跟随 302 到最终直链）。大于约 20MB 的文件不带该 UA 会失败，
// 因此 UA 是硬性要求。直链 8 小时有效，403/410/429/5xx 时经 pacer
// 重试并重新取 dlink。
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (in io.ReadCloser, err error) {
	if o.fsID == 0 {
		return nil, errors.New("baidupcs: can't download - no fs_id")
	}
	fs.FixRangeOption(options, o.size)
	// Range 起点越过文件末尾时直接返回空内容，与 local backend 行为一致
	// （FixRangeOption 只钳制 End 不钳制 Start，直接发请求会得到 416）
	for _, option := range options {
		if ro, ok := option.(*fs.RangeOption); ok && ro.Start >= o.size {
			return io.NopCloser(strings.NewReader("")), nil
		}
	}
	var res *http.Response
	err = o.fs.pacer.Call(func() (bool, error) {
		dlink, err := o.getDlink(ctx)
		if err != nil {
			// 对象消失不可重试；其余错误按标准规则判断
			return !errors.Is(err, fs.ErrorObjectNotFound) && shouldRetry(err), err
		}
		// dlink 自带 query 参数，追加 token 即可
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, dlink+"&access_token="+url.QueryEscape(o.fs.currentAccessToken()), nil)
		if err != nil {
			return false, err
		}
		req.Header.Set("User-Agent", baiduUA)
		fs.OpenOptionAddHTTPHeaders(req.Header, options)
		res, err = o.fs.client.Do(req)
		if err != nil {
			return fserrors.ShouldRetry(err), err
		}
		if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
			statusCode := res.StatusCode
			_ = res.Body.Close()
			res = nil
			// 直链过期（403/410）与限流（429）/服务端错误（5xx）重取 dlink 重试
			return statusCode == http.StatusForbidden ||
					statusCode == http.StatusGone ||
					statusCode == http.StatusTooManyRequests ||
					statusCode >= 500,
				fmt.Errorf("baidupcs: download failed: HTTP status %d", statusCode)
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}

// Features returns the optional features of this Fs
func (f *Fs) Features() *fs.Features {
	return f.features
}

// Name of the remote (as passed into NewFs)
func (f *Fs) Name() string {
	return f.name
}

// Root of the remote (as passed into NewFs)
func (f *Fs) Root() string {
	return f.root
}

// String converts this Fs to a string
func (f *Fs) String() string {
	return fmt.Sprintf("Baidu Netdisk root '%s'", f.srvPath(""))
}

// Precision return the precision of this Fs
func (f *Fs) Precision() time.Duration {
	return fs.ModTimeNotSupported
}

// Hashes returns the supported hash sets
//
// 百度列表返回的 md5 字段不可信（OpenList 明确不使用），不提供 hash。
func (f *Fs) Hashes() hash.Set {
	return hash.Set(hash.None)
}

// Remote returns the remote path of the object
func (o *Object) Remote() string {
	return o.remote
}

// ModTime returns the modification time of the object
func (o *Object) ModTime(ctx context.Context) time.Time {
	return o.modTime
}

// SetModTime sets the modification time of the object
//
// 百度网盘不支持修改已有文件的时间（仅上传时可携带 local_mtime）。
func (o *Object) SetModTime(ctx context.Context, modTime time.Time) error {
	return fs.ErrorCantSetModTime
}

// Size returns the size of the object
func (o *Object) Size() int64 {
	return o.size
}

// Fs returns the Fs of the object
func (o *Object) Fs() fs.Info {
	return o.fs
}

// Hash returns the hash of the object - Baidu Netdisk provides no usable hashes
func (o *Object) Hash(ctx context.Context, r hash.Type) (string, error) {
	return "", hash.ErrUnsupported
}

// Storable returns whether the object is storable
func (o *Object) Storable() bool {
	return true
}

// String converts this object to a string
func (o *Object) String() string {
	if o == nil {
		return "<nil>"
	}
	return o.remote
}
