package wopan

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"

	wopansdk "github.com/OpenListTeam/wopan-sdk-go"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseCreateTime 覆盖联通 createTime 的正常值、边界值与非法值
func TestParseCreateTime(t *testing.T) {
	// 正常值：2023-06-07 21:43:51 UTC+8
	got, err := parseCreateTime("20230607214351")
	require.NoError(t, err)
	loc := time.FixedZone("UTC+8", 8*3600)
	assert.Equal(t, time.Date(2023, 6, 7, 21, 43, 51, 0, loc), got)

	// 闰日：2024-02-29
	got, err = parseCreateTime("20240229120000")
	require.NoError(t, err)
	assert.Equal(t, time.Date(2024, 2, 29, 12, 0, 0, 0, loc), got)

	// 年末跨秒
	got, err = parseCreateTime("20231231235959")
	require.NoError(t, err)
	assert.Equal(t, time.Date(2023, 12, 31, 23, 59, 59, 0, loc), got)

	// 非法值：空串、格式错误、不存在的日期
	for _, s := range []string{"", "20230607", "2023-06-07 21:43:51", "20230230120000"} {
		_, err := parseCreateTime(s)
		assert.Error(t, err, "input %q should fail to parse", s)
	}
}

// TestFileToObject 覆盖 SDK File 到 Object 的转换
func TestFileToObject(t *testing.T) {
	f := &Fs{}
	o, err := f.fileToObject("dir/a.bin", &wopansdk.File{
		Fid:        "fid123",
		Id:         "id456",
		Name:       "a.bin",
		Size:       1024,
		CreateTime: "20230607214351",
		Type:       1,
	})
	require.NoError(t, err)
	assert.Equal(t, "dir/a.bin", o.remote)
	assert.Equal(t, int64(1024), o.size)
	assert.Equal(t, "fid123", o.fid)
	assert.Equal(t, "id456", o.id)
	loc := time.FixedZone("UTC+8", 8*3600)
	assert.Equal(t, time.Date(2023, 6, 7, 21, 43, 51, 0, loc), o.modTime)

	// 非法 createTime 必须传播错误
	_, err = f.fileToObject("dir/b.bin", &wopansdk.File{
		Fid:        "fid789",
		Id:         "id000",
		Name:       "b.bin",
		CreateTime: "garbage",
		Type:       1,
	})
	assert.Error(t, err)
}

// TestHasMorePage 分页终止条件：返回条数等于 pageSize 时继续，小于时终止
func TestHasMorePage(t *testing.T) {
	assert.True(t, hasMorePage(100, 100))
	assert.False(t, hasMorePage(99, 100))
	assert.False(t, hasMorePage(0, 100))
}

// TestSpaceTypeOf 个人云/家庭云推导
func TestSpaceTypeOf(t *testing.T) {
	assert.Equal(t, wopansdk.SpaceTypePersonal, spaceTypeOf(""))
	assert.Equal(t, wopansdk.SpaceTypeFamily, spaceTypeOf("12345"))
}

// fakeNetError 模拟网络层错误
type fakeNetError struct{ timeout bool }

func (e fakeNetError) Error() string   { return "fake network error" }
func (e fakeNetError) Timeout() bool   { return e.timeout }
func (e fakeNetError) Temporary() bool { return true }

var _ net.Error = fakeNetError{}

// TestShouldRetry 错误分类：网络错误、5xx/429、直链过期、普通业务错误
func TestShouldRetry(t *testing.T) {
	// 网络层错误可重试
	assert.True(t, shouldRetry(fakeNetError{timeout: true}))
	// io.EOF 可重试
	assert.True(t, shouldRetry(io.EOF))

	// SDK 的 HTTP 状态码错误串
	assert.True(t, shouldRetry(errors.New("request failed with status: 500 Internal Server Error")))
	assert.True(t, shouldRetry(errors.New("request failed with status: 503 Service Unavailable")))
	assert.True(t, shouldRetry(errors.New("request failed with status: 429 Too Many Requests")))
	assert.True(t, shouldRetry(errors.New("partIndex: 3, failed to upload2C with http status: 502, body: x")))

	// 普通业务错误与 4xx 不重试
	assert.False(t, shouldRetry(nil))
	assert.False(t, shouldRetry(errors.New("request failed with rsp_code: 1001,rep_desc: some error")))
	assert.False(t, shouldRetry(errors.New("request failed with status: 403 Forbidden")))
	assert.False(t, shouldRetry(errors.New("request failed with status: 404 Not Found")))
}

// TestIsNotFoundErr 不存在错误的归类
func TestIsNotFoundErr(t *testing.T) {
	assert.False(t, isNotFoundErr(nil))
	assert.True(t, isNotFoundErr(errors.New("request failed with rsp_code: 1001,rep_desc: directory not exist")))
	assert.True(t, isNotFoundErr(errors.New("request failed with rsp_code: 1002,rep_desc: file NOT FOUND")))
	assert.False(t, isNotFoundErr(errors.New("request failed with status: 500")))
}

// TestNewFsRequiresToken 无 refresh_token 时 NewFs 必须在发起网络请求前报错
func TestNewFsRequiresToken(t *testing.T) {
	_, err := NewFs(t.Context(), "wopan", "", configmap.New())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refresh_token")
}

// 确认 Object 实现了必要的接口
var (
	_ fs.Object = (*Object)(nil)
	_ fs.Fs     = (*Fs)(nil)
)
