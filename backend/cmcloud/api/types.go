// Package api 提供中国移动云盘 IDaaS 开放通道的请求/响应类型与错误码。
//
// 业务 API 全部为 POST JSON；业务错误的表现形式是 HTTP 200 +
// success:false + code（真实凭据实测），错误判定必须解析响应体，
// 不能依赖 HTTP 状态码。
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// 业务网关与认证通道地址（固定值，来自 skill 发布配置 cmcloud.json）
const (
	BusinessBaseURL = "https://openapi.yun.139.com/open-ai-space"
	TokenBaseURL    = "https://idaas.cmpassport.com"
)

// BusinessAPIVersion 是业务请求公共头里的版本号（来自 cmcloud.json 的 request.commonHeaders）。
const BusinessAPIVersion = "2.0.0"

// 错误码（真实凭据实测确认）
const (
	// codeSuccessList 是文件族接口的成功码
	codeSuccessList = "0000"
	// codeSuccessFolder 是 user/folder/query 的成功码（与文件族不同）
	codeSuccessFolder = "0"
	// CodePermissionDenied 资源路径未在授权范围内（越界写）
	CodePermissionDenied = "16110013"
	// CodeResourceUnavailable 鉴权资源路径不可用（非法 parentFileId 等）
	CodeResourceUnavailable = "16110039"
	// CodeResourceNotExist 资源不存在（被屏蔽端点一律返回此码）
	CodeResourceNotExist = "01000005"
)

// ticketInvalidCodes 是票据失效码集合：命中后重新取票并重试当前请求一次。
// 来自官方 skill 客户端文档，未单独实测。
var ticketInvalidCodes = map[string]bool{
	"110044":   true, // 缓存失效
	"130090":   true, // uid 失效
	"16110038": true, // Token 有效期不足
	"01000003": true, // 令牌失效
}

// IsTicketInvalid 判断错误是否由短票据失效引起（含 HTTP 401）。
func IsTicketInvalid(err error) bool {
	apiErr, ok := err.(*Error)
	if !ok {
		return false
	}
	if apiErr.StatusCode == http.StatusUnauthorized {
		return true
	}
	return ticketInvalidCodes[apiErr.Code]
}

// Error 是平台返回的业务错误（HTTP 200 + success:false + code）
// 或非 2xx 响应包装出的错误。
type Error struct {
	// StatusCode 为 0 表示 HTTP 层成功（业务错误）
	StatusCode int
	Code       string
	Message    string
}

func (e *Error) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("cmcloud: HTTP status %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("cmcloud: platform returned failure (code=%s): %s", e.Code, e.Message)
}

// Response 是业务接口的统一响应外壳。code 可能是字符串（"0000"/"0"）
// 也可能是数字，success 可能缺失（folder/query 不返回该字段），
// 因此都用 RawMessage 承载后统一判定。
type Response struct {
	Success json.RawMessage `json:"success"`
	Code    json.RawMessage `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// check 判定响应是否成功，失败时返回 *Error。
// 判定规则：success 显式为假即失败；
// code 存在且不在成功码集合（"0000"/"0"）内即失败。
func (r *Response) check() error {
	if s := strings.TrimSpace(string(r.Success)); s != "" && s != "null" {
		if s != "true" && s != "1" && s != `"true"` && s != `"1"` {
			return &Error{Code: normalizeCode(r.Code), Message: r.Message}
		}
	}
	if c := normalizeCode(r.Code); c != "" && c != codeSuccessList && c != codeSuccessFolder {
		return &Error{Code: c, Message: r.Message}
	}
	return nil
}

// normalizeCode 把 RawMessage 形态的 code 归一化为十进制字符串，
// 兼容 "0000"（字符串）与 0（数字）两种形态。
func normalizeCode(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	s = strings.Trim(s, `"`)
	if n, err := strconv.Atoi(s); err == nil {
		return strconv.Itoa(n)
	}
	return s
}

// Item 是文件/目录对象（file/list、file/get、file/complete 的 data）。
// 时间为 RFC3339 带时区偏移、毫秒精度。
type Item struct {
	FileID               string `json:"fileId"`
	ParentFileID         string `json:"parentFileId"`
	Name                 string `json:"name"`
	Type                 string `json:"type"` // file | folder
	FileExtension        string `json:"fileExtension"`
	Category             string `json:"category"` // file 族为 doc/image/...，目录为 folder
	Size                 int64  `json:"size"`
	CreatedAt            string `json:"createdAt"`
	UpdatedAt            string `json:"updatedAt"`
	LocalCreatedAt       string `json:"localCreatedAt"`
	LocalUpdatedAt       string `json:"localUpdatedAt"`
	ContentHash          string `json:"contentHash"`
	ContentHashAlgorithm string `json:"contentHashAlgorithm"`
}

// IsFolder 判断条目是否为目录。
func (i *Item) IsFolder() bool {
	return i.Type == "folder"
}

// PageInfo 是 file/list 的分页参数。pageSize 限 10~100，
// items 为空但 nextPageCursor 非空时仍需继续翻页（实测结论）。
type PageInfo struct {
	PageSize   int    `json:"pageSize"`
	PageCursor string `json:"pageCursor,omitempty"`
}

// ListRequest 是 file/list 请求体。
type ListRequest struct {
	ParentFileID string    `json:"parentFileId"`
	PageInfo     *PageInfo `json:"pageInfo,omitempty"`
}

// ListResponse 是 file/list 的 data。
type ListResponse struct {
	Items          []Item `json:"items"`
	NextPageCursor string `json:"nextPageCursor"`
}

// CreateFolderRequest 是 file/createFolder 请求体。
// 不发送 fileRenameMode（平台默认 force_rename 会在同名时改名为
// 时间戳副本），目录创建的幂等性由 dircache 的先查后建保证。
type CreateFolderRequest struct {
	Name         string `json:"name"`
	ParentFileID string `json:"parentFileId"`
}

