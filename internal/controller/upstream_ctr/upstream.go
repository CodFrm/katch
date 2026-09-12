// Package upstream_ctr 处理上游管理接口的请求。
package upstream_ctr

import (
	"context"

	"github.com/CodFrm/katch/internal/api/admin"
	api_upstream "github.com/CodFrm/katch/internal/api/upstream"
	"github.com/CodFrm/katch/internal/service/stat_svc"
	"github.com/CodFrm/katch/internal/service/upstream_svc"
)

// Upstream 上游管理控制器。
type Upstream struct{}

// NewUpstream 构造上游管理控制器。
func NewUpstream() *Upstream {
	return &Upstream{}
}

// List 列出全部上游。
func (u *Upstream) List(ctx context.Context, req *admin.ListUpstreamsRequest) (*admin.ListUpstreamsResponse, error) {
	return upstream_svc.Upstream().List(ctx, req)
}

// PublicList 列出公开的上游。不要密钥，是否可见由「公开首页」设置决定，
// 那道闸在路由层，见 internal/api/router.go。
//
// 名片字段来自 upstream_svc，命中率与状态来自 stat_svc，两者在这里合并而不是
// 在 upstream_svc 里：stat_svc 已经依赖 upstream_svc（它要把 upstream_id 翻成
// 主机名），反过来再依赖回去就是一个导入环。
func (u *Upstream) PublicList(ctx context.Context, req *api_upstream.ListRequest) (*api_upstream.ListResponse, error) {
	resp, err := upstream_svc.Upstream().PublicList(ctx, req)
	if err != nil {
		return nil, err
	}
	stats, err := stat_svc.Stat().PublicUpstreams(ctx)
	if err != nil {
		// 统计读不出来就整个失败，而不是给一张命中率全是 0 的表：0% 命中率
		// 看起来就是「这台镜像站没在工作」，那比少一张表更误导人。
		return nil, err
	}
	for _, item := range resp.List {
		// 统计里没有这一行只可能是两次查询之间刚加了一个上游：它确实还没有
		// 任何流量，也不在退避里，normal 加 0 命中率就是它此刻的真实状态。
		item.Status = api_upstream.StatusNormal
		if stat, ok := stats[item.Host]; ok {
			item.HitRate = stat.HitRate
			item.CacheBytes = stat.CacheBytes
			item.Status = stat.Status
		}
	}
	return resp, nil
}

// Save 新增或更新一条上游。
func (u *Upstream) Save(ctx context.Context, req *admin.SaveUpstreamRequest) (*admin.SaveUpstreamResponse, error) {
	return upstream_svc.Upstream().Save(ctx, req)
}

// Delete 删除一条上游。
func (u *Upstream) Delete(ctx context.Context, req *admin.DeleteUpstreamRequest) (*admin.DeleteUpstreamResponse, error) {
	return upstream_svc.Upstream().Delete(ctx, req)
}
