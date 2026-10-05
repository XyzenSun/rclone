// API 请求封装：rest client 统一入口（access_token 注入、errno 解析、
// token 失效刷新重试）以及各端点的请求/响应类型。
// 端点与参数形态参考 OpenList baidu_netdisk 驱动的长期验证行为。

package baidupcs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/lib/rest"
)

// 域名与端点常量。业务 API 统一挂在 pan.baidu.com 下，
// 上传数据面在 d.pcs.baidu.com（可被 locateupload 探测的动态域名替换）。
const (
	apiBaseURL       = "https://pan.baidu.com"
	tokenBaseURL     = "https://openapi.baidu.com"
	defaultUploadURL = "https://d.pcs.baidu.com"
	superfile2Path   = "/rest/2.0/pcs/superfile2"
	locateUploadPath = "/rest/2.0/pcs/file"
	baiduUA          = "pan.baidu.com"
	listPageSize     = 1000
	callbackAddr     = "127.0.0.1:53682"
	callbackURL      = "http://localhost:53682/"
	callbackTimeout  = 5 * time.Minute
)

// 业务错误码。语义来源为官方错误码文档与 OpenList 驱动经验。
const (
	errnoFileExists    = -8    // 文件或目录已存在
	errnoAlreadyExists = 102   // 文件或目录已存在（系统目录变体）
	errnoNotFound      = -9    // 文件或目录不存在
	errnoNameInvalid   = -7    // 文件名非法或无权访问
	errnoTokenInvalid  = -6    // access_token 无效
	errnoTokenExpired  = 111   // access_token 过期或无效
	errnoUserLimit     = 20011 // 用户数超限
	errnoRateLimited   = 20012 // 请求频率超限
	errnoNoPermission  = 20013 // 无权限访问该文件
	errnoTokenInvalid2 = 20017 // access_token 为空或格式错误
	errnoFileNotExists = 31023 // 文件不存在
)

// apiError 携带百度业务错误码，调用方用 asErrno 据此映射 rclone 标准错误。
type apiError struct {
	Op    string
	Errno int
}

// Error 满足 error 接口，附上官方错误码文档入口便于排查。
func (e *apiError) Error() string {
	return fmt.Sprintf("baidupcs: %s failed with errno %d (%s) - see https://pan.baidu.com/union/doc/", e.Op, e.Errno, errnoText(e.Errno))
}

// asErrno 从错误链中提取业务错误码，非业务错误返回 0。
func asErrno(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Errno
	}
	return 0
}

// httpError 包装非 2xx 的 HTTP 响应，携带状态码供重试判断。
type httpError struct {
	StatusCode int
	err        error
}

// Error 满足 error 接口。
func (e *httpError) Error() string {
	return e.err.Error()
}

// Unwrap 暴露底层错误。
func (e *httpError) Unwrap() error {
	return e.err
}

// shouldRetry 综合判断错误是否可重试：网络层临时错误、429/5xx、
// 以及 errno 20012（频率超限）。
func shouldRetry(err error) bool {
	var he *httpError
	if errors.As(err, &he) {
		return he.StatusCode == http.StatusTooManyRequests || he.StatusCode >= 500
	}
	return fserrors.ShouldRetry(err)
}

// apiEnvelope 是所有业务端点响应的公共信封。
type apiEnvelope struct {
	Errno int `json:"errno"`
}

// hasErrno 由嵌入 apiEnvelope 的响应类型自动满足，供 callAPI 统一读取 errno。
type hasErrno interface {
	APIErrno() int
}

// APIErrno 实现 hasErrno。
func (e apiEnvelope) APIErrno() int {
	return e.Errno
}

// apiFile 是 list/filemetas/create/precreate 返回的文件元数据。
// 不同端点返回的字段子集不同，共用一个宽松结构。
type apiFile struct {
	FsID           int64  `json:"fs_id"`
	Path           string `json:"path"`
	ServerFilename string `json:"server_filename"`
	Isdir          int    `json:"isdir"`
	Size           int64  `json:"size"`
	ServerCtime    int64  `json:"server_ctime"`
	ServerMtime    int64  `json:"server_mtime"`
	LocalCtime     int64  `json:"local_ctime"`
	LocalMtime     int64  `json:"local_mtime"`
	// create/precreate 响应使用 ctime/mtime 字段，且恒为当前时间
	// （OpenList 已踩坑，调用方必须用本地时间覆盖）
	Ctime int64  `json:"ctime"`
	Mtime int64  `json:"mtime"`
	Dlink string `json:"dlink"`
}

// serverName 返回服务端空间的名字（不做编码转换）。
func (file *apiFile) serverName() string {
	if file.ServerFilename != "" {
		return file.ServerFilename
	}
	return path.Base(file.Path)
}

