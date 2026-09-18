package proxy_svc

import (
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/dispatch"
)

const sumDBHost = "sum.golang.org"

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
	if supported {
		if !s.configured(ctx) {
			return sumDBResponse(http.StatusServiceUnavailable)
		}
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
	return target != nil && strings.EqualFold(strings.TrimSuffix(target.Host, "."), sumDBHost)
}

func sumDBRoute(target *Target) (upstreamPath string, supported, ok bool) {
	if target == nil || !canonicalSumDBTarget(target) {
		return "", false, false
	}
	path := target.Path
	if path == "/supported" {
		return "", true, true
	}
	if path == "/latest" || strings.HasPrefix(path, "/lookup/") && len(path) > len("/lookup/") ||
		strings.HasPrefix(path, "/tile/") && len(path) > len("/tile/") {
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
