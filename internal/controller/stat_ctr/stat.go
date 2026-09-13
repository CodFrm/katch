// Package stat_ctr 处理统计接口的请求。
package stat_ctr

import (
	"context"

	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/api/stat"
	"github.com/CodFrm/katch/internal/service/stat_svc"
)

// Stat 统计控制器。
type Stat struct{}

// NewStat 构造统计控制器。
func NewStat() *Stat {
	return &Stat{}
}

// Overview 全站总览。公开接口，不要密钥。
func (s *Stat) Overview(ctx context.Context, req *stat.OverviewRequest) (*stat.OverviewResponse, error) {
	return stat_svc.Stat().Overview(ctx, req)
}

// ByUpstream 按上游的统计。要密钥：单个上游的量是运营数据，
// 和「支持哪些上游」这种站点名片不是一回事。
func (s *Stat) ByUpstream(ctx context.Context, req *admin.UpstreamStatsRequest) (*admin.UpstreamStatsResponse, error) {
	return stat_svc.Stat().ByUpstream(ctx, req)
}

// UpstreamSeries 单个上游的按小时时序。要密钥，理由同 ByUpstream：
// 逐小时的量比区间合计还细，更是运营数据。
func (s *Stat) UpstreamSeries(ctx context.Context, req *admin.UpstreamSeriesRequest) (*admin.UpstreamSeriesResponse, error) {
	return stat_svc.Stat().UpstreamSeries(ctx, req)
}
