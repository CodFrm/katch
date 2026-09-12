package admin

import "github.com/cago-frame/cago/server/mux"

// CacheObjectItem 一个缓存对象在管理接口上的表示。
//
// 这里给的是运维要看的全部：多大、是不是不可变、有没有被 pin、什么时候过期、
// 最后一次被谁读过。摘要也给——「这两条路径其实是同一份内容」只有摘要答得出。
type CacheObjectItem struct {
	ID         int64  `json:"id"`
	UpstreamID int64  `json:"upstream_id"`
	Key        string `json:"key"`
	Digest     string `json:"digest"`
	Size       int64  `json:"size"`
	// Immutable 内容寻址的对象，长期缓存、只由 LRU 淘汰。
	Immutable bool `json:"immutable"`
	// Pinned 人工要求常驻：既不参与淘汰，也不会被按上游的批量清除带走。
	Pinned bool `json:"pinned"`
	// ExpiresAt 可变对象的过期时刻（秒），不可变对象是 0。
	ExpiresAt    int64 `json:"expires_at"`
	LastAccessAt int64 `json:"last_access_at"`
	HitCount     int64 `json:"hit_count"`
	Createtime   int64 `json:"createtime"`
	Updatetime   int64 `json:"updatetime"`
}

// SearchCacheObjectsRequest 按关键字搜索缓存对象。
//
// 分页是必须的，和上游列表相反：缓存对象是拉取路径自己长出来的，一个跑了几天的
// 镜像站就有几十万条，一次全捞回来既打爆界面也打爆库。
type SearchCacheObjectsRequest struct {
	mux.Meta `path:"/admin/cache/objects" method:"GET"`
	// UpstreamID 留空表示不限上游。
	UpstreamID int64 `form:"upstream_id" binding:"omitempty,gte=0" label:"上游"`
	// Keyword 按对象路径模糊匹配，留空表示不限路径。
	Keyword string `form:"keyword" binding:"omitempty,max=256" label:"关键字"`
	Page    int    `form:"page" binding:"omitempty,gte=1" label:"页码"`
	Size    int    `form:"size" binding:"omitempty,gte=1,lte=200" label:"每页条数"`
}

// SearchCacheObjectsResponse 搜索结果。
//
// Page 与 Size 是归一化之后的值（请求里留空时由服务端兜底），不回声一份的话，
// 调用方按自己传的参数画分页器会画错。
type SearchCacheObjectsResponse struct {
	List  []*CacheObjectItem `json:"list"`
	Total int64              `json:"total"`
	Page  int                `json:"page"`
	Size  int                `json:"size"`
}

// PurgeCacheRequest 清缓存：给 ID 清一条，给 UpstreamID 清整个上游。
//
// 两者合在一个端点而不是拆成 DELETE /objects/:id 与 DELETE /objects?upstream_id=：
// 它们是同一个操作的两个取值范围，拆开会有两份等价的实现和两份等价的返回口径。
// 两个都不给时报错——不给「清空一切」留一个不写参数就能触发的形态。
type PurgeCacheRequest struct {
	mux.Meta   `path:"/admin/cache/purge" method:"POST"`
	ID         int64 `json:"id" binding:"omitempty,gte=1" label:"缓存对象"`
	UpstreamID int64 `json:"upstream_id" binding:"omitempty,gte=1" label:"上游"`
}

// PurgeCacheResponse 清掉了几条、跳过了几条。
type PurgeCacheResponse struct {
	Removed int64 `json:"removed"`
	// Skipped 按上游清除时因为被 pin 而留下的条数。不报出来的话，界面只能说
	// 「清了 N 条」，看不出还有几条留在那里，看起来就像清缓存没生效。
	Skipped int64 `json:"skipped"`
}

// PinCacheObjectRequest 钉住或放开一个缓存对象。
//
// pin 与 unpin 是同一个字段的两个取值，不拆成两个端点：拆开之后「当前是什么状态」
// 要靠调用哪个端点来表达，而界面上那是一个开关。
type PinCacheObjectRequest struct {
	mux.Meta `path:"/admin/cache/objects/:id/pin" method:"POST"`
	ID       int64 `uri:"id" binding:"required,gte=1" label:"缓存对象"`
	Pinned   bool  `json:"pinned"`
}

// PinCacheObjectResponse 钉住成功没有额外信息可返回。
type PinCacheObjectResponse struct{}
