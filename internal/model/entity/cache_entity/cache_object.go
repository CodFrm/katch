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
	if c.Immutable || c.ExpiresAt == 0 {
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

// 缓存键的变体段：路径（含查询串）之后是 0x1F，再接 VariantTag 与 VariantDigestLen 位
// 十六进制摘要。写键的一侧（cache_svc）与按键搜索的一侧（cache_repo）共用这一份形状。
const (
	// VariantTag 变体段在分隔符之后的固定开头。
	VariantTag = "accept="
	// VariantDigestLen 变体段里摘要的十六进制位数。
	VariantDigestLen = 16
)

// UpstreamTreeStat 目录树根上一个上游的合计。
type UpstreamTreeStat struct {
	UpstreamID   int64 `gorm:"column:upstream_id"`
	Count        int64 `gorm:"column:count"`
	Pinned       int64 `gorm:"column:pinned"`
	Size         int64 `gorm:"column:size"`
	LastAccessAt int64 `gorm:"column:last_access_at"`
}

// PrefixTreeStat 一个目录（键前缀）下的合计。
//
// Objects 与 Dirs 只数直接的一层，供分页算「目录在前、对象在后」的偏移；
// 其余几项是递归到底的合计。
type PrefixTreeStat struct {
	Count        int64 `gorm:"column:count"`
	Pinned       int64 `gorm:"column:pinned"`
	Size         int64 `gorm:"column:size"`
	LastAccessAt int64 `gorm:"column:last_access_at"`
	Objects      int64 `gorm:"column:objects"`
	Dirs         int64 `gorm:"column:dirs"`
}

// TreeDir 一层里的一个子目录与它之下（递归）的合计。
type TreeDir struct {
	Name         string `gorm:"column:name"`
	Count        int64  `gorm:"column:count"`
	Pinned       int64  `gorm:"column:pinned"`
	Size         int64  `gorm:"column:size"`
	LastAccessAt int64  `gorm:"column:last_access_at"`
}

// TreeOption 列目录树的一层。
//
// Prefix 是键的前缀，以 / 开头也以 / 结尾（上游根就是 "/"）。
type TreeOption struct {
	UpstreamID int64
	Prefix     string
	Offset     int
	Limit      int
}

// TreeSearchOption 在一个目录下搜索对象。
//
// UpstreamID 大于 0 时限定在该上游的 Prefix 之下；为 0 表示在根上搜索，
// 范围是 UpstreamIDs 列出的上游，HostMatched 里的上游（主机名命中关键字）整体算命中。
type TreeSearchOption struct {
	UpstreamID  int64
	Prefix      string
	UpstreamIDs []int64
	HostMatched []int64
	Keyword     string
	Limit       int
}

// TreeMatchOption 搜索结果里一个目录的合计。
//
// Prefix 是这个目录的键前缀；SearchPrefix 是发起搜索的那个目录的键前缀
// （根上搜索时为空），关键字只和它之下的部分比。AllMatched 表示这个上游的
// 主机名已经命中，目录下的对象全部算命中。
type TreeMatchOption struct {
	UpstreamID   int64
	Prefix       string
	SearchPrefix string
	Keyword      string
	AllMatched   bool
}

// TreeMatchStat 搜索结果里一个目录的合计，以及其中命中的部分。
type TreeMatchStat struct {
	Count        int64 `gorm:"column:count"`
	Size         int64 `gorm:"column:size"`
	LastAccessAt int64 `gorm:"column:last_access_at"`
	MatchedCount int64 `gorm:"column:matched_count"`
	MatchedSize  int64 `gorm:"column:matched_size"`
}
