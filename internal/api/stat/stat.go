// Package stat 定义公开统计接口的请求与响应结构。
//
// 这一组接口不要密钥：首页要展示这个镜像站替使用者省下了多少流量、缓存有没有
// 在起作用，那是站点的名片。它只给全站的合计，不暴露单条路径或单个调用方。
package stat

import "github.com/cago-frame/cago/server/mux"

// OverviewRequest 查站点总览。Range 留空按 24h 处理。
type OverviewRequest struct {
	mux.Meta `path:"/stats/overview" method:"GET"`
	Range    string `form:"range" binding:"omitempty,oneof=24h 7d 30d" label:"统计区间"`
}

// OverviewResponse 一段区间的流量合计。
//
// 没有命中率字段：它是 hits / (hits + misses) 的推导值，服务端多给一个数，
// 就多一处会和这几个计数对不上的地方（可观测性一节）。未命中数同理，
// 由 Requests 减去其余三项得到。
type OverviewResponse struct {
	// Range 实际生效的区间，请求没给时回显默认值。
	Range string `json:"range"`
	// From、To 这次聚合的左闭右开边界（秒），让界面能标出「统计自什么时候起」。
	From         int64 `json:"from"`
	To           int64 `json:"to"`
	Requests     int64 `json:"requests"`
	Hits         int64 `json:"hits"`
	Denied       int64 `json:"denied"`
	OriginErrors int64 `json:"origin_errors"`
	BytesServed  int64 `json:"bytes_served"`
	BytesOrigin  int64 `json:"bytes_origin"`
}
