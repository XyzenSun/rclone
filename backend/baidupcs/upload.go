// 上传编排：spool 单遍计算三重 MD5、precreate/superfile2/create 三段式
// 上传、秒传、分片并发与 uploadid 过期重传。
// 百度 precreate 要求在上传开始前提交全量 hash（block_list 每片 MD5、
// content-md5 全文件 MD5、slice-md5 前 256KB MD5），因此必须先读完输入。

package baidupcs

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"strconv"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/fserrors"
)

// errUploadIDExpired 表示 superfile2 报告 uploadid 失效，需要重新
// precreate 并全量重传所有分片。该错误不可按分片重试。
var errUploadIDExpired = errors.New("baidupcs: uploadid expired or invalid")

// spoolResult 是单遍读完输入流的产物：临时文件与三重 MD5。
type spoolResult struct {
	tmpFile    *os.File
	contentMD5 string
	sliceMD5   string
	blockList  []string
}

// spool 单遍流式读完 in：同时计算全文件 MD5、前 256KB MD5、各分片 MD5，
// 并把内容写入临时文件供分片上传阶段按偏移读取。
func (f *Fs) spool(ctx context.Context, in io.Reader, size int64) (res spoolResult, err error) {
	sliceSize := sliceSizeForVip(f.vipType)
	count, lastSize := blockCount(size, sliceSize)
	if count > maxSliceNum {
		return res, fmt.Errorf("baidupcs: file size %d needs %d slices of %d bytes, exceeding the %d slice limit", size, count, sliceSize, maxSliceNum)
	}
	res.tmpFile, err = os.CreateTemp("", "rclone-baidupcs-*")
	if err != nil {
		return res, fmt.Errorf("baidupcs: failed to create temporary file: %w", err)
	}
	// 任一步失败都清理临时文件，避免残留
	defer func() {
		if err != nil {
			_ = res.tmpFile.Close()
			_ = os.Remove(res.tmpFile.Name())
			res.tmpFile = nil
		}
	}()

	fileMD5 := md5.New()
	sliceMD5 := md5.New()
	sliceMD5Limited := &limitedWriter{w: sliceMD5, n: sliceMD5Size}
	blockMD5 := md5.New()
	res.blockList = make([]string, 0, count)
	var written int64
	for i := range count {
		readSize := sliceSize
		if i == count-1 {
			readSize = lastSize
		}
		blockMD5.Reset()
		w := io.MultiWriter(fileMD5, blockMD5, sliceMD5Limited, res.tmpFile)
		n, copyErr := io.CopyN(w, in, readSize)
		written += n
		if copyErr != nil {
			// io.CopyN 在源短于预期时返回 io.EOF，统一报为数据不足
			return res, fmt.Errorf("baidupcs: failed to read upload data (got %d of %d bytes): %w", written, size, copyErr)
		}
		res.blockList = append(res.blockList, hex.EncodeToString(blockMD5.Sum(nil)))
	}
	if written != size {
		return res, fmt.Errorf("baidupcs: spool size mismatch: read %d bytes, expected %d", written, size)
	}
	if _, err = res.tmpFile.Seek(0, io.SeekStart); err != nil {
		return res, fmt.Errorf("baidupcs: failed to seek temporary file: %w", err)
	}
	res.contentMD5 = hex.EncodeToString(fileMD5.Sum(nil))
	res.sliceMD5 = hex.EncodeToString(sliceMD5.Sum(nil))
	return res, nil
}

// objectFromCreated 用 create/precreate 响应构造 Object。
// 百度 create/precreate 响应里的时间恒为当前时间（OpenList 已踩坑），
// 必须用本地时间覆盖，否则 rclone 的 size+modtime 判等会失效。
func (f *Fs) objectFromCreated(remote, srvPath string, size int64, mtime int64, file apiFile) *Object {
	o := &Object{
		fs:      f,
		remote:  remote,
		srvPath: srvPath,
		fsID:    file.FsID,
		size:    size,
	}
	if mtime != 0 {
		o.modTime = time.Unix(mtime, 0)
	} else {
		o.modTime = fileModTime(&file)
	}
	return o
}

