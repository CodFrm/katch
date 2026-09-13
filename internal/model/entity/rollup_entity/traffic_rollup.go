// Package rollup_entity 定义流量分钟桶的实体。
package rollup_entity

// TrafficRollup 一个上游在某一整分钟内的流量累计。
//
// 每分钟由进程内计数器落一行（决策 16），界面上的 24 小时 / 7 天 / 30 天区间
// 都从这张表聚合——每个请求写一次库会把热路径拖进事务，而 Prometheus 是可选的
// 外部组件，不能让「看自己的命中率」依赖它。
type TrafficRollup struct {
	ID         int64 `gorm:"column:id;primary_key" json:"id"`
	UpstreamID int64 `gorm:"column:upstream_id" json:"upstream_id"`
	// Bucket 整分钟的起点（秒）。与 UpstreamID 一起唯一确定一行。
	Bucket int64 `gorm:"column:bucket" json:"bucket"`
	// Requests 这一分钟的请求总数。
	//
	// 没有 misses 列：未命中数是 Requests 减去其余三项，多存一列就多一处可以
	// 和总数对不上的地方（可观测性一节：比值与冗余量都由读取方推导）。
	Requests     int64 `gorm:"column:requests" json:"requests"`
	Hits         int64 `gorm:"column:hits" json:"hits"`
	Denied       int64 `gorm:"column:denied" json:"denied"`
	OriginErrors int64 `gorm:"column:origin_errors" json:"origin_errors"`
	// BytesServed 发给客户端的字节数。
	BytesServed int64 `gorm:"column:bytes_served" json:"bytes_served"`
	// BytesOrigin 其中来自上游的字节数，两者之差就是这个镜像站省下的流量。
	BytesOrigin int64 `gorm:"column:bytes_origin" json:"bytes_origin"`
	// 四个回源原因：首次拉取、TTL 过期、被淘汰、上游 digest 变更。
	// 它们加起来正好是未命中数，所以仍然没有 misses 列——上面那条理由没变。
	//
	// 做成这张表上的四列而不是一张每请求的明细表：界面只需要占比，而占比是
	// 可加的；决策 16 否掉每请求写库的理由（把热路径拖进事务）在这里同样成立。
	MissFirst   int64 `gorm:"column:miss_first" json:"miss_first"`
	MissTTL     int64 `gorm:"column:miss_ttl" json:"miss_ttl"`
	MissEvicted int64 `gorm:"column:miss_evicted" json:"miss_evicted"`
	MissChanged int64 `gorm:"column:miss_changed" json:"miss_changed"`
	Createtime  int64 `gorm:"column:createtime" json:"createtime"`
	Updatetime  int64 `gorm:"column:updatetime" json:"updatetime"`
}

// Totals 一段区间的聚合结果。
//
// 单独一个结构体而不是复用 TrafficRollup：聚合结果没有 id，也没有属于自己的
// 那一分钟，混用会让调用方拿到一个 Bucket 恒为 0 的「记录」。
type Totals struct {
	// UpstreamID 全站聚合时为 0。
	UpstreamID   int64 `gorm:"column:upstream_id" json:"upstream_id"`
	Requests     int64 `gorm:"column:requests" json:"requests"`
	Hits         int64 `gorm:"column:hits" json:"hits"`
	Denied       int64 `gorm:"column:denied" json:"denied"`
	OriginErrors int64 `gorm:"column:origin_errors" json:"origin_errors"`
	BytesServed  int64 `gorm:"column:bytes_served" json:"bytes_served"`
	BytesOrigin  int64 `gorm:"column:bytes_origin" json:"bytes_origin"`
}
