package api

// 业务 API 封装：统一请求头、票据失效自动换票重试、分页与轮询。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/rest"
)

const (
	minSleep = 200 * time.Millisecond
	maxSleep = 2 * time.Second

	// listPageSize 是 file/list 的页大小（服务端限 10~100）。
	listPageSize = 100

	// downloadURLExpireSec 是预签名下载地址的有效期（1~86400）。
	downloadURLExpireSec = 3600
)

// retryErrorCodes 是值得经 pacer 重试的 HTTP 状态码。
var retryErrorCodes = []int{
	429, // Too Many Requests
	500, // Internal Server Error
	502, // Bad Gateway
	503, // Service Unavailable
	504, // Gateway Timeout
}

// Client 是业务通道客户端（openapi.yun.139.com V3 网关）。
type Client struct {
	srv   *rest.Client
	auth  *Auth
	pacer *fs.Pacer
}

// NewClient 创建业务客户端。srv 必须已 SetRoot(BusinessBaseURL)
// 并安装了把非 2xx 响应包装为 *api.Error 的 ErrorHandler。
func NewClient(srv *rest.Client, auth *Auth, pacer *fs.Pacer) *Client {
	return &Client{srv: srv, auth: auth, pacer: pacer}
}

// NewPacer 构造默认的 pacer（与 backend 主包共用同一配置）。
func NewPacer(ctx context.Context) *fs.Pacer {
	return fs.NewPacer(ctx, pacer.NewDefault(pacer.MinSleep(minSleep), pacer.MaxSleep(maxSleep)))
}

// shouldRetry 判断业务请求是否值得重试（网络错误/429/5xx）。
// 票据失效不走 pacer，由 callJSON 的换票重试层处理。
func shouldRetry(ctx context.Context, resp *http.Response, err error) (bool, error) {
	if fserrors.ContextError(ctx, &err) {
		return false, err
	}
	return fserrors.ShouldRetry(err) || fserrors.ShouldRetryHTTP(resp, retryErrorCodes), err
}

// businessHeaders 组装业务请求头（全部业务接口必需）。
func (c *Client) businessHeaders(creditToken string) map[string]string {
	return map[string]string{
		"creditToken": creditToken,
		"skillId":     skillID,
		"clientId":    skillClientID,
		"strategyId":  skillStrategy,
		"clawUid":     c.auth.ClawUID(),
		"version":     BusinessAPIVersion,
	}
}

// HTTPErrorHandler 把非 2xx 业务响应包装为 *Error，供
// rest.Client.SetErrorHandler 使用。鉴权层失败（票据失效）表现为
// HTTP 401，必须携带状态码以便 IsTicketInvalid 识别。
func HTTPErrorHandler(resp *http.Response) error {
	body, _ := rest.ReadBody(resp)
	message := string(body)
	var envelope Response
	if json.Unmarshal(body, &envelope) == nil && envelope.Message != "" {
		message = envelope.Message
	}
	return &Error{
		StatusCode: resp.StatusCode,
		Code:       normalizeCode(envelope.Code),
		Message:    message,
	}
}