// CreateFolderResponse 是 file/createFolder 的 data。
type CreateFolderResponse struct {
	ParentFileID string `json:"parentFileId"`
	FileID       string `json:"fileId"`
	FileName     string `json:"fileName"`
	Type         string `json:"type"`
}

// PartInfo 是 create 请求里的分片描述。串行上传只填
// partNumber/partSize（并行上传需要 partOffset/parallelHashCtx，本实现不做）。
type PartInfo struct {
	PartNumber int   `json:"partNumber"`
	PartSize   int64 `json:"partSize"`
}

// CreateUploadRequest 是 file/create 请求体。
type CreateUploadRequest struct {
	Name                 string     `json:"name"`
	Type                 string     `json:"type"` // 固定 "file"
	Size                 int64      `json:"size"`
	ParentFileID         string     `json:"parentFileId"`
	PartInfos            []PartInfo `json:"partInfos,omitempty"`
	FormUpload           bool       `json:"formUpload,omitempty"`
	FileRenameMode       string     `json:"fileRenameMode,omitempty"`
	ContentHash          string     `json:"contentHash,omitempty"`
	ContentHashAlgorithm string     `json:"contentHashAlgorithm,omitempty"`
	LocalCreatedAt       string     `json:"localCreatedAt,omitempty"`
	LocalUpdatedAt       string     `json:"localUpdatedAt,omitempty"`
}

// UploadPart 是 create 响应里的分片上传地址（S3 预签名 PUT，有效期 7200 秒）。
type UploadPart struct {
	PartNumber int    `json:"partNumber"`
	UploadURL  string `json:"uploadUrl"`
}

// FormInfo 是表单上传（formUpload:true）时 create 响应的表单信息，
// 仅空文件上传使用：分片链路的 partSize 必须为正数，无法表达 size=0。
type FormInfo struct {
	UploadURL string            `json:"uploadUrl"`
	DataField string            `json:"dataField"`
	FormData  map[string]string `json:"formData"`
}

// CreateUploadResponse 是 file/create 的 data。
// rapidUpload=true 时秒传命中，无 uploadId/partInfos；
// fileRenameMode=refuse 命中同名文件时 exist=true 且无上传任务。
type CreateUploadResponse struct {
	ParentFileID string       `json:"parentFileId"`
	FileID       string       `json:"fileId"`
	FileName     string       `json:"fileName"`
	Type         string       `json:"type"`
	RapidUpload  bool         `json:"rapidUpload"`
	Exist        *bool        `json:"exist"`
	UploadID     string       `json:"uploadId"`
	PartInfos    []UploadPart `json:"partInfos"`
	FormInfo     *FormInfo    `json:"formInfo"`
}

// CompleteUploadRequest 是 file/complete 请求体。
type CompleteUploadRequest struct {
	FileID               string `json:"fileId"`
	UploadID             string `json:"uploadId"`
	ContentHash          string `json:"contentHash"`
	ContentHashAlgorithm string `json:"contentHashAlgorithm"`
}

// GetDownloadURLRequest 是 file/getDownloadUrl 请求体（expireSec 1~86400）。
type GetDownloadURLRequest struct {
	FileID    string `json:"fileId"`
	ExpireSec int    `json:"expireSec,omitempty"`
}

// GetDownloadURLResponse 是 file/getDownloadUrl 的 data。
type GetDownloadURLResponse struct {
	FileID      string `json:"fileId"`
	URL         string `json:"url"`
	Expiration  string `json:"expiration"`
	Size        int64  `json:"size"`
	ContentHash string `json:"contentHash"`
}

// BatchCopyRequest 是 file/batchCopy 请求体。
// 该接口只能把文件原名复制到目标目录，不支持改名。
type BatchCopyRequest struct {
	FileIDs        []string `json:"fileIds"`
	ToParentFileID string   `json:"toParentFileId"`
}

// BatchCopyResponse 是 file/batchCopy 的 data。
type BatchCopyResponse struct {
	TaskID string `json:"taskId"`
}

// TaskGetRequest 是 task/get 请求体。
type TaskGetRequest struct {
	TaskID string `json:"taskId"`
}

// 任务状态枚举：Running 为非终态，其余为终态。
const (
	TaskStatusRunning        = "Running"
	TaskStatusSucceed        = "Succeed"
	TaskStatusPartialSucceed = "PartialSucceed"
	TaskStatusFailed         = "Failed"
)

// FileRef 是任务结果里对文件的引用。
type FileRef struct {
	FileID string `json:"fileId"`
	Type   string `json:"type"`
}

// BatchFileResult 是批量任务的逐文件结果。
type BatchFileResult struct {
	SrcFile FileRef `json:"srcFile"`
	RstFile FileRef `json:"rstFile"`
	ErrCode string  `json:"errCode"`
	Message string  `json:"message"`
}

// TaskInfo 是任务的状态信息。
type TaskInfo struct {
	TaskID     string `json:"taskId"`
	Status     string `json:"status"`
	Progress   int    `json:"progress"`
	TaskType   int    `json:"taskType"`
	CreatedAt  string `json:"createdAt"`
	FinishedAt string `json:"finishedAt"`
}

// TaskGetResponse 是 task/get 的 data。
type TaskGetResponse struct {
	TaskInfo         TaskInfo          `json:"taskInfo"`
	BatchFileResults []BatchFileResult `json:"batchFileResults"`
}

// UserFolderResponse 是 user/folder/query 的 data（专属目录）。
type UserFolderResponse struct {
	FolderID string `json:"folderId"`
	IDPath   string `json:"idPath"`
	NamePath string `json:"namePath"`
}
