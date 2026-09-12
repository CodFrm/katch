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
	// CacheBytes 此刻缓存里存着多少字节。
	//
	// 它和上面那几个数不是一个时态：区间计数说的是「这段时间发生了什么」，
	// 缓存量说的是「现在存着什么」，所以它不随 Range 变。
	CacheBytes int64 `json:"cache_bytes"`
	// Daily 近 14 天的逐日序列，由旧到新，固定 DailyDays 个点。
	//
	// 同样不随 Range 变：区间是「看多久的合计」，趋势是首页侧栏那张固定的
	// 14 天图（spec 前台一节），两者各问各的问题。
	Daily []*DailyPoint `json:"daily"`
}

// DailyDays 逐日序列固定给多少个点。
//
// 服务端把缺的日子补成零点再给出去：一个刚上线三天的站点，图上应该是 11 个
// 零点加 3 根柱子，而不是 3 个点被拉满整张图。
const DailyDays = 14

// DailyPoint 一个自然日的流量。
//
// 只有这四个数：侧栏那张图要画的是命中率和省下的流量。denied 与 origin_errors
// 在总览里已有合计，再逐日铺开就等于把「这台站什么时候被谁打了、什么时候上游
// 挂了」的时间线公开出去，那是运营数据。
type DailyPoint struct {
	// Day 这一天 00:00:00 UTC 的秒数。
	//
	// UTC 而不是进程本地时区：分钟桶本身是 UTC 秒，跟着时区走会让同一批数据
	// 在两台部署上落到不同的天。
	Day         int64 `json:"day"`
	Requests    int64 `json:"requests"`
	Hits        int64 `json:"hits"`
	BytesServed int64 `json:"bytes_served"`
	BytesOrigin int64 `json:"bytes_origin"`
}