// fileModTime 解析服务端时间。优先 local_mtime：官方客户端上传时它是
// 真实修改时间（server_mtime 是上传时间），且 filemanager move 会把
// server_mtime 重置为移动时间而 local_mtime 保留；两者都缺失时逐级
// 回退创建时间，全部缺失返回零值。
func fileModTime(file *apiFile) time.Time {
	for _, ts := range []int64{file.LocalMtime, file.ServerMtime, file.LocalCtime, file.ServerCtime, file.Mtime, file.Ctime} {
		if ts != 0 {
			return time.Unix(ts, 0)
		}
	}
	return time.Time{}
}

type listResponse struct {
	apiEnvelope
	List []apiFile `json:"list"`
}

type filemetasResponse struct {
	apiEnvelope
	List []apiFile `json:"list"`
}

type uinfoResponse struct {
	apiEnvelope
	Uk      int64 `json:"uk"`
	VipType int   `json:"vip_type"`
}

type quotaResponse struct {
	apiEnvelope
	Total int64 `json:"total"`
	Used  int64 `json:"used"`
}

type precreateResponse struct {
	apiEnvelope
	ReturnType int     `json:"return_type"`
	Uploadid   string  `json:"uploadid"`
	BlockList  []int   `json:"block_list"`
	File       apiFile `json:"info"`
}

// createResponse 的文件字段直接平铺在顶层，嵌入 apiFile 复用解码。
type createResponse struct {
	apiEnvelope
	apiFile
}

type fileManagerInfo struct {
	Path  string `json:"path"`
	Errno int    `json:"errno"`
}

type fileManagerResponse struct {
	apiEnvelope
	Info []fileManagerInfo `json:"info"`
}

type uploadServerResponse struct {
	apiEnvelope
	Servers []struct {
		Server string `json:"server"`
	} `json:"servers"`
	BakServers []struct {
		Server string `json:"server"`
	} `json:"bak_servers"`
}

// superfile2Response 是分片上传端点的响应，错误时用 error_code 而非 errno。
type superfile2Response struct {
	Errno     int    `json:"errno"`
	ErrorCode int    `json:"error_code"`
	ErrorMsg  string `json:"error_msg"`
	Md5       string `json:"md5"`
}

