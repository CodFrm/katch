// Package cache_entity 定义缓存对象的实体。
package cache_entity

// CacheObject 一条缓存记录：某个上游的某条路径对应盘上的哪一份内容。
//
// 记录和内容是分开的：文件本体按内容摘要存放，同一份字节只存一份，因此多条记录
// 可能指向同一个 Digest（一个对象在两个上游各被拉过、或同一份内容有多个别名）。
// 删记录之前必须先问「还有没有别的记录引用这份内容」，否则会把另一条记录变成
// 指向空文件的坏缓存。
type CacheObject struct {
	ID         int64 `gorm:"column:id;primary_key" json:"id"`
	UpstreamID int64 `gorm:"column:upstream_id" json:"upstream_id"`
	// Key 上游内路径（含查询串），与 UpstreamID 一起唯一确定一个对象。
	//
	// 少数请求的键末尾还缀着一段变体：同一个 manifest 路径，docker 与 OCI 的
	// 客户端靠 Accept 各要各的那一份，两份内容不能共用一条记录（键怎么拼见
	// cache_svc 的 variant.go）。没有变体可言的请求落的是路径本身，所以这一列
	// 里绝大多数行的形态和它一直以来的样子没有分别。
	Key string `gorm:"column:key" json:"key"`
	// Digest 内容摘要 sha256:<hex>，同时就是文件在缓存目录里的位置。
	Digest string `gorm:"column:digest" json:"digest"`
	Size   int64  `gorm:"column:size" json:"size"`
	// ContentType 回源时的内容类型。缓存命中要和回源给出同样的响应，
	// 少了它客户端会按别的类型解析同一份字节。
	ContentType string `gorm:"column:content_type" json:"content_type"`
	// ETag 上游成功响应里的 ETag，原样保存、命中时原样回放；上游没给时为空。
	//
	// 不为历史行推导：上游的 ETag 与缓存摘要未必是同一个标识，拿摘要冒充会让
	// 客户端拿着另一串 validator 去做条件请求，得到错误的结果（决策 5）。
	ETag string `gorm:"column:etag" json:"etag"`
	// LastModified 上游成功响应里的 Last-Modified，原样保存、命中时原样回放。
	LastModified string `gorm:"column:last_modified" json:"last_modified"`
	// Safe response metadata is persisted explicitly; arbitrary origin headers never reach disk.
	CacheControl        string `gorm:"column:cache_control" json:"cache_control"`
	OriginDate          string `gorm:"column:origin_date" json:"origin_date"`
	OriginAge           int64  `gorm:"column:origin_age" json:"origin_age"`
	OriginExpires       string `gorm:"column:origin_expires" json:"origin_expires"`
	Vary                string `gorm:"column:vary" json:"vary"`
	AcceptRanges        string `gorm:"column:accept_ranges" json:"accept_ranges"`
	ContentDisposition  string `gorm:"column:content_disposition" json:"content_disposition"`
	DockerContentDigest string `gorm:"column:docker_content_digest" json:"docker_content_digest"`
	// DockerDistributionAPIVersion 上游响应里的 Docker-Distribution-Api-Version，命中时原样
	// 回放；上游没给时为空，命中也不带。
	DockerDistributionAPIVersion string `gorm:"column:docker_distribution_api_version" json:"docker_distribution_api_version"`
	// Maven-compatible checksum headers are opaque origin metadata. They are replayed as-is
	// on hits, never derived from or used to validate the cached body.
	XChecksumMD5         string `gorm:"column:x_checksum_md5" json:"x_checksum_md5"`
	XChecksumSHA1        string `gorm:"column:x_checksum_sha1" json:"x_checksum_sha1"`
	XChecksumSHA256      string `gorm:"column:x_checksum_sha256" json:"x_checksum_sha256"`
	XChecksumSHA512      string `gorm:"column:x_checksum_sha512" json:"x_checksum_sha512"`
	StoredAt             int64  `gorm:"column:stored_at" json:"stored_at"`
	RequiresRevalidation bool   `gorm:"column:requires_revalidation" json:"requires_revalidation"`
	// Immutable 内容寻址的对象：内容永不改写，长期缓存，只由 LRU 淘汰（决策 7）。
	Immutable bool `gorm:"column:immutable" json:"immutable"`
	// Pinned 人工要求常驻，不参与淘汰。
	Pinned bool `gorm:"column:pinned" json:"pinned"`
	// ExpiresAt 可变对象的过期时刻（秒）。不可变对象是 0，表示不按时间过期。
	ExpiresAt    int64 `gorm:"column:expires_at" json:"expires_at"`
	LastAccessAt int64 `gorm:"column:last_access_at" json:"last_access_at"`
	HitCount     int64 `gorm:"column:hit_count" json:"hit_count"`
	Createtime   int64 `gorm:"column:createtime" json:"createtime"`
	Updatetime   int64 `gorm:"column:updatetime" json:"updatetime"`
}

// Expired 判断这条记录在 now（秒）是否已经过期。
//
// 不可变对象永不过期（决策 7）：它的内容按摘要寻址，改不了，也就没有「过期」
// 这回事；可变对象到点即失效，宁可多回一次源，也不能发出过期的 tag 或 InRelease。
func (c *CacheObject) Expired(now int64) bool {
	if c.ExpiresAt == 0 || (c.Immutable && c.StoredAt == 0) {
		return false
	}
	return now >= c.ExpiresAt
}

// SearchOption 缓存对象的搜索条件，供管理界面的对象搜索用。
//
// UpstreamID 为 0 表示不限上游，Keyword 为空表示不限路径。
type SearchOption struct {
	UpstreamID int64
	Keyword    string
	Offset     int
	Limit      int
}
