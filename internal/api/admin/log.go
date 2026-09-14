package admin

import "github.com/cago-frame/cago/server/mux"

// RecentRequestsMaxLimit 一次最多给多少条最近请求。
//
// 这是硬上限，不是默认值：响应大小不能由调用方决定，而这块面板是「刚刚发生了
// 什么」，翻到几百条之前早就该去看别的了。
const RecentRequestsMaxLimit = 100

// RecentRequestsDefaultLimit 没指定条数时给多少条。面板上一屏的量级。
const RecentRequestsDefaultLimit = 30

// RecentRequestsRequest 查某个上游的最近请求。
//
// 过滤条件只有 upstream_id 与条数：数据来自 recent_request 这张明细表，端点上
// **没有文件名**这个参数，也不会有——面板读的是库，不碰任何文件。
type RecentRequestsRequest struct {
	mux.Meta   `path:"/admin/logs/requests" method:"GET"`
	UpstreamID int64 `form:"upstream_id" binding:"required" label:"上游"`
	Limit      int   `form:"limit" binding:"omitempty,min=1,max=100" label:"条数"`
}

// RecentRequest 一次拉取在 recent_request 里留下的那一行。
type RecentRequest struct {
	// At 这次拉取结束的时刻（秒）。
	At int64 `json:"at"`
	// Object 上游内的路径，registry 的不含 /v2 前缀——和回源时用的那一段一致。
	Object string `json:"object"`
	// Result 与 katch_requests_total 的 result 同一套取值：
	// hit / miss / denied / origin_error。
	Result string `json:"result"`
	// Bytes 发给客户端的字节数。
	Bytes int64 `json:"bytes"`
	// DurationMS 这次拉取耗时（毫秒）。
	DurationMS int64 `json:"duration_ms"`
}

// RecentRequestsResponse 最近请求，最近的在最前。
//
// 库里没有这个上游的行（新装的站、或该上游确实没被拉过）时给一个空列表而不是
// 错误：界面据此让那块面板整块不渲染。库读不出来时会由 storageAware 翻成 503，
// 和上游表查不出来时一样。
type RecentRequestsResponse struct {
	List []*RecentRequest `json:"list"`
}
