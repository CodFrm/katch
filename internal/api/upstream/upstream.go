// Package upstream 定义公开上游接口的请求与响应结构。
//
// 这一组接口不要密钥：首页页脚的上游表要回答「支不支持我要的东西」，那是站点的
// 名片。它和 /api/v1/admin/upstreams 是两份结构而不是同一份加字段过滤——同一个
// 结构体既服务后台又服务首页时，日后往上加一个字段就会默认公开出去，而那个默认
// 值错得没有任何征兆。
package upstream

import (
	"github.com/cago-frame/cago/server/mux"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
)

// 上游此刻的可服务状态。降级是内存里的退避状态（决策 17），不落库——它说的是
// 「现在」，重启后重新探测。
const (
	// StatusNormal 正常服务。
	StatusNormal = "normal"
	// StatusDegraded 正处在回源退避里，界面上标为「限流中/降级」。
	StatusDegraded = "degraded"
)

// Item 公开列表里的一条上游。
//
// 前三个字段是使用者自己拼拉取地址时用得上的：主机名是路径第一段，协议集合决定
// 用 docker 还是 curl 还是 git clone，library_completion 决定 docker.io 上的官方
// 镜像能不能省掉 library/ 前缀。后面两个回答的是「这个上游现在好不好用」——首页页脚那张表
// 要答的正是「支不支持我要的东西」（spec 前台一节）。回源地址、默认策略、
// 不可变模式、备注都是运营数据，留在管理接口那一侧。
type Item struct {
	Host string `json:"host"`
	// Protocols 这条上游开着的协议，取值见 upstream_entity 的 Protocol* 常量。
	Protocols         []string                       `json:"protocols"`
	PackageProfile    upstream_entity.PackageProfile `json:"package_profile"`
	LibraryCompletion bool                           `json:"library_completion"`
	// HitRate 近 24 小时的命中率，取值 0~1。
	//
	// 由计数推导而不是存一个比值（可观测性一节），口径与后台那张健康矩阵同一套。
	HitRate float64 `json:"hit_rate"`
	// CacheBytes 这个上游此刻在缓存里占了多少字节，没缓存过是 0。
	CacheBytes int64 `json:"cache_bytes"`
	// Status 此刻的可服务状态，StatusNormal 或 StatusDegraded。
	Status string `json:"status"`
}

// ListRequest 列出公开的上游。
type ListRequest struct {
	mux.Meta `path:"/upstreams" method:"GET"`
}

// ListResponse 公开的上游列表，只含启用中的记录。
//
// 停用的上游不在里面：拉取路径上「停用」和「没有这条记录」是同一件事（决策 6），
// 首页上也必须是同一件事，否则表里列着的上游拉下来是 404。
type ListResponse struct {
	List []*Item `json:"list"`
}
