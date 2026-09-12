// Package upstream 定义公开上游接口的请求与响应结构。
//
// 这一组接口不要密钥：首页页脚的上游表要回答「支不支持我要的东西」，那是站点的
// 名片。它和 /api/v1/admin/upstreams 是两份结构而不是同一份加字段过滤——同一个
// 结构体既服务后台又服务首页时，日后往上加一个字段就会默认公开出去，而那个默认
// 值错得没有任何征兆。
package upstream

import "github.com/cago-frame/cago/server/mux"

// Item 公开列表里的一条上游。
//
// 只有三个字段，且都是使用者自己拼拉取地址时用得上的：主机名是路径第一段，
// 类别决定用 docker 还是 curl，library_completion 决定 docker.io 上的官方镜像
// 能不能省掉 library/ 前缀。回源地址、默认策略、不可变模式、备注都是运营数据，
// 留在管理接口那一侧。
type Item struct {
	Host              string `json:"host"`
	Kind              string `json:"kind"`
	LibraryCompletion bool   `json:"library_completion"`
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
