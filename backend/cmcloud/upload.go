package cmcloud

// 上传链路：file/create → 预签名 PUT 分片 → file/complete。
//
// 分片规划保证总片数不超过 100（服务端单批上限），create 响应直接
// 携带全部分片地址；空文件走 formUpload 表单链路（分片链路的
// partSize 必须为正数，无法表达 size=0）。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/rclone/rclone/backend/cmcloud/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/fs/fserrors"
	libmultipart "github.com/rclone/rclone/lib/multipart"
)

// planParts 规划分片（算法对齐官方 skill 客户端的 _plan_parts，
// 以 chunk_size 替代其固定的 5MiB 基准）：
//
//	片数 = min(ceil(size/chunkSize), 100)
//	单片 = max(chunkSize, ceil(size/片数))
//
// 片数被 100 封顶后单片随文件增大，因此总片数恒不超过 100，
// getUploadUrl 补取地址的路径在本规划下不可达。非末片大小恒
// 不小于 chunkSize（NewFs 已验证 >= 5MiB），满足服务端约束。
func planParts(size, chunkSize int64) []api.PartInfo {
	count := (size + chunkSize - 1) / chunkSize
	if count > 100 {
		count = 100
	}
	if count < 1 {
		count = 1
	}
	base := (size + count - 1) / count
	if base < chunkSize {
		base = chunkSize
	}
	var parts []api.PartInfo
	var offset int64
	for number := 1; offset < size; number++ {
		partSize := base
		if remaining := size - offset; remaining < partSize {
			partSize = remaining
		}
		parts = append(parts, api.PartInfo{PartNumber: number, PartSize: partSize})
		offset += partSize
	}
	return parts
}

// hashAndPrepare 计算输入的完整 sha256 并返回可重复读取的输入流。
// create 需要提交 contentHash（服务端据此判定秒传），因此哈希必须在
// 建任务前算好：可 seek 的输入就地扫描后回卷，零额外内存；不可 seek
// 的流先落入临时文件。
func hashAndPrepare(in io.Reader) (r io.Reader, contentHash string, cleanup func(), err error) {
	if rs, ok := in.(io.ReadSeeker); ok {
		hasher := sha256.New()
		if _, err = io.Copy(hasher, rs); err != nil {
			return nil, "", nil, fmt.Errorf("cmcloud: failed to hash input: %w", err)
		}
		if _, err = rs.Seek(0, io.SeekStart); err != nil {
			return nil, "", nil, fmt.Errorf("cmcloud: failed to rewind input: %w", err)
		}
		return rs, hex.EncodeToString(hasher.Sum(nil)), nil, nil
	}
	tmp, err := os.CreateTemp("", "rclone-cmcloud-*.bin")
	if err != nil {
		return nil, "", nil, fmt.Errorf("cmcloud: failed to create spool file: %w", err)
	}
	cleanup = func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}
	hasher := sha256.New()
	if _, err = io.Copy(io.MultiWriter(tmp, hasher), in); err != nil {
		cleanup()
		return nil, "", nil, fmt.Errorf("cmcloud: failed to spool input: %w", err)
	}
	if _, err = tmp.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, "", nil, fmt.Errorf("cmcloud: failed to rewind spool file: %w", err)
	}
	return tmp, hex.EncodeToString(hasher.Sum(nil)), cleanup, nil
}

