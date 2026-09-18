package proxy_svc

import (
	"context"
	"strings"

	"github.com/cago-frame/cago/pkg/logger"
	"go.uber.org/zap"

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
	base := setting_svc.SiteBaseURL(stored.SiteDomain)
	if base == "" && strings.TrimSpace(stored.SiteDomain) != "" {
		// 拼不出可用地址的值一路失败下去都是「还没配站点地址」，那条路上谁也
		// 不会再提这个值一次；不在这里把它念出来，运维就只能看着一个说没配、
		// 库里却写着东西的设置项。
		logger.Ctx(ctx).Warn("站点地址不是一个可用的地址，本次按未配置处理",
			zap.String("site_domain", stored.SiteDomain))
	}
	out := &RewriteSnapshot{
		SiteBaseURL: base,
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
