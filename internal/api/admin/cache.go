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

// CacheTreeRequest 列缓存对象目录树的一层。
//
// Path 不以 / 开头，第一段是上游主机名，其余各段对应缓存键里查询串之前的路径；
// 留空表示根（列出有缓存对象的上游主机）。每次最多返回 200 项，Offset 取上一批的
// next_offset。
type CacheTreeRequest struct {
	mux.Meta `path:"/admin/cache/tree" method:"GET"`
	Path     string `form:"path" binding:"omitempty,max=1024" label:"目录"`
	Offset   int    `form:"offset" binding:"omitempty,gte=0" label:"偏移"`
}

// CacheTreeObjectItem 目录树里的缓存对象：比对象搜索多一个主机名。
type CacheTreeObjectItem struct {
	CacheObjectItem
	Host string `json:"host"`
}

// CacheTreeNode 一层里的一个子项，Kind 是 dir 或 object。
//
// 目录行给的是其下（递归）全部对象的合计；对象行的数字看 Object，Variant 表示
// 这条记录是同一路径按 Accept 区分出来的变体之一，Name 已经去掉了变体段。
type CacheTreeNode struct {
	Kind         string               `json:"kind"`
	Name         string               `json:"name"`
	Path         string               `json:"path"`
	Count        int64                `json:"count"`
	PinnedCount  int64                `json:"pinned_count"`
	Size         int64                `json:"size"`
	LastAccessAt int64                `json:"last_access_at"`
	Variant      bool                 `json:"variant"`
	Object       *CacheTreeObjectItem `json:"object,omitempty"`
}

// CacheTreeResponse 一层的子项（目录在前、对象在后，各按名称排序）与当前目录的合计。
type CacheTreeResponse struct {
	Path        string           `json:"path"`
	TotalCount  int64            `json:"total_count"`
	TotalPinned int64            `json:"total_pinned"`
	TotalSize   int64            `json:"total_size"`
	Children    []*CacheTreeNode `json:"children"`
	HasMore     bool             `json:"has_more"`
	NextOffset  int              `json:"next_offset"`
}

// CacheTreeSearchRequest 在一个目录下按路径做不区分大小写的子串搜索。
type CacheTreeSearchRequest struct {
	mux.Meta `path:"/admin/cache/tree/search" method:"GET"`
	Path     string `form:"path" binding:"omitempty,max=1024" label:"目录"`
	Keyword  string `form:"keyword" binding:"required,max=256" label:"关键字"`
}

// CacheTreeSearchObject 一个匹配的对象。
type CacheTreeSearchObject struct {
	Name    string               `json:"name"`
	Path    string               `json:"path"`
	Variant bool                 `json:"variant"`
	Object  *CacheTreeObjectItem `json:"object"`
}

// CacheTreeSearchDir 匹配对象的一个上级目录：全部对象与其中匹配部分的合计。
// NameMatch 表示目录名本身就含关键字。
type CacheTreeSearchDir struct {
	Path         string `json:"path"`
	NameMatch    bool   `json:"name_match"`
	Count        int64  `json:"count"`
	Size         int64  `json:"size"`
	MatchedCount int64  `json:"matched_count"`
	MatchedSize  int64  `json:"matched_size"`
	LastAccessAt int64  `json:"last_access_at"`
}

// CacheTreeSearchResponse 搜索结果：最多 200 个对象，Matched 是匹配总数，
// 超出时 Truncated 为真。
type CacheTreeSearchResponse struct {
	Path      string                   `json:"path"`
	Matched   int64                    `json:"matched"`
	Truncated bool                     `json:"truncated"`
	Objects   []*CacheTreeSearchObject `json:"objects"`
	Dirs      []*CacheTreeSearchDir    `json:"dirs"`
}

// CacheTreePurgeRequest 按目录路径清除缓存对象：清掉 Path 下（递归到底）全部
// 未 pin 的对象。Path 必填——根路径横跨全部上游，不给「清空一切」留一个只填
// 根路径就能触发的形态；`..`、空段、超长同 CacheTreeRequest 一样按参数错误拒绝。
type CacheTreePurgeRequest struct {
	mux.Meta `path:"/admin/cache/tree/purge" method:"POST"`
	Path     string `json:"path" binding:"required,max=1024" label:"目录"`
}

// CacheTreePurgeResponse 清掉了几条、跳过了几条已固定的对象。
type CacheTreePurgeResponse struct {
	Removed int64 `json:"removed"`
	// Skipped 因为被 pin 而留下的条数。不报出来的话，界面只能说「清了 N 条」，
	// 看不出还有几条留在那里。
	Skipped int64 `json:"skipped"`
}

