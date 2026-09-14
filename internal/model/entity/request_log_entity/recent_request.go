// Package request_log_entity 定义「最近请求」明细表的实体。
//
// 这张表和分钟桶（rollup_entity.TrafficRollup）不是同一种东西：那张答「这段时间
// 总共怎么样」（可加，每分钟一行），这张答「刚刚按什么顺序发生了什么」（不可加，
// 每请求一行，保留时长由设置决定）。合成一张表会让两者互相迁就。
package request_log_entity

// RecentRequest 一次拉取的明细。
//
// 它由进程内的环形缓冲每秒批量落库（记录与落库一节），因此这一层只负责把一行
// 原样存取，不在写路径上做任何聚合。
type RecentRequest struct {
	ID int64 `gorm:"column:id;primary_key" json:"id"`
	// UpstreamID 这次拉取所属的上游；查面板时按它筛选。
	UpstreamID int64 `gorm:"column:upstream_id" json:"upstream_id"`
	// At 这次拉取**结束**的时刻（秒）。不是开始时刻：同一秒内的先后由 id 兜底，
	// 而面板按时间倒序读，用开始时刻会让「慢请求」出现在比它更早结束的请求前面。
	At int64 `gorm:"column:at" json:"at"`
	// Object 上游内的路径，registry 的不含 /v2 前缀。
	Object string `gorm:"column:object" json:"object"`
	// Result 取值与 katch_requests_total 同一套：hit / miss / denied / origin_error。
	Result string `gorm:"column:result" json:"result"`
	// Bytes 发给客户端的字节数。
	Bytes int64 `gorm:"column:bytes" json:"bytes"`
	// DurationMS 这次拉取的耗时（毫秒）。
	DurationMS int64 `gorm:"column:duration_ms" json:"duration_ms"`
	// 保留期裁剪看的是 At，这两个时间戳只记录这行是什么时候写进来的。
	Createtime int64 `gorm:"column:createtime" json:"createtime"`
	Updatetime int64 `gorm:"column:updatetime" json:"updatetime"`
}