// callJSON 发起一次业务 POST：组头 → pacer 重试 → 响应判定 →
// 票据失效时换票重试一次 → 把 data 解码到 out。
func (c *Client) callJSON(ctx context.Context, path string, in, out any) error {
	var lastErr error
	// 最多两轮：第一轮正常执行；命中票据失效码/HTTP 401 时作废票据再来一轮。
	for range 2 {
		creditToken, err := c.auth.CreditToken(ctx)
		if err != nil {
			return err
		}
		headers := c.businessHeaders(creditToken)
		var envelope Response
		err = c.pacer.Call(func() (bool, error) {
			opts := rest.Opts{
				Method:       "POST",
				Path:         path,
				ExtraHeaders: headers,
				ContentType:  "application/json; charset=utf-8",
			}
			resp, err := c.srv.CallJSON(ctx, &opts, in, &envelope)
			return shouldRetry(ctx, resp, err)
		})
		if err != nil {
			if IsTicketInvalid(err) {
				c.auth.InvalidateCreditToken()
				lastErr = err
				continue
			}
			return err
		}
		if err := envelope.check(); err != nil {
			if IsTicketInvalid(err) {
				c.auth.InvalidateCreditToken()
				lastErr = err
				continue
			}
			return err
		}
		if out != nil && len(envelope.Data) > 0 && string(envelope.Data) != "null" {
			if err := json.Unmarshal(envelope.Data, out); err != nil {
				return fmt.Errorf("cmcloud: failed to decode response data of %s: %w", path, err)
			}
		}
		return nil
	}
	return fmt.Errorf("cmcloud: request to %s still failing after ticket refresh: %w", path, lastErr)
}

// UserFolderQuery 查询智能体专属目录（backend 的 root 锚点）。
// 该接口无请求体，身份由请求头承载；成功码是 "0"（与文件族不同）。
func (c *Client) UserFolderQuery(ctx context.Context) (*UserFolderResponse, error) {
	var out UserFolderResponse
	// 传 nil 请求体：rest 会发送空 body 的 POST（与 SDK 行为一致）。
	if err := c.callJSON(ctx, "/richlifeApp/idaas/user/folder/query", nil, &out); err != nil {
		return nil, err
	}
	if out.FolderID == "" {
		return nil, fmt.Errorf("cmcloud: user/folder/query response missing folderId")
	}
	return &out, nil
}

