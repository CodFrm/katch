package admin

import "github.com/cago-frame/cago/server/mux"

// UpstreamStatsRequest 按上游查一段区间的流量。Range 留空按 24h 处理。
type UpstreamStatsRequest struct {
	mux.Meta `path:"/admin/stats/upstreams" method:"GET"`
	Range    string `form:"range" binding:"omitempty,oneof=24h 7d 30d" label:"统计区间"`
}

// UpstreamStatItem 一个上游在这段区间里的表现。
type UpstreamStatItem struct {
	UpstreamID   int64  `json:"upstream_id"`
	Host         string `json:"host"`
	Requests     int64  `json:"requests"`
	Hits         int64  `json:"hits"`
	Denied       int64  `json:"denied"`
	OriginErrors int64  `json:"origin_errors"`
	BytesServed  int64  `json:"bytes_served"`
	BytesOrigin  int64  `json:"bytes_origin"`
	// Degraded 这个上游此刻正处在回源退避里，界面上标为「降级」。
	//
	// 它是内存态而不是区间统计：退避是「现在是什么情况」，进程重启后重新探测
	// （决策 17），所以它不来自 traffic_rollup，也不该被写进去。
	Degraded bool `json:"degraded"`
	// RetryAt 下一次放行探测的时刻（秒），Degraded 为假时是 0。
	RetryAt int64 `json:"retry_at"`
}

// UpstreamStatsResponse 每个上游一行。
//
// 列出全部上游而不只是有流量的那些：界面上的健康矩阵要能显示「这个上游一次
// 请求也没有」，只回有数据的行会让刚加的上游在矩阵里凭空消失。
type UpstreamStatsResponse struct {
	Range string              `json:"range"`
	From  int64               `json:"from"`
	To    int64               `json:"to"`
	List  []*UpstreamStatItem `json:"list"`
}