// formatLocalTime 把时间格式化为平台接受的 RFC3339 毫秒精度 UTC 字符串
// （create 的 localCreatedAt/localUpdatedAt 字段，是设置 ModTime 的
// 唯一途径）。
func formatLocalTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// Update the object with the contents of the io.Reader, modTime and size
//
// 平台不接受覆盖语义（fileRenameMode 无 overwrite 选项，删除端点被
// 屏蔽）：目标已存在时 create 以 refuse 模式返回 exist=true 且不建立
// 上传任务，本方法将其映射为明确报错。
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (err error) {
	size := src.Size()
	if size < 0 {
		return errors.New("cmcloud: can't upload files of unknown size")
	}
	modTime := src.ModTime(ctx)
	remote := o.Remote()

	// 建目录链（父目录缺失时逐级创建）
	leaf, directoryID, err := o.fs.dirCache.FindPath(ctx, remote, true)
	if err != nil {
		return err
	}

	// 把 accounting 移到缓冲链末端，避免哈希扫描与分片传输重复计数
	in, wrap := accounting.UnWrap(in)
	hashedIn, contentHash, cleanup, err := hashAndPrepare(in)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}

	createReq := &api.CreateUploadRequest{
		Name:                 o.fs.opt.Enc.FromStandardName(leaf),
		Type:                 "file",
		Size:                 size,
		ParentFileID:         directoryID,
		ContentHash:          contentHash,
		ContentHashAlgorithm: "sha256",
		// refuse：目标已存在时报错而非静默改名（rclone 语义）
		FileRenameMode: "refuse",
		LocalCreatedAt: formatLocalTime(modTime),
		LocalUpdatedAt: formatLocalTime(modTime),
	}
	if size == 0 {
		// 空文件走表单链路（分片链路要求 partSize 为正数）
		createReq.FormUpload = true
	} else if size <= int64(o.fs.opt.UploadCutoff) {
		createReq.PartInfos = []api.PartInfo{{PartNumber: 1, PartSize: size}}
	} else {
		createReq.PartInfos = planParts(size, int64(o.fs.opt.ChunkSize))
	}

	created, err := o.fs.c.CreateUpload(ctx, createReq)
	if err != nil {
		return err
	}
	if created.RapidUpload {
		// 秒传命中：跳过传输与 complete（服务端已凭 contentHash 入库）
		return o.refreshFromServer(ctx, created.FileID)
	}
	if created.Exist != nil && *created.Exist {
		return fmt.Errorf("cmcloud: can't upload %q: file exists and the platform can't overwrite files", remote)
	}

	if created.FormInfo != nil {
		if err = o.formUpload(ctx, created.FormInfo, hashedIn, wrap); err != nil {
			return err
		}
	} else {
		if created.UploadID == "" {
			return fmt.Errorf("cmcloud: file/create response missing uploadId for %q", remote)
		}
		if err = o.uploadParts(ctx, created, createReq.PartInfos, hashedIn, wrap); err != nil {
			return err
		}
	}

	complete, err := o.fs.c.CompleteUpload(ctx, &api.CompleteUploadRequest{
		FileID:               created.FileID,
		UploadID:             created.UploadID,
		ContentHash:          contentHash,
		ContentHashAlgorithm: "sha256",
	})
	if err != nil {
		return err
	}
	// 实测 complete 响应直接携带完整文件元数据；字段缺失时防御性回查
	if complete.FileID != "" && complete.Name != "" {
		o.setMetaData(complete)
		return nil
	}
	return o.refreshFromServer(ctx, created.FileID)
}

// refreshFromServer 回查文件元数据并刷新本地缓存。
func (o *Object) refreshFromServer(ctx context.Context, fileID string) error {
	item, err := o.fs.c.FileGet(ctx, fileID)
	if err != nil {
		return err
	}
	o.setMetaData(item)
	return nil
}