// uploadSlices 并发上传全部分片。partseqs 来自 precreate 响应的
// block_list（新上传为全部分片序号）。每片经 pacer 重试；
// uploadid 过期不按分片重试，而是向上抛出触发整体重传。
func (f *Fs) uploadSlices(ctx context.Context, uploadURL, srvPath, uploadid string, partseqs []int, tmpFile *os.File, size int64, fileName string) error {
	sliceSize := sliceSizeForVip(f.vipType)
	count, _ := blockCount(size, sliceSize)
	if len(partseqs) == 0 {
		// 防御：precreate 响应缺失 block_list 时按全量分片处理
		partseqs = make([]int, count)
		for i := range partseqs {
			partseqs[i] = i
		}
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(f.opt.UploadConcurrency)
	for _, partseq := range partseqs {
		if partseq < 0 || partseq >= count {
			// 防御：序号越界时跳过，交给 create 阶段校验完整性
			continue
		}
		g.Go(func() error {
			offset := int64(partseq) * sliceSize
			sliceLen := sliceSize
			if partseq == count-1 {
				sliceLen = size - offset
			}
			section := io.NewSectionReader(tmpFile, offset, sliceLen)
			return f.pacer.Call(func() (bool, error) {
				// 重试前把 SectionReader 归零，保证重读分片内容
				if _, err := section.Seek(0, io.SeekStart); err != nil {
					return false, err
				}
				err := f.uploadSlice(gctx, uploadURL, srvPath, uploadid, fileName, partseq, section)
				if errors.Is(err, errUploadIDExpired) {
					return false, err
				}
				return shouldRetry(err), err
			})
		})
	}
	return g.Wait()
}

// uploadSlice 上传单个分片到 superfile2 端点。
// multipart 头尾分离构造，文件内容直接从 SectionReader 流式读取，
// 避免整片缓冲进内存（分片最大 32MB）。
func (f *Fs) uploadSlice(ctx context.Context, uploadURL, srvPath, uploadid, fileName string, partseq int, section *io.SectionReader) error {
	head := &bytes.Buffer{}
	mw := multipart.NewWriter(head)
	if _, err := mw.CreateFormFile("file", fileName); err != nil {
		return err
	}
	headBytes := bytes.Clone(head.Bytes())
	if err := mw.Close(); err != nil {
		return err
	}
	// Close 之后 head 追加了结束边界，作为 multipart 尾部
	tail := head.Bytes()[len(headBytes):]

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL+superfile2Path,
		io.MultiReader(bytes.NewReader(headBytes), section, bytes.NewReader(tail)))
	if err != nil {
		return err
	}
	q := req.URL.Query()
	q.Set("method", "upload")
	q.Set("access_token", f.currentAccessToken())
	q.Set("type", "tmpfile")
	q.Set("path", srvPath)
	q.Set("uploadid", uploadid)
	q.Set("partseq", strconv.Itoa(partseq))
	req.URL.RawQuery = q.Encode()
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.ContentLength = int64(len(headBytes)) + section.Size() + int64(len(tail))

	resp, err := f.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return err
	}
	bodyStr := string(body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("baidupcs: slice upload failed: HTTP %d: %s", resp.StatusCode, bodyStr)
	}
	// superfile2 的错误以自由文本返回，先按关键字识别 uploadid 失效
	if isUploadIDExpiredMsg(bodyStr) {
		return errUploadIDExpired
	}
	var res superfile2Response
	if err := json.Unmarshal(body, &res); err != nil {
		return fmt.Errorf("baidupcs: failed to decode slice upload response: %w", err)
	}
	if res.Errno != 0 || res.ErrorCode != 0 {
		return fmt.Errorf("baidupcs: slice upload failed: errno %d, error_code %d: %s", res.Errno, res.ErrorCode, res.ErrorMsg)
	}
	return nil
}

