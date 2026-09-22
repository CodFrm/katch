package proxy_svc

import (
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/dispatch"
)

const sumDBHost = dispatch.SumDBHost

// SumDBOptions supplies the coherent configuration and normal proxy fallback used by the checksum bridge.
type SumDBOptions struct {
	RewriteConfig RewriteConfigSource
	Fallback      ProxySvc
}

type sumDBSvc struct {
	source   RewriteConfigSource
	fallback ProxySvc
}

// NewSumDB constructs the fixed sum.golang.org checksum bridge.
func NewSumDB(opt SumDBOptions) ProxySvc {
	if opt.RewriteConfig == nil {
		opt.RewriteConfig = NewRewriteConfigSource()
	}
	return &sumDBSvc{
		source:   opt.RewriteConfig,
		fallback: opt.Fallback,
	}
}

func (s *sumDBSvc) Fetch(ctx context.Context, target *Target) (io.ReadCloser, *Meta, error) {
	if s.fallback != nil && !canonicalSumDBTarget(target) {
		return s.fallback.Fetch(ctx, target)
	}
	upstreamPath, supported, ok := sumDBRoute(target)
	if !ok {
		return sumDBResponse(http.StatusNotFound)
	}
	if target.Method != http.MethodGet && target.Method != http.MethodHead {
		return sumDBResponse(http.StatusMethodNotAllowed)
	}
	if !s.configured(ctx) {
		return sumDBResponse(http.StatusServiceUnavailable)
	}
	if supported {
		return sumDBResponse(http.StatusOK)
	}
	if s.fallback == nil {
		return nil, nil, ErrUpstreamNotAllowed
	}
	canonical := *target
	canonical.Kind = dispatch.KindStatic
	canonical.Host = sumDBHost
	canonical.Path = upstreamPath
	return s.fallback.Fetch(ctx, &canonical)
}

func (s *sumDBSvc) configured(ctx context.Context) bool {
	snapshot, err := s.source.Snapshot(ctx)
	if err != nil || snapshot == nil {
		return false
	}
	for host, upstream := range snapshot.Upstreams {
		if !strings.EqualFold(strings.TrimSuffix(host, "."), sumDBHost) {
			continue
		}
		return upstream_entity.NormalizePackageProfile(upstream.Profile) == upstream_entity.PackageProfileGoProxy &&
			upstream.Transports.Has(upstream_entity.ProtocolStatic)
	}
	return false
}

func canonicalSumDBTarget(target *Target) bool {
	return target != nil && target.Kind == dispatch.KindSumDB &&
		strings.EqualFold(strings.TrimSuffix(target.Host, "."), sumDBHost)
}

func sumDBRoute(target *Target) (upstreamPath string, supported, ok bool) {
	if target == nil || !canonicalSumDBTarget(target) {
		return "", false, false
	}
	path := target.Path
	if path == "/supported" {
		// 唯一一条合成应答：能力问答由本地配置回答，不打上游。
		return "", true, true
	}
	if dispatch.ValidSumDBRoute(path) {
		return path, false, true
	}
	return "", false, false
}

func sumDBResponse(status int) (io.ReadCloser, *Meta, error) {
	return io.NopCloser(strings.NewReader("")), &Meta{
		StatusCode:    status,
		Header:        make(http.Header),
		ContentLength: 0,
	}, nil
}
