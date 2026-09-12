// Package admin 定义 /api/v1/admin/* 管理接口的请求与响应结构。
//
// 这些接口全部需要管理密钥：增删上游、改访问规则、清缓存都是破坏性操作。
// 拉取路径本身是公开的——镜像站的价值就在于任何人可以直接用。
package admin

import "github.com/cago-frame/cago/server/mux"

// UpstreamItem 一条上游在管理接口上的表示。
//
// ImmutablePatterns 在这里是 []string 而不是实体上的 PatternList：API 结构体是
// 对外契约，不该把「库里存成 JSON 文本」这个存储细节泄漏给调用方。
type UpstreamItem struct {
	ID                int64    `json:"id"`
	Host              string   `json:"host"`
	Kind              string   `json:"kind"`
	Origin            string   `json:"origin"`
	Enabled           bool     `json:"enabled"`
	ImmutablePatterns []string `json:"immutable_patterns"`
	MutableTTLSeconds int      `json:"mutable_ttl_seconds"`
	DefaultPolicy     string   `json:"default_policy"`
	LibraryCompletion bool     `json:"library_completion"`
	Note              string   `json:"note"`
	Createtime        int64    `json:"createtime"`
	Updatetime        int64    `json:"updatetime"`
}

// ListUpstreamsRequest 列出全部上游。
type ListUpstreamsRequest struct {
	mux.Meta `path:"/admin/upstreams" method:"GET"`
}

// ListUpstreamsResponse 上游列表。
//
// 不分页：上游是人工维护的白名单，规模是几十条而不是几万条，分页只会让界面上
// 「支持哪些上游」这个问题需要翻页才能答。
type ListUpstreamsResponse struct {
	List []*UpstreamItem `json:"list"`
}

// SaveUpstreamRequest 新增或更新一条上游。ID 为 0 时新增，否则更新该条。
//
// 新增和更新合成一个接口：两者的字段集合完全相同，拆开只会有两份等价的校验。
type SaveUpstreamRequest struct {
	mux.Meta `path:"/admin/upstreams" method:"POST"`
	ID       int64  `json:"id"`
	Host     string `json:"host" binding:"required" label:"上游主机名"`
	// Kind 只有两种。其余差异靠本记录上的字段表达，而不是给每类上游写一个适配器。
	Kind   string `json:"kind" binding:"required,oneof=registry static" label:"上游类别"`
	Origin string `json:"origin" binding:"required,url" label:"回源地址"`
	// Enabled 为 false 等同于不在白名单里：既不回源，也不在拉取路径上回显。
	Enabled           bool     `json:"enabled"`
	ImmutablePatterns []string `json:"immutable_patterns"`
	MutableTTLSeconds int      `json:"mutable_ttl_seconds" binding:"gte=0" label:"可变对象缓存时长"`
	// DefaultPolicy 留空时按 allow_all 处理，见 upstream_entity.PolicyAllowAll。
	DefaultPolicy     string `json:"default_policy" binding:"omitempty,oneof=allow_all deny_unless_matched" label:"默认策略"`
	LibraryCompletion bool   `json:"library_completion"`
	Note              string `json:"note"`
}

// SaveUpstreamResponse 返回该条上游的 id，新增时这是调用方唯一能拿到 id 的地方。
type SaveUpstreamResponse struct {
	ID int64 `json:"id"`
}

// DeleteUpstreamRequest 删除一条上游。
type DeleteUpstreamRequest struct {
	mux.Meta `path:"/admin/upstreams/:id" method:"DELETE"`
	ID       int64 `uri:"id"`
}

// DeleteUpstreamResponse 删除成功没有额外信息可返回。
type DeleteUpstreamResponse struct{}