// Update 上传 in 的内容到对象路径，覆盖同名旧文件（rtype=3）。
//
// 已知限制：accounting 只覆盖 spool 阶段的读取（读完即 100%），
// 分片上传阶段无逐片进度，与 flickr 等 spool 型 backend 一致。
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	remote := o.Remote()
	size := src.Size()
	if size < 0 {
		return errors.New("baidupcs: can't upload file with unknown size")
	}
	if size == 0 {
		// 百度网盘硬性拒绝空文件上传，实测无变体可用；
		// 标记为不可重试，避免 rclone 白白重试三次
		return fserrors.NoRetryError(errors.New("baidupcs: the server rejects zero-length files, can't upload"))
	}
	srvPath := o.fs.srvPath(remote)
	mtime := src.ModTime(ctx).Unix()

	// 确保父目录存在（copyto 到不存在的目录时 rclone 不会预先 Mkdir，
	// 参照 webdav 的 mkParentDir 约定在 Update 兜底；已存在时仅一次
	// list 探测，不触发 create）
	if parent, _ := splitServerPath(srvPath); parent != "" {
		if err := o.fs.mkdirServer(ctx, parent); err != nil {
			return err
		}
	}

	// spool：单遍读完输入，计算三重 MD5 并落临时文件
	spool, err := o.fs.spool(ctx, in, size)
	if err != nil {
		return err
	}
	defer func() {
		_ = spool.tmpFile.Close()
		_ = os.Remove(spool.tmpFile.Name())
	}()
	blockListJSON, err := json.Marshal(spool.blockList)
	if err != nil {
		return err
	}

	// 三段式第一步：precreate（rtype=3 覆盖同名）
	pre, err := o.fs.precreate(ctx, srvPath, size, string(blockListJSON), spool.contentMD5, spool.sliceMD5, mtime)
	if err != nil {
		return err
	}
	if pre.ReturnType == 2 {
		// 秒传命中：服务端已有相同内容（实测当前对该应用不生效，
		// 恒返回 return_type=1，但命中时此分支零成本生效）
		*o = *o.fs.objectFromCreated(remote, srvPath, size, mtime, pre.File)
		return nil
	}

	// 分片上传；uploadid 过期时重新 precreate（不带 md5）并全量重传一次
	for range 2 {
		uploadURL := o.fs.locateUpload(ctx, srvPath, pre.Uploadid)
		err = o.fs.uploadSlices(ctx, uploadURL, srvPath, pre.Uploadid, pre.BlockList, spool.tmpFile, size, path.Base(srvPath))
		if err == nil {
			break
		}
		if !errors.Is(err, errUploadIDExpired) {
			return err
		}
		fs.Debugf(o, "uploadid expired, restarting upload from scratch")
		pre, err = o.fs.precreate(ctx, srvPath, size, string(blockListJSON), "", "", mtime)
		if err != nil {
			return err
		}
		if pre.ReturnType == 2 {
			*o = *o.fs.objectFromCreated(remote, srvPath, size, mtime, pre.File)
			return nil
		}
	}
	if err != nil {
		return err
	}

	// 三段式第三步：create 落盘
	file, err := o.fs.createEntry(ctx, srvPath, size, 0, pre.Uploadid, string(blockListJSON), mtime)
	if err != nil {
		return err
	}
	*o = *o.fs.objectFromCreated(remote, srvPath, size, mtime, file)
	return nil
}

// Put 直接上传新对象，不预检查同名（Update 走 rtype=3 服务端覆盖）。
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	o := &Object{
		fs:     f,
		remote: src.Remote(),
	}
	if err := o.Update(ctx, in, src, options...); err != nil {
		return nil, err
	}
	return o, nil
}
