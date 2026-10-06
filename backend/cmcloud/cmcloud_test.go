// Unit tests for cmcloud backend.
//
// 只覆盖纯算法与纯函数（分片规划边界、时间格式化），信封加密对拍
// 在 api 包内测（需要访问包内部状态），不 mock HTTP 层——mock 出来
// 的协议假设没有验证价值（fork 测试策略），backend 行为验证一律用
// 真实凭证跑编译产物。
package cmcloud

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fstest/fstests"
)

// TestPlanParts 验证分片规划的约束：片数不超过 100、非末片不小于
// chunkSize（即 >= 5MiB 的服务端约束）、分片精确覆盖整个文件。
func TestPlanParts(t *testing.T) {
	const chunk = int64(5 * 1024 * 1024)
	cases := []struct {
		name      string
		size      int64
		chunkSize int64
		wantParts int
	}{
		{"single small file", 100, chunk, 1},
		{"exact one chunk", chunk, chunk, 1},
		{"one chunk plus one byte", chunk + 1, chunk, 2},
		{"hundred chunks", 100 * chunk, chunk, 100},
		{"over hundred chunks caps part count", 101 * chunk, chunk, 100},
		{"large file grows part size", 100 * 1024 * 1024 * 1024, 32 * 1024 * 1024, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parts := planParts(tc.size, tc.chunkSize)
			if len(parts) != tc.wantParts {
				t.Fatalf("got %d parts, want %d", len(parts), tc.wantParts)
			}
			var total int64
			for i, part := range parts {
				if part.PartNumber != i+1 {
					t.Fatalf("part %d has number %d, want %d", i, part.PartNumber, i+1)
				}
				if part.PartSize <= 0 {
					t.Fatalf("part %d has non-positive size %d", i, part.PartSize)
				}
				if i < len(parts)-1 && part.PartSize < tc.chunkSize {
					t.Fatalf("non-last part %d size %d < chunk size %d", i, part.PartSize, tc.chunkSize)
				}
				total += part.PartSize
			}
			if total != tc.size {
				t.Fatalf("parts total %d != file size %d", total, tc.size)
			}
		})
	}
}

// TestPlanPartsLargeFilePartSize 验证片数封顶后单片按比例增大，
// 保证 100 片恰好覆盖超大文件。
func TestPlanPartsLargeFilePartSize(t *testing.T) {
	size := int64(10 * 1024 * 1024 * 1024) // 10 GiB
	chunk := int64(32 * 1024 * 1024)       // 32 MiB
	parts := planParts(size, chunk)
	if len(parts) != 100 {
		t.Fatalf("got %d parts, want 100", len(parts))
	}
	var total int64
	for _, part := range parts {
		total += part.PartSize
	}
	if total != size {
		t.Fatalf("parts total %d != file size %d", total, size)
	}
}

// TestFormatLocalTime 验证平台时间格式：RFC3339、毫秒精度、UTC。
func TestFormatLocalTime(t *testing.T) {
	want := "2001-02-03T04:05:06.499Z"
	got := formatLocalTime(time.Date(2001, 2, 3, 4, 5, 6, 499999999, time.UTC))
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// 非零时区输入应归一化为 UTC
	got = formatLocalTime(time.Date(2001, 2, 3, 12, 5, 6, 0, time.FixedZone("CST", 8*3600)))
	if got != "2001-02-03T04:05:06.000Z" {
		t.Fatalf("got %q, want %q (input should be normalized to UTC)", got, "2001-02-03T04:05:06.000Z")
	}
}

// TestHashAndPrepareSeekable 验证哈希计算后输入可重复读取。
func TestHashAndPrepareSeekable(t *testing.T) {
	data := []byte("cmcloud hash and prepare test data")
	r := bytes.NewReader(data)
	out, contentHash, cleanup, err := hashAndPrepare(r)
	if err != nil {
		t.Fatal(err)
	}
	if cleanup != nil {
		t.Fatal("seekable input should not need cleanup")
	}
	// 重新读取应得到相同数据
	got, err := io.ReadAll(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("reread mismatch: got %q want %q", got, data)
	}
	// 与标准 sha256 对拍
	if contentHash != sha256Hex(data) {
		t.Fatalf("content hash mismatch: got %s want %s", contentHash, sha256Hex(data))
	}
}

// TestHashAndPrepareNonSeekable 验证不可 seek 的流落入临时文件且
// 清理函数删除它。
func TestHashAndPrepareNonSeekable(t *testing.T) {
	data := []byte("cmcloud non seekable spool test")
	// io.NopCloser 遮蔽 bytes.Reader 的 Seek 方法，模拟不可 seek 的流
	out, contentHash, cleanup, err := hashAndPrepare(io.NopCloser(bytes.NewReader(data)))
	if err != nil {
		t.Fatal(err)
	}
	if cleanup == nil {
		t.Fatal("non-seekable input should provide cleanup")
	}
	got, err := io.ReadAll(out)
	if err != nil {
		cleanup()
		t.Fatal(err)
	}
	if string(got) != string(data) {
		cleanup()
		t.Fatalf("reread mismatch: got %q want %q", got, data)
	}
	if contentHash != sha256Hex(data) {
		cleanup()
		t.Fatalf("content hash mismatch: got %s want %s", contentHash, sha256Hex(data))
	}
	cleanup()
}

// sha256Hex 返回数据的 sha256 十六进制摘要。
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// TestIntegration runs integration tests against the remote.
// 远端未配置时整套测试自动跳过（fstests.Run 内部判定）。
func TestIntegration(t *testing.T) {
	fstests.Run(t, &fstests.Opt{
		RemoteName: "TestCmcloud:",
		NilObject:  (*Object)(nil),
		ChunkedUpload: fstests.ChunkedUploadConfig{
			MinChunkSize: minChunkSize,
		},
	})
}

// SetUploadChunkSize 设置分片大小（fstests 的分片上传用例用小分片
// 触发多片路径）。
func (f *Fs) SetUploadChunkSize(cs fs.SizeSuffix) (fs.SizeSuffix, error) {
	if cs < minChunkSize {
		return f.opt.ChunkSize, fmt.Errorf("chunk size %v is less than minimum %v", cs, fs.SizeSuffix(minChunkSize))
	}
	old := f.opt.ChunkSize
	f.opt.ChunkSize = cs
	return old, nil
}

// SetUploadCutoff 设置单片直传阈值。
func (f *Fs) SetUploadCutoff(cs fs.SizeSuffix) (fs.SizeSuffix, error) {
	old := f.opt.UploadCutoff
	f.opt.UploadCutoff = cs
	return old, nil
}

var (
	_ fstests.SetUploadChunkSizer = (*Fs)(nil)
	_ fstests.SetUploadCutoffer   = (*Fs)(nil)
)