// FileList 拉取一页目录列表。cursor 为空时从第一页开始。
// 返回的 ListResponse.NextPageCursor 非空时需继续翻页（即使 items 为空）。
func (c *Client) FileList(ctx context.Context, parentFileID, cursor string) (*ListResponse, error) {
	in := ListRequest{
		ParentFileID: parentFileID,
		PageInfo: &PageInfo{
			PageSize:   listPageSize,
			PageCursor: cursor,
		},
	}
	var out ListResponse
	if err := c.callJSON(ctx, "/richlifeApp/idaasV3/personalSaas/file/list", &in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// FileGet 按 fileId 查询单个对象的完整元数据。
func (c *Client) FileGet(ctx context.Context, fileID string) (*Item, error) {
	in := map[string]string{"fileId": fileID}
	var out Item
	if err := c.callJSON(ctx, "/richlifeApp/idaasV3/personalSaas/file/get", &in, &out); err != nil {
		return nil, err
	}
	if out.FileID == "" {
		return nil, fmt.Errorf("cmcloud: file/get response missing fileId")
	}
	return &out, nil
}

// CreateFolder 在 parentFileID 下创建目录。
// 同名冲突由服务端按默认 force_rename 改名，调用方（dircache）
// 通过先查后建保证不会对已存在目录调用本接口。
func (c *Client) CreateFolder(ctx context.Context, name, parentFileID string) (*CreateFolderResponse, error) {
	in := CreateFolderRequest{Name: name, ParentFileID: parentFileID}
	var out CreateFolderResponse
	if err := c.callJSON(ctx, "/richlifeApp/idaasV3/personalSaas/file/createFolder", &in, &out); err != nil {
		return nil, err
	}
	if out.FileID == "" {
		return nil, fmt.Errorf("cmcloud: createFolder response missing fileId")
	}
	return &out, nil
}

// CreateUpload 创建上传任务（file/create）。
func (c *Client) CreateUpload(ctx context.Context, in *CreateUploadRequest) (*CreateUploadResponse, error) {
	var out CreateUploadResponse
	if err := c.callJSON(ctx, "/richlifeApp/idaasV3/personalSaas/file/create", in, &out); err != nil {
		return nil, err
	}
	if out.FileID == "" {
		return nil, fmt.Errorf("cmcloud: file/create response missing fileId")
	}
	return &out, nil
}

// CompleteUpload 完成上传（file/complete）。实测响应 data 直接返回
// 完整文件元数据，无需再调 file/get 核验。
func (c *Client) CompleteUpload(ctx context.Context, in *CompleteUploadRequest) (*Item, error) {
	var out Item
	if err := c.callJSON(ctx, "/richlifeApp/idaasV3/personalSaas/file/complete", in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetDownloadURL 获取预签名下载地址（S3 预签名 GET，支持 Range）。
func (c *Client) GetDownloadURL(ctx context.Context, fileID string) (*GetDownloadURLResponse, error) {
	in := GetDownloadURLRequest{FileID: fileID, ExpireSec: downloadURLExpireSec}
	var out GetDownloadURLResponse
	if err := c.callJSON(ctx, "/richlifeApp/idaasV3/personalSaas/file/getDownloadUrl", &in, &out); err != nil {
		return nil, err
	}
	if out.URL == "" {
		return nil, fmt.Errorf("cmcloud: getDownloadUrl response missing url")
	}
	return &out, nil
}

// BatchCopy 发起服务端复制（file/batchCopy），返回任务 ID。
// 该接口只能把文件原名复制到目标目录，不支持改名。
func (c *Client) BatchCopy(ctx context.Context, fileIDs []string, toParentFileID string) (string, error) {
	in := BatchCopyRequest{FileIDs: fileIDs, ToParentFileID: toParentFileID}
	var out BatchCopyResponse
	if err := c.callJSON(ctx, "/richlifeApp/idaasV3/personalSaas/file/batchCopy", &in, &out); err != nil {
		return "", err
	}
	if out.TaskID == "" {
		return "", fmt.Errorf("cmcloud: batchCopy response missing taskId")
	}
	return out.TaskID, nil
}

// TaskGet 查询批量任务状态（task/get）。
func (c *Client) TaskGet(ctx context.Context, taskID string) (*TaskGetResponse, error) {
	in := TaskGetRequest{TaskID: taskID}
	var out TaskGetResponse
	if err := c.callJSON(ctx, "/richlifeApp/idaasV3/personalSaas/task/get", &in, &out); err != nil {
		return nil, err
	}
	if out.TaskInfo.TaskID == "" && out.TaskInfo.Status == "" {
		return nil, fmt.Errorf("cmcloud: task/get response missing taskInfo")
	}
	return &out, nil
}

// WaitForTask 轮询任务直到终态（Succeed/PartialSucceed/Failed）。
// 实测小文件复制秒级完成；轮询间隔 1 秒，上限 5 分钟。
func (c *Client) WaitForTask(ctx context.Context, taskID string) (*TaskGetResponse, error) {
	const (
		pollInterval = time.Second
		pollTimeout  = 5 * time.Minute
	)
	deadline := time.Now().Add(pollTimeout)
	for {
		res, err := c.TaskGet(ctx, taskID)
		if err != nil {
			return nil, err
		}
		switch res.TaskInfo.Status {
		case TaskStatusSucceed:
			return res, nil
		case TaskStatusPartialSucceed, TaskStatusFailed:
			return res, fmt.Errorf("cmcloud: task %s finished with status %s: %s", taskID, res.TaskInfo.Status, summarizeTaskResults(res))
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("cmcloud: task %s still %s after %s", taskID, res.TaskInfo.Status, pollTimeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// summarizeTaskResults 汇总批量任务的逐文件失败明细。
func summarizeTaskResults(res *TaskGetResponse) string {
	detail := ""
	for _, r := range res.BatchFileResults {
		if r.ErrCode != "" && r.ErrCode != codeSuccessList && r.ErrCode != codeSuccessFolder {
			detail += fmt.Sprintf("[src=%s code=%s %s]", r.SrcFile.FileID, r.ErrCode, r.Message)
		}
	}
	return detail
}
