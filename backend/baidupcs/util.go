// 纯逻辑工具：分片规格推导、errno 分类与文案、路径拆分、授权码提取等。
// 本文件不触碰网络与 fs 状态，便于独立推理正确性。

package baidupcs

import (
	"io"
	"net/url"
	"path"
	"strings"
)

// 上传分片规格由会员类型决定：普通用户 4MB、普通会员 16MB、超级会员 32MB。
// 1024 片上限取官方文档口径（OpenList 实测 2048 可用，但按文档收敛；
// 1024x4MB 恰好等于普通用户 4GB 单文件上限，自洽）。
const (
	maxSliceNum = 1024
)

// sliceMD5Size 是 slice-md5 参数覆盖的字节数：文件前 256KB 的 MD5。
const sliceMD5Size = 256 * 1024

// sliceSizeForVip 按会员类型返回上传分片大小。
// 未知会员类型按普通用户处理，宁可分片偏小也不能超规格被服务端拒绝。
func sliceSizeForVip(vipType int) int64 {
	switch vipType {
	case 1:
		return 16 << 20
	case 2:
		return 32 << 20
	default:
		return 4 << 20
	}
}

// blockCount 计算分片数与末片大小。size 必须大于 0（空文件在入口已被拒绝）。
func blockCount(size, sliceSize int64) (count int, lastSize int64) {
	count = int((size + sliceSize - 1) / sliceSize)
	lastSize = size % sliceSize
	if lastSize == 0 {
		lastSize = sliceSize
	}
	return count, lastSize
}

// errnoRetryable 判断业务错误码是否值得重试。
// 20012 为请求频率超限，是唯一确认可退避重试的业务码；
// 权限类（20013）与配额类错误重试无意义，直接失败。
func errnoRetryable(errno int) bool {
	return errno == errnoRateLimited
}

// errnoText 返回 errno 的人类可读说明，未收录的码返回统一占位文案。
// 语义来源为官方错误码文档与 OpenList 驱动经验，未覆盖的码原样透出数字。
func errnoText(errno int) string {
	switch errno {
	case errnoFileExists:
		return "file already exists"
	case errnoAlreadyExists:
		return "file already exists"
	case errnoNotFound:
		return "file or directory not found"
	case errnoTokenInvalid:
		return "access token invalid"
	case errnoNameInvalid:
		return "file or directory name invalid, or no permission to access"
	case errnoTokenExpired:
		return "access token expired or invalid"
	case errnoUserLimit:
		return "user count over limit"
	case errnoRateLimited:
		return "request rate limit exceeded"
	case errnoNoPermission:
		return "no permission to access this file"
	case errnoTokenInvalid2:
		return "access token is empty or malformed"
	case errnoFileNotExists:
		return "file does not exist"
	}
	return "unrecognized error"
}

// splitRemotePath 把 rclone 相对路径拆为父目录与末段。
// 单段路径的父目录为 ""（即 Fs 根），与 path.Dir 对单段返回 "." 不同。
func splitRemotePath(p string) (dir, base string) {
	dir, base = path.Split(p)
	return strings.TrimSuffix(dir, "/"), base
}

// splitServerPath 把服务端绝对路径拆为父目录与末段。
// 根路径 "/" 的父目录是自身、末段为空，调用方需自行处理该边界。
func splitServerPath(p string) (parent, base string) {
	p = strings.TrimRight(p, "/")
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if p == "/" {
		return "/", ""
	}
	i := strings.LastIndexByte(p, '/')
	if i == 0 {
		return "/", p[1:]
	}
	return p[:i], p[i+1:]
}

// extractCode 从用户粘贴的内容中提取授权码。
// 支持三种形态：完整跳转 URL（含 code 查询参数）、仅查询串（code=xxx）、裸 code。
func extractCode(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if u, err := url.Parse(s); err == nil {
		if code := u.Query().Get("code"); code != "" {
			return code
		}
	}
	// 既不是 URL 也没有空白分隔时按裸 code 处理
	if !strings.ContainsAny(s, " \t\r\n") && !strings.Contains(s, "://") {
		return s
	}
	return ""
}

// normalizeUploadServer 规范化 locateupload 返回的域名：
// 无 scheme 时补 https://，去掉尾部斜杠，保证可以直接拼接路径。
func normalizeUploadServer(server string) string {
	server = strings.TrimSpace(server)
	server = strings.TrimSuffix(server, "/")
	if server == "" {
		return ""
	}
	if !strings.HasPrefix(server, "http://") && !strings.HasPrefix(server, "https://") {
		return "https://" + server
	}
	return server
}

// isUploadIDExpiredMsg 判断 superfile2 响应体是否表示 uploadid 失效。
// 该端点的错误以自由文本返回，只能按关键字识别（OpenList 同款判定）。
func isUploadIDExpiredMsg(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, "uploadid") &&
		(strings.Contains(lower, "invalid") ||
			strings.Contains(lower, "expired") ||
			strings.Contains(lower, "not found"))
}

// normalizeRootPath 规范化 root 配置：去掉首尾斜杠、补前导斜杠，空值为根目录。
func normalizeRootPath(root string) string {
	root = strings.TrimSpace(root)
	root = strings.Trim(root, "/")
	if root == "" {
		return "/"
	}
	return "/" + root
}

// clampInt 把 v 限制在 [min, max] 区间，越界取边界值。
func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// limitedWriter 只把前 n 字节转发给底层 writer，之后静默丢弃。
// 用于单遍流式计算前 256KB 的 slice-md5，避免回读临时文件。
type limitedWriter struct {
	w io.Writer
	n int64
}

// Write 满足 io.Writer；即使内容被丢弃也必须上报完整长度，
// 否则 io.MultiWriter 会误判写入失败。
func (lw *limitedWriter) Write(p []byte) (int, error) {
	if lw.n <= 0 {
		return len(p), nil
	}
	if int64(len(p)) <= lw.n {
		n, err := lw.w.Write(p)
		lw.n -= int64(n)
		return n, err
	}
	_, _ = lw.w.Write(p[:lw.n])
	lw.n = 0
	return len(p), nil
}
