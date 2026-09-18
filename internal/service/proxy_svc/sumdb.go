package proxy_svc

import (
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/destination"
	"github.com/CodFrm/katch/internal/proxy/origin"
)

const (
	sumDBHost   = "sum.golang.org"
	sumDBPrefix = "/sumdb/" + sumDBHost
)

// SumDBOptions supplies the coherent configuration and pinned destination used by the checksum bridge.
type SumDBOptions struct {
	RewriteConfig       RewriteConfigSource
	DestinationResolver destination.DestinationResolver
	Fallback            ProxySvc
}

type sumDBSvc struct {
	source   RewriteConfigSource
	origin   *origin.Client
	fallback ProxySvc
}

// NewSumDB constructs the fixed sum.golang.org checksum bridge.
func NewSumDB(opt SumDBOptions) ProxySvc {
	if opt.RewriteConfig == nil {
		opt.RewriteConfig = NewRewriteConfigSource()
	}
	if opt.DestinationResolver == nil {
		opt.DestinationResolver = destination.New(destination.Options{
			Source: destinationConfigSource{source: opt.RewriteConfig},
		})
	}
	return &sumDBSvc{
		source:   opt.RewriteConfig,
		origin:   origin.New(origin.Options{Resolver: opt.DestinationResolver}),
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

	resp, err := s.origin.Do(ctx, &origin.Request{
		Method:   target.Method,
		Origin:   "https://" + sumDBHost,
		Path:     upstreamPath,
		RawQuery: target.RawQuery,
		Header:   target.Header,
		Requirement: destination.DestinationRequirement{
			RequireRegistered: true,
			Transport:         upstream_entity.ProtocolStatic,
			Profile:           upstream_entity.PackageProfileGoProxy,
			AddressPolicy:     destination.PublicAddressesOnly,
		},
	})
	if err != nil {
		return nil, nil, err
	}
	return resp.Body, &Meta{
		StatusCode:    resp.StatusCode,
		Header:        resp.Header,
		ContentLength: resp.ContentLength,
		SourceURL:     cloneURL(resp.SourceURL),
	}, nil
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
	if target == nil {
		return "", false, false
	}
	path := target.Path
	if !canonicalSumDBTarget(target) {
		var found bool
		path, found = strings.CutPrefix(path, sumDBPrefix)
		if !found {
			return "", false, false
		}
	}
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
