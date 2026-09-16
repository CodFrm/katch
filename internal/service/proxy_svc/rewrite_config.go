package proxy_svc

import (
	"context"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
	"github.com/CodFrm/katch/internal/service/setting_svc"
)

// RewriteUpstream 是一个启用主机参与 metadata rewrite 的配置。
type RewriteUpstream struct {
	Profile    upstream_entity.PackageProfile
	Transports upstream_entity.ProtocolSet
}

// RewriteSnapshot 把公开站点地址、代数和可用主机固定在同一个配置版本。
type RewriteSnapshot struct {
	SiteBaseURL string
	Generation  int64
	Upstreams   map[string]RewriteUpstream
}

// RewriteConfigSource 提供 metadata rewrite 一次请求所需的完整配置快照。
type RewriteConfigSource interface {
	Snapshot(ctx context.Context) (*RewriteSnapshot, error)
}

type rewriteConfigSource struct{}

// NewRewriteConfigSource 构造数据库快照来源。
func NewRewriteConfigSource() RewriteConfigSource {
	return &rewriteConfigSource{}
}

func (s *rewriteConfigSource) Snapshot(ctx context.Context) (*RewriteSnapshot, error) {
	stored, err := upstream_repo.RewriteConfig().Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	out := &RewriteSnapshot{
		SiteBaseURL: setting_svc.SiteBaseURL(stored.SiteDomain),
		Generation:  stored.Generation,
		Upstreams:   make(map[string]RewriteUpstream, len(stored.Upstreams)),
	}
	for _, item := range stored.Upstreams {
		out.Upstreams[item.Host] = RewriteUpstream{
			Profile:    upstream_entity.NormalizePackageProfile(item.PackageProfile),
			Transports: append(upstream_entity.ProtocolSet(nil), item.Protocols...),
		}
	}
	return out, nil
}
