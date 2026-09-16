// Package admin 定义 /api/v1/admin/* 管理接口的请求与响应结构。
//
// 这些接口全部需要管理密钥：增删上游、改访问规则、清缓存都是破坏性操作。
// 拉取路径本身是公开的——镜像站的价值就在于任何人可以直接用。
package admin

import (
	"github.com/cago-frame/cago/server/mux"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
)

// UpstreamItem 一条上游在管理接口上的表示。
//
// ImmutablePatterns 在这里是 []string 而不是实体上的 PatternList：API 结构体是
// 对外契约，不该把「库里存成 JSON 文本」这个存储细节泄漏给调用方。
type UpstreamItem struct {
	ID   int64  `json:"id"`
	Host string `json:"host"`
	// Protocols 这条上游开着的协议，取值见 upstream_entity 的 Protocol* 常量。
	Protocols         []string                       `json:"protocols"`
	PackageProfile    upstream_entity.PackageProfile `json:"package_profile"`
	Origin            string                         `json:"origin"`
	Enabled           bool                           `json:"enabled"`
	ImmutablePatterns []string                       `json:"immutable_patterns"`
	MutableTTLSeconds int                            `json:"mutable_ttl_seconds"`
	DefaultPolicy     string                         `json:"default_policy"`
	LibraryCompletion bool                           `json:"library_completion"`
	Note              string                         `json:"note"`
	Createtime        int64                          `json:"createtime"`
	Updatetime        int64                          `json:"updatetime"`
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

// UpstreamSpec 一条上游的可写字段，新增与整条替换共用的那一份。
//
// 它没有标签，也不出现在任何请求体上：业务层只认这一个结构，于是「一次写入要落
// 哪些字段」在这一层只有一处定义。两个请求结构体各自平铺一份带标签的字段，是被
// 框架逼出来的——muxclient 只遍历顶层带 json/form/uri 标签的字段，嵌进去的结构体
// 在序列化请求时会被整个跳过，那会让所有 Go 侧调用方发出空请求体。
type UpstreamSpec struct {
	Host string
	// Protocols 这条上游开着的协议，非空。一条记录可以同时开多个。
	Protocols []string
	// PackageProfile 是 static 传输之上的包管理器语义；省略时为 none。
	PackageProfile upstream_entity.PackageProfile
	// Origin 回源地址。
	Origin string
	// Enabled 为 false 等同于不在白名单里：既不回源，也不在拉取路径上回显，
	// 连已经躺在缓存里的副本都不再发出去。
	Enabled           bool
	ImmutablePatterns []string
	MutableTTLSeconds int
	DefaultPolicy     string
	LibraryCompletion bool
	Note              string
}

// SaveUpstreamRequest 新登记一条上游。
//
// 只负责新增：改一条已存在的上游走 PUT /admin/upstreams/:id。两者分开是因为
// 「这条记录必须已经存在」是更新独有的前提——合成一个接口时，一个拿着过期列表的
// 调用方本想改第 7 条，id 对不上就会悄悄多出一条新上游。
type SaveUpstreamRequest struct {
	mux.Meta `path:"/admin/upstreams" method:"POST"`
	Host     string `json:"host" binding:"required" label:"上游主机名"`
	// Protocols 这条上游开着的协议，至少一个。协议之外的差异靠本记录上的字段
	// 表达，而不是给每类上游写一个适配器。
	//
	// 空集合被 required 挡住而不是当成「什么都开」：判定的默认值必须是拒绝。
	Protocols      []string                       `json:"protocols" binding:"required,min=1,dive,oneof=registry static git" label:"上游协议"`
	PackageProfile upstream_entity.PackageProfile `json:"package_profile" binding:"omitempty,oneof=none npm pypi goproxy maven cargo nuget rubygems apt rpm apk composer homebrew" label:"包管理器配置"`
	Origin         string                         `json:"origin" binding:"required,url" label:"回源地址"`
	// Enabled 见 UpstreamSpec.Enabled。
	Enabled           bool     `json:"enabled"`
	ImmutablePatterns []string `json:"immutable_patterns"`
	MutableTTLSeconds int      `json:"mutable_ttl_seconds" binding:"gte=0" label:"可变对象缓存时长"`
	// DefaultPolicy 留空时按 allow_all 处理，见 upstream_entity.PolicyAllowAll。
	DefaultPolicy     string `json:"default_policy" binding:"omitempty,oneof=allow_all deny_unless_matched" label:"默认策略"`
	LibraryCompletion bool   `json:"library_completion"`
	Note              string `json:"note"`
}

// Spec 这次请求要落的字段。
func (r *SaveUpstreamRequest) Spec() *UpstreamSpec {
	return &UpstreamSpec{
		Host: r.Host, Protocols: r.Protocols, PackageProfile: r.PackageProfile,
		Origin: r.Origin, Enabled: r.Enabled,
		ImmutablePatterns: r.ImmutablePatterns, MutableTTLSeconds: r.MutableTTLSeconds,
		DefaultPolicy: r.DefaultPolicy, LibraryCompletion: r.LibraryCompletion, Note: r.Note,
	}
}

// SaveUpstreamResponse 返回该条上游的 id，新增时这是调用方唯一能拿到 id 的地方。
type SaveUpstreamResponse struct {
	ID int64 `json:"id"`
}

// UpdateUpstreamRequest 整条替换一条已存在的上游，启停也走它。
//
// 是 PUT 而不是 PATCH：请求体就是这条上游接下来的**全貌**，服务端不必去分辨
// 「这个字段是没给，还是给了零值」。停用因此不需要第二个端点——把 enabled 翻过来
// 连同整条写回去即可，而按字段打补丁的 PATCH 恰恰会在 enabled=false 这里踩中零值：
// 缺省与 false 在 JSON 里长得一样，停用会被静默丢掉，界面上停了、拉取路径上还活着。
//
// 字段与 SaveUpstreamRequest 逐字相同，理由见 UpstreamSpec。
type UpdateUpstreamRequest struct {
	mux.Meta       `path:"/admin/upstreams/:id" method:"PUT"`
	ID             int64                          `uri:"id"`
	Host           string                         `json:"host" binding:"required" label:"上游主机名"`
	Protocols      []string                       `json:"protocols" binding:"required,min=1,dive,oneof=registry static git" label:"上游协议"`
	PackageProfile upstream_entity.PackageProfile `json:"package_profile" binding:"omitempty,oneof=none npm pypi goproxy maven cargo nuget rubygems apt rpm apk composer homebrew" label:"包管理器配置"`
	Origin         string                         `json:"origin" binding:"required,url" label:"回源地址"`
	// Enabled 见 UpstreamSpec.Enabled。
	Enabled           bool     `json:"enabled"`
	ImmutablePatterns []string `json:"immutable_patterns"`
	MutableTTLSeconds int      `json:"mutable_ttl_seconds" binding:"gte=0" label:"可变对象缓存时长"`
	DefaultPolicy     string   `json:"default_policy" binding:"omitempty,oneof=allow_all deny_unless_matched" label:"默认策略"`
	LibraryCompletion bool     `json:"library_completion"`
	Note              string   `json:"note"`
}

// Spec 这次请求要落的字段。
func (r *UpdateUpstreamRequest) Spec() *UpstreamSpec {
	return &UpstreamSpec{
		Host: r.Host, Protocols: r.Protocols, PackageProfile: r.PackageProfile,
		Origin: r.Origin, Enabled: r.Enabled,
		ImmutablePatterns: r.ImmutablePatterns, MutableTTLSeconds: r.MutableTTLSeconds,
		DefaultPolicy: r.DefaultPolicy, LibraryCompletion: r.LibraryCompletion, Note: r.Note,
	}
}

// UpdateUpstreamResponse 返回被改的那条上游的 id。
type UpdateUpstreamResponse struct {
	ID int64 `json:"id"`
}

// DeleteUpstreamRequest 删除一条上游。
type DeleteUpstreamRequest struct {
	mux.Meta `path:"/admin/upstreams/:id" method:"DELETE"`
	ID       int64 `uri:"id"`
}

// DeleteUpstreamResponse 删除成功没有额外信息可返回。
type DeleteUpstreamResponse struct{}
