// util.go 集中放置 wopan backend 的纯逻辑：不发起网络请求，便于单元测试。

package wopan

import (
	"strings"
	"time"

	wopansdk "github.com/OpenListTeam/wopan-sdk-go"
	"github.com/rclone/rclone/fs/fserrors"
)

const (
	// wopanCreateTimeLayout 是联通 API createTime 字段的时间格式（秒级精度）
	wopanCreateTimeLayout = "20060102150405"

	// wopanPageSize 是 QueryAllFiles 的分页大小，OpenList 驱动同样使用 100
	wopanPageSize = 100

	// wopanRootFolderID 是联通根目录的固定 ID 约定
	wopanRootFolderID = "0"
)

// wopanTimeZone 固定使用 UTC+8：联通服务器返回的 createTime 基于东八区
var wopanTimeZone = time.FixedZone("UTC+8", 8*3600)

// parseCreateTime 解析联通 API 的 createTime 字段（如 "20230607214351"）
func parseCreateTime(s string) (time.Time, error) {
	return time.ParseInLocation(wopanCreateTimeLayout, s, wopanTimeZone)
}

// hasMorePage 分页终止判定：返回条数达到 pageSize 时认为可能还有下一页，
// 否则已到末页
func hasMorePage(got, pageSize int) bool {
	return got >= pageSize
}

// spaceTypeOf 由 familyId 推导空间类型：familyId 为空为个人云，否则为家庭云
func spaceTypeOf(familyID string) string {
	if familyID == "" {
		return wopansdk.SpaceTypePersonal
	}
	return wopansdk.SpaceTypeFamily
}

// shouldRetry 判断 SDK 返回的错误是否值得重试。
//
// SDK 内部用 fmt.Errorf 编码错误，HTTP 状态码只能出现在错误串里，形如：
//   - "request failed with status: 500 Internal Server Error"（dispatcher 请求）
//   - "partIndex: 3, failed to upload2C with http status: 503, body: ..."（分片上传）
//
// 因此按 "status: 5"/"status: 429" 前缀匹配覆盖 5xx 与限流；
// 网络层错误（net.Error、io.EOF 等）交给 fserrors.ShouldRetry 判定。
func shouldRetry(err error) bool {
	if err == nil {
		return false
	}
	if fserrors.ShouldRetry(err) {
		return true
	}
	msg := err.Error()
	if strings.Contains(msg, "status: 429") {
		return true
	}
	if strings.Contains(msg, "status: 5") {
		return true
	}
	return false
}

// isNotFoundErr 把 SDK 的"对象/目录不存在"业务错误归类为 NotFound 语义。
// 联通 API 的不存在错误通过 rsp_desc 文本表达，按常见措辞匹配。
func isNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, kw := range []string{"not exist", "not found", "不存在"} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}