// tokenResponse 是 OAuth token 端点的响应（授权码与刷新两种 grant 共用）。
type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int    `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// callAPI 是所有 pan.baidu.com 业务端点的统一入口：
// 注入 access_token（百度约定走 query 参数而非 Authorization header）、
// 解析 errno、在 token 失效（111/-6）时串行刷新并重试。
// build 在每次重试时重新构造 Opts，保证 Body 等一次性资源可重复使用。
func (f *Fs) callAPI(ctx context.Context, op string, build func() *rest.Opts, result any) error {
	usedRefreshToken := f.currentRefreshToken()
	refreshed := false
	return f.pacer.Call(func() (bool, error) {
		opts := build()
		if opts.Parameters == nil {
			opts.Parameters = url.Values{}
		}
		// 百度网盘开放平台的 token 走 query 参数而非 Authorization header；
		// 每次尝试都读最新值，刷新成功后的重试会带上新 token
		opts.Parameters.Set("access_token", f.currentAccessToken())
		resp, err := f.srv.Call(ctx, opts)
		if err != nil {
			// HTTP/网络层错误（含经 errorHandler 包装的 4xx/5xx）
			return shouldRetry(err), fmt.Errorf("baidupcs: %s: %w", op, err)
		}
		err = json.NewDecoder(resp.Body).Decode(result)
		closeErr := resp.Body.Close()
		if err != nil {
			return false, fmt.Errorf("baidupcs: %s: failed to decode response: %w", op, err)
		}
		if closeErr != nil {
			return false, fmt.Errorf("baidupcs: %s: failed to close response body: %w", op, closeErr)
		}
		errno := 0
		if h, ok := result.(hasErrno); ok {
			errno = h.APIErrno()
		}
		if errno == 0 {
			return false, nil
		}
		if (errno == errnoTokenExpired || errno == errnoTokenInvalid || errno == errnoTokenInvalid2) && !refreshed {
			// token 失效只刷新一次：新 token 仍失效说明凭据或权限有问题，
			// 反复轮换只会白白消耗 refresh_token（单次使用且失败即作废）
			refreshed = true
			if err := f.refreshToken(ctx, usedRefreshToken); err != nil {
				return false, fmt.Errorf("baidupcs: %s: %w", op, err)
			}
			usedRefreshToken = f.currentRefreshToken()
			return true, &apiError{Op: op, Errno: errno}
		}
		return errnoRetryable(errno), &apiError{Op: op, Errno: errno}
	})
}

// listPage 拉取一页目录列表。order=name 固定升序，保证分页期间排序稳定。
// errno -9 映射为 fs.ErrorDirNotFound。
func (f *Fs) listPage(ctx context.Context, dir string, start, limit int) ([]apiFile, error) {
	var res listResponse
	err := f.callAPI(ctx, "list", func() *rest.Opts {
		return &rest.Opts{
			Method: http.MethodGet,
			Path:   "/rest/2.0/xpan/file",
			Parameters: url.Values{
				"method": {"list"},
				"dir":    {dir},
				"start":  {strconv.Itoa(start)},
				"limit":  {strconv.Itoa(limit)},
				"order":  {"name"},
				"web":    {"web"},
			},
		}
	}, &res)
	if err != nil {
		if asErrno(err) == errnoNotFound {
			return nil, fs.ErrorDirNotFound
		}
		return nil, err
	}
	return res.List, nil
}

// listAll 分页拉取完整目录列表。
func (f *Fs) listAll(ctx context.Context, dir string) ([]apiFile, error) {
	var files []apiFile
	start := 0
	for {
		page, err := f.listPage(ctx, dir, start, listPageSize)
		if err != nil {
			return nil, err
		}
		files = append(files, page...)
		if len(page) < listPageSize {
			break
		}
		start += listPageSize
	}
	return files, nil
}

// filemetas 按 fs_id 取文件元数据（含下载直链 dlink）。
// fs_id 不存在时服务端返回 errno 0 加空 list，必须判空。
func (f *Fs) filemetas(ctx context.Context, fsID int64, dlink bool) ([]apiFile, error) {
	var res filemetasResponse
	params := url.Values{
		"method": {"filemetas"},
		"fsids":  {fmt.Sprintf("[%d]", fsID)},
	}
	if dlink {
		params.Set("dlink", "1")
	}
	err := f.callAPI(ctx, "filemetas", func() *rest.Opts {
		return &rest.Opts{
			Method:     http.MethodGet,
			Path:       "/rest/2.0/xpan/multimedia",
			Parameters: params,
		}
	}, &res)
	if err != nil {
		return nil, err
	}
	return res.List, nil
}

// userInfo 验证 token 并取会员类型（决定上传分片规格）。
func (f *Fs) userInfo(ctx context.Context) error {
	var res uinfoResponse
	err := f.callAPI(ctx, "uinfo", func() *rest.Opts {
		return &rest.Opts{
			Method:     http.MethodGet,
			Path:       "/rest/2.0/xpan/nas",
			Parameters: url.Values{"method": {"uinfo"}},
		}
	}, &res)
	if err != nil {
		return err
	}
	f.vipType = res.VipType
	return nil
}

// quota 取空间总量与已用量。
func (f *Fs) quota(ctx context.Context) (total, used int64, err error) {
	var res quotaResponse
	err = f.callAPI(ctx, "quota", func() *rest.Opts {
		return &rest.Opts{
			Method:     http.MethodGet,
			Path:       "/api/quota",
			Parameters: url.Values{"checkfree": {"1"}, "checkexpire": {"1"}},
		}
	}, &res)
	if err != nil {
		return 0, 0, err
	}
	return res.Total, res.Used, nil
}

// createEntry 调用 create 端点，三种用途共用：
// isdir=1 建目录；带 uploadid+block_list 完成上传落盘；
// 仅带 block_list（全文件 MD5）不带 uploadid 时为秒传。
func (f *Fs) createEntry(ctx context.Context, srvPath string, size int64, isdir int, uploadid, blockList string, mtime int64) (apiFile, error) {
	form := url.Values{
		"path":  {srvPath},
		"size":  {strconv.FormatInt(size, 10)},
		"isdir": {strconv.Itoa(isdir)},
	}
	if isdir == 0 {
		// rtype=3：同名覆盖。目录创建不传 rtype，避免误触发覆盖语义
		form.Set("rtype", "3")
		if mtime != 0 {
			form.Set("local_mtime", strconv.FormatInt(mtime, 10))
			form.Set("local_ctime", strconv.FormatInt(mtime, 10))
		}
	}
	if uploadid != "" {
		form.Set("uploadid", uploadid)
	}
	if blockList != "" {
		form.Set("block_list", blockList)
	}
	var res createResponse
	err := f.callAPI(ctx, "create", func() *rest.Opts {
		return &rest.Opts{
			Method:      http.MethodPost,
			Path:        "/rest/2.0/xpan/file",
			Parameters:  url.Values{"method": {"create"}},
			ContentType: "application/x-www-form-urlencoded",
			Body:        strings.NewReader(form.Encode()),
		}
	}, &res)
	return res.apiFile, err
}

// precreate 执行预上传。contentMD5/sliceMD5 仅首次上传携带，
// uploadid 过期重传时省略（OpenList 验证的行为）。
func (f *Fs) precreate(ctx context.Context, srvPath string, size int64, blockList, contentMD5, sliceMD5 string, mtime int64) (*precreateResponse, error) {
	form := url.Values{
		"path":       {srvPath},
		"size":       {strconv.FormatInt(size, 10)},
		"isdir":      {"0"},
		"autoinit":   {"1"},
		"rtype":      {"3"},
		"block_list": {blockList},
	}
	if contentMD5 != "" && sliceMD5 != "" {
		form.Set("content-md5", contentMD5)
		form.Set("slice-md5", sliceMD5)
	}
	if mtime != 0 {
		form.Set("local_mtime", strconv.FormatInt(mtime, 10))
		form.Set("local_ctime", strconv.FormatInt(mtime, 10))
	}
	var res precreateResponse
	err := f.callAPI(ctx, "precreate", func() *rest.Opts {
		return &rest.Opts{
			Method:      http.MethodPost,
			Path:        "/rest/2.0/xpan/file",
			Parameters:  url.Values{"method": {"precreate"}},
			ContentType: "application/x-www-form-urlencoded",
			Body:        strings.NewReader(form.Encode()),
		}
	}, &res)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// fileManagerEntry 是 move/copy 的 filelist 条目。
type fileManagerEntry struct {
	Path    string `json:"path"`
	Dest    string `json:"dest,omitempty"`
	Newname string `json:"newname,omitempty"`
}

// fileManager 调用文件管理端点（move/copy/delete）。
// async=0 同步返回逐条结果；ondup=fail 让目标同名时直接失败，
// rclone 的覆盖语义由 operations 层预删除目标保证。
func (f *Fs) fileManager(ctx context.Context, opera string, filelist any) error {
	encoded, err := json.Marshal(filelist)
	if err != nil {
		return fmt.Errorf("baidupcs: failed to encode filelist: %w", err)
	}
	form := url.Values{
		"async":    {"0"},
		"filelist": {string(encoded)},
		"ondup":    {"fail"},
	}
	var res fileManagerResponse
	err = f.callAPI(ctx, "filemanager "+opera, func() *rest.Opts {
		return &rest.Opts{
			Method:      http.MethodPost,
			Path:        "/rest/2.0/xpan/file",
			Parameters:  url.Values{"method": {"filemanager"}, "opera": {opera}},
			ContentType: "application/x-www-form-urlencoded",
			Body:        strings.NewReader(form.Encode()),
		}
	}, &res)
	if err != nil {
		return err
	}
	for _, info := range res.Info {
		if info.Errno != 0 {
			return &apiError{Op: "filemanager " + opera, Errno: info.Errno}
		}
	}
	return nil
}

// locateUpload 探测动态上传域名，任何失败都回退默认域名 d.pcs.baidu.com。
// 该接口的失败不应导致上传中断。
func (f *Fs) locateUpload(ctx context.Context, srvPath, uploadid string) string {
	var res uploadServerResponse
	err := f.callAPI(ctx, "locateupload", func() *rest.Opts {
		return &rest.Opts{
			Method:  http.MethodGet,
			RootURL: defaultUploadURL,
			Path:    locateUploadPath,
			Parameters: url.Values{
				"method":         {"locateupload"},
				"appid":          {"250528"},
				"path":           {srvPath},
				"uploadid":       {uploadid},
				"upload_version": {"2.0"},
			},
		}
	}, &res)
	if err != nil {
		fs.Debugf(f, "locateupload failed, using default upload URL %s: %v", defaultUploadURL, err)
		return defaultUploadURL
	}
	for _, server := range res.Servers {
		if normalized := normalizeUploadServer(server.Server); normalized != "" {
			return normalized
		}
	}
	for _, server := range res.BakServers {
		if normalized := normalizeUploadServer(server.Server); normalized != "" {
			return normalized
		}
	}
	return defaultUploadURL
}

// fetchToken 调用 OAuth token 端点（授权码换 token 与刷新共用）。
// token 端点为 GET 语义，且不能携带 access_token 参数。
func fetchToken(ctx context.Context, client *rest.Client, params url.Values) (*tokenResponse, error) {
	var res tokenResponse
	resp, err := client.Call(ctx, &rest.Opts{
		Method:     http.MethodGet,
		RootURL:    tokenBaseURL,
		Path:       "/oauth/2.0/token",
		Parameters: params,
	})
	if err != nil {
		return nil, err
	}
	err = json.NewDecoder(resp.Body).Decode(&res)
	closeErr := resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("failed to decode token response: %w", err)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("failed to close token response body: %w", closeErr)
	}
	if res.Error != "" {
		return nil, fmt.Errorf("%s: %s", res.Error, res.ErrorDescription)
	}
	if res.AccessToken == "" || res.RefreshToken == "" {
		return nil, errors.New("empty token in response")
	}
	return &res, nil
}