// ListCacheImagesRequest 按仓库列出协议含 registry 的上游里缓存过的镜像。
//
// 仓库名按最后一个动词段切出，开了 library_completion 的上游把 x 归并到 library/x；
// 按最后访问倒序，每页默认 50 个。Keyword 按仓库名或 tag 做不区分大小写的子串匹配。
type ListCacheImagesRequest struct {
	mux.Meta `path:"/admin/cache/images" method:"GET"`
	// UpstreamID 留空表示全部 registry 上游。
	UpstreamID int64  `form:"upstream_id" binding:"omitempty,gte=0" label:"上游"`
	Keyword    string `form:"keyword" binding:"omitempty,max=256" label:"关键字"`
	Offset     int    `form:"offset" binding:"omitempty,gte=0" label:"偏移"`
	Size       int    `form:"size" binding:"omitempty,gte=1,lte=200" label:"每页条数"`
}

// CacheImageTag 一个 manifest 引用（tag 或摘要）合并全部 Accept 变体之后的一行。
//
// Digest 取最近访问的那个变体；Expired 表示那个变体是已过过期时刻的可变 manifest，
// 下次拉取会回源；任意一个变体被 pin 时 Pinned 为真。
type CacheImageTag struct {
	Reference    string `json:"reference"`
	ByDigest     bool   `json:"by_digest"`
	Digest       string `json:"digest"`
	Variants     int    `json:"variants"`
	ObjectCount  int64  `json:"object_count"`
	PinnedCount  int64  `json:"pinned_count"`
	Pinned       bool   `json:"pinned"`
	Expired      bool   `json:"expired"`
	HitCount     int64  `json:"hit_count"`
	LastAccessAt int64  `json:"last_access_at"`
}

// CacheImageItem 一个镜像：体积、命中、对象数是仓库下全部缓存对象的合计，
// 共用层在各自镜像里各算一次。Tags 仅在关键字命中 tag 时给出命中的那些，否则为空数组。
type CacheImageItem struct {
	UpstreamID   int64            `json:"upstream_id"`
	Host         string           `json:"host"`
	Repository   string           `json:"repository"`
	TagCount     int              `json:"tag_count"`
	ObjectCount  int64            `json:"object_count"`
	PinnedCount  int64            `json:"pinned_count"`
	Size         int64            `json:"size"`
	HitCount     int64            `json:"hit_count"`
	LastAccessAt int64            `json:"last_access_at"`
	Tags         []*CacheImageTag `json:"tags"`
}

// ListCacheImagesResponse 一页镜像。
type ListCacheImagesResponse struct {
	Total      int64             `json:"total"`
	HasMore    bool              `json:"has_more"`
	NextOffset int               `json:"next_offset"`
	List       []*CacheImageItem `json:"list"`
}

// ListCacheImageTagsRequest 列出一个镜像的 tag，按最后访问倒序。
type ListCacheImageTagsRequest struct {
	mux.Meta   `path:"/admin/cache/images/tags" method:"GET"`
	UpstreamID int64  `form:"upstream_id" binding:"required,gte=1" label:"上游"`
	Repository string `form:"repository" binding:"required,max=500" label:"仓库"`
	Keyword    string `form:"keyword" binding:"omitempty,max=256" label:"关键字"`
}

// ListCacheImageTagsResponse 一个镜像的 tag 行。
type ListCacheImageTagsResponse struct {
	List []*CacheImageTag `json:"list"`
}

// PurgeCacheImageRequest 删除镜像（不给 Reference）或删除一个 tag。
//
// 删除镜像清掉仓库下（含 x 与 library/x 两种写法）全部未 pin 对象；删除 tag 只清
// 该引用的全部变体记录，不连带层。`..`、空段、超长按参数错误拒绝。
type PurgeCacheImageRequest struct {
	mux.Meta   `path:"/admin/cache/images/purge" method:"POST"`
	UpstreamID int64  `json:"upstream_id" binding:"required,gte=1" label:"上游"`
	Repository string `json:"repository" binding:"required,max=500" label:"仓库"`
	Reference  string `json:"reference" binding:"omitempty,max=256" label:"tag"`
}

// PurgeCacheImageResponse 清掉了几条、跳过了几条已固定的对象。
type PurgeCacheImageResponse struct {
	Removed int64 `json:"removed"`
	Skipped int64 `json:"skipped"`
}
