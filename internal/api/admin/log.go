package admin

import "github.com/cago-frame/cago/server/mux"

// RecentRequestsMaxLimit 一次最多给多少条最近请求。
//
// 这是硬上限，不是默认值：响应大小不能由调用方决定，而这块面板是「刚刚发生了
// 什么」，翻到几百条之前早就该去看日志文件本身了。
const RecentRequestsMaxLimit = 100

// RecentRequestsDefaultLimit 没指定条数时给多少条。面板上一屏的量级。
const RecentRequestsDefaultLimit = 30

// RecentRequestsRequest 查某个上游的最近请求。
//
// 参数里**没有文件名**，也不会有：日志文件的位置只由 configs/config.yaml 的
// logger.logFile 决定。让调用方指定读哪个文件，等于给管理接口开一条任意文件
// 读取的路——密钥被拿到过一次，整台机器的文件就都跟着出去了。
type RecentRequestsRequest struct {
	mux.Meta   `path:"/admin/logs/requests" method:"GET"`
	UpstreamID int64 `form:"upstream_id" binding:"required" label:"上游"`
	Limit      int   `form:"limit" binding:"omitempty,min=1,max=100" label:"条数"`
}

// RecentRequest 一次拉取在日志里留下的那一行。
//
// 它不来自数据库：每请求写库会把拉取热路径拖进事务（决策 16），而这几个字段
// 本来就是结构化日志已经记着的东西。
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

// RecentRequestsResponse 最近请求的有界尾部，最近的在最前。
//
// 读不到日志（没开落盘、文件被轮转走、目录权限变了）时给一个空列表而不是错误：
// 这块面板是排障的辅助，它消失总好过让整屏管理界面挂在一句「打不开文件」上。
type RecentRequestsResponse struct {
	List []*RecentRequest `json:"list"`
}
