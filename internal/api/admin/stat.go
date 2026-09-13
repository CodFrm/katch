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

// SeriesBucketSeconds 时序的桶宽：一小时。
//
// 固定值而不是跟着 Range 变：换区间只该改「看多久」，桶宽跟着变会让 24 小时
// 和 7 天两张图上的一根柱子代表不同的时间跨度，眼睛对不上，也没法对比。
const SeriesBucketSeconds = 3600

// UpstreamSeriesRequest 查单个上游的按小时时序。Range 留空按 24h 处理。
type UpstreamSeriesRequest struct {
	mux.Meta   `path:"/admin/stats/upstreams/series" method:"GET"`
	UpstreamID int64  `form:"upstream_id" binding:"required" label:"上游"`
	Range      string `form:"range" binding:"omitempty,oneof=24h 7d 30d" label:"统计区间"`
}

// UpstreamSeriesPoint 一个小时桶里的量。
//
// 六个计数一个不少：上游详情要画的是命中/回源的堆叠柱加回源原因分解，
// 少给一个就得为同一张图再打一次接口，两次请求之间的时刻还会对不上。
// 回源原因分解要的那四个也在这里，理由同上——它和堆叠图是同一份序列。
type UpstreamSeriesPoint struct {
	// Bucket 这个小时的起点（秒，UTC）。
	//
	// UTC 而不是进程本地时区：分钟桶本身是 UTC 秒，跟着时区走会让同一批数据
	// 在两台部署上落到不同的小时，换算成本地时间是界面的事。
	Bucket       int64 `json:"bucket"`
	Requests     int64 `json:"requests"`
	Hits         int64 `json:"hits"`
	Denied       int64 `json:"denied"`
	OriginErrors int64 `json:"origin_errors"`
	BytesServed  int64 `json:"bytes_served"`
	BytesOrigin  int64 `json:"bytes_origin"`
	// 四个回源原因：从没缓存过、TTL 过期、被淘汰、上游 digest 变更。
	// 四项之和就是这个小时的未命中数，界面按这个分母算占比。
	MissFirst   int64 `json:"miss_first"`
	MissTTL     int64 `json:"miss_ttl"`
	MissEvicted int64 `json:"miss_evicted"`
	MissChanged int64 `json:"miss_changed"`
}

// UpstreamSeriesResponse 一个上游在这段区间里的逐小时序列。
//
// 由旧到新、缺的小时补零、固定 (To-From)/SeriesBucketSeconds 个点：一个刚加
// 上的上游应该是一排零点加一两根柱子，而不是一根柱子被拉满整张图。
type UpstreamSeriesResponse struct {
	// Range 实际生效的区间，请求没给时回显默认值。
	Range string `json:"range"`
	// From、To 这次序列的左闭右开边界（秒），都落在小时边界上。
	From int64 `json:"from"`
	To   int64 `json:"to"`
	// BucketSeconds 桶宽（秒），恒为 SeriesBucketSeconds。
	BucketSeconds int64                  `json:"bucket_seconds"`
	List          []*UpstreamSeriesPoint `json:"list"`
}