// uploadParts 串行上传全部分片。每片从全局内存池取缓冲（可回卷，
// 供 pacer 重试重读），PUT 预签名地址时显式携带 Content-Length 与
// Content-Type（预签名 PUT 的对象存储契约要求，缺失会被拒绝）。
func (o *Object) uploadParts(ctx context.Context, created *api.CreateUploadResponse, planned []api.PartInfo, in io.Reader, wrap accounting.WrapFn) error {
	urlByNumber := make(map[int]string, len(created.PartInfos))
	for _, part := range created.PartInfos {
		urlByNumber[part.PartNumber] = part.UploadURL
	}
	var offset int64
	for i := range planned {
		part := planned[i]
		// 分片地址缺失说明服务端未按规划返回（规划保证片数 <= 100，
		// create 应携带全部地址），属于契约异常而非可重试状态
		uploadURL := urlByNumber[part.PartNumber]
		if uploadURL == "" {
			return fmt.Errorf("cmcloud: missing upload URL for part %d of %q", part.PartNumber, o.remote)
		}
		rw := libmultipart.NewRW().Reserve(part.PartSize)
		n, err := io.CopyN(rw, in, part.PartSize)
		if err != nil && err != io.EOF {
			_ = rw.Close()
			return fmt.Errorf("cmcloud: failed to read part %d of %q: %w", part.PartNumber, o.remote, err)
		}
		if n != part.PartSize {
			_ = rw.Close()
			return fmt.Errorf("cmcloud: short read for part %d of %q: got %d want %d", part.PartNumber, o.remote, n, part.PartSize)
		}
		err = o.putPart(ctx, uploadURL, part, rw, wrap)
		_ = rw.Close()
		if err != nil {
			return fmt.Errorf("cmcloud: failed to upload part %d/%d of %q: %w", part.PartNumber, len(planned), o.remote, err)
		}
		offset += part.PartSize
		fs.Debugf(o, "Uploaded part %d/%d (offset %d bytes)", part.PartNumber, len(planned), offset)
	}
	return nil
}

// putPart 把一片数据 PUT 到预签名地址，经 pacer 重试。
// rw 是可回卷的池化缓冲，每次重试前 Seek 回起点。
func (o *Object) putPart(ctx context.Context, uploadURL string, part api.PartInfo, rw io.ReadSeeker, wrap accounting.WrapFn) error {
	return o.fs.pacer.Call(func() (bool, error) {
		if _, err := rw.Seek(0, io.SeekStart); err != nil {
			return false, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, wrap(rw))
		if err != nil {
			return false, err
		}
		req.ContentLength = part.PartSize
		req.Header.Set("Content-Type", "application/octet-stream")
		// 对象存储契约要求显式 Date 头（Go client 不会自动添加）
		req.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
		resp, err := o.fs.client.Do(req)
		if err != nil {
			return fserrors.ShouldRetry(err), err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			statusCode := resp.StatusCode
			_ = resp.Body.Close()
			return statusCode == http.StatusTooManyRequests || statusCode >= 500,
				fmt.Errorf("HTTP status %d (%s) returned body: %q", statusCode, resp.Status, body)
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return false, nil
	})
}

// formUpload 走表单链路上传（仅空文件）。表单构造复刻官方 skill
// 客户端：普通字段在前，文件段（filename="blob"）最后。
func (o *Object) formUpload(ctx context.Context, form *api.FormInfo, in io.Reader, wrap accounting.WrapFn) error {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	// map 遍历顺序不稳定，按 key 排序保证请求可复现
	keys := make([]string, 0, len(form.FormData))
	for key := range form.FormData {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := writer.WriteField(key, form.FormData[key]); err != nil {
			return fmt.Errorf("cmcloud: form field write failed: %w", err)
		}
	}
	partWriter, err := writer.CreateFormFile(form.DataField, "blob")
	if err != nil {
		return fmt.Errorf("cmcloud: form file field create failed: %w", err)
	}
	if _, err = io.Copy(partWriter, wrap(in)); err != nil {
		return fmt.Errorf("cmcloud: form body write failed: %w", err)
	}
	if err = writer.Close(); err != nil {
		return fmt.Errorf("cmcloud: form close failed: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, form.UploadURL, bytes.NewReader(body.Bytes()))
	if err != nil {
		return err
	}
	req.ContentLength = int64(body.Len())
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := o.fs.client.Do(req)
	if err != nil {
		return fmt.Errorf("cmcloud: form upload failed: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("cmcloud: form upload failed: HTTP status %d: %q", resp.StatusCode, respBody)
	}
	return nil
}
