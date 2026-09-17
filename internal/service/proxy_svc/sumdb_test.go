package proxy_svc

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/destination"
	"github.com/CodFrm/katch/internal/proxy/dispatch"
)

type sumDBRewriteSource struct {
	snapshot *RewriteSnapshot
	err      error
}

func (s sumDBRewriteSource) Snapshot(context.Context) (*RewriteSnapshot, error) {
	return s.snapshot, s.err
}

type resolverFunc func(context.Context, *url.URL, destination.DestinationRequirement) (*destination.ResolvedTarget, error)

func (f resolverFunc) Resolve(ctx context.Context, target *url.URL, requirement destination.DestinationRequirement) (*destination.ResolvedTarget, error) {
	return f(ctx, target, requirement)
}

func TestNewComposesSumDBBeforeStaticFallback(t *testing.T) {
	repo := setupRepo(t)
	repo.EXPECT().List(gomock.Any()).Return([]*upstream_entity.Upstream{}, nil).AnyTimes()
	resolverCalls := 0
	svc := New(Options{
		RewriteConfig: configuredSumDBSource(upstream_entity.PackageProfileGoProxy, upstream_entity.ProtocolStatic),
		DestinationResolver: resolverFunc(func(context.Context, *url.URL, destination.DestinationRequirement) (*destination.ResolvedTarget, error) {
			resolverCalls++
			return nil, errors.New("supported must not resolve")
		}),
	})
	body, meta, err := svc.Fetch(context.Background(), &Target{
		Kind: dispatch.KindStatic, Host: "sum.golang.org", Path: "/supported", Method: http.MethodGet,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if meta.StatusCode != http.StatusOK || resolverCalls != 0 {
		t.Fatalf("status = %d, resolver calls = %d; want 200, 0", meta.StatusCode, resolverCalls)
	}
}

func TestSumDBSupportedRequiresConfiguredUpstream(t *testing.T) {
	valid := &RewriteSnapshot{Upstreams: map[string]RewriteUpstream{
		"sum.golang.org": {
			Profile:    upstream_entity.PackageProfileGoProxy,
			Transports: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
		},
	}}

	tests := []struct {
		name   string
		path   string
		source RewriteConfigSource
		want   int
	}{
		{name: "configured", path: "/sumdb/sum.golang.org/supported", source: sumDBRewriteSource{snapshot: valid}, want: http.StatusOK},
		{name: "snapshot error", path: "/sumdb/sum.golang.org/supported", source: sumDBRewriteSource{err: errors.New("database unavailable")}, want: http.StatusServiceUnavailable},
		{name: "nil snapshot", path: "/sumdb/sum.golang.org/supported", source: sumDBRewriteSource{}, want: http.StatusServiceUnavailable},
		{name: "missing upstream", path: "/sumdb/sum.golang.org/supported", source: sumDBRewriteSource{snapshot: &RewriteSnapshot{Upstreams: map[string]RewriteUpstream{}}}, want: http.StatusServiceUnavailable},
		{name: "missing upstream lookup", path: "/sumdb/sum.golang.org/lookup/example.com/mod@v1.0.0", source: sumDBRewriteSource{snapshot: &RewriteSnapshot{Upstreams: map[string]RewriteUpstream{}}}, want: http.StatusServiceUnavailable},
		{name: "wrong profile", path: "/sumdb/sum.golang.org/supported", source: configuredSumDBSource(upstream_entity.PackageProfileNPM, upstream_entity.ProtocolStatic), want: http.StatusServiceUnavailable},
		{name: "wrong transport", path: "/sumdb/sum.golang.org/supported", source: configuredSumDBSource(upstream_entity.PackageProfileGoProxy, upstream_entity.ProtocolRegistry), want: http.StatusServiceUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resolverCalls := 0
			svc := NewSumDB(SumDBOptions{
				RewriteConfig: tc.source,
				DestinationResolver: resolverFunc(func(context.Context, *url.URL, destination.DestinationRequirement) (*destination.ResolvedTarget, error) {
					resolverCalls++
					return nil, errors.New("must not resolve supported")
				}),
			})
			body, meta, err := svc.Fetch(context.Background(), &Target{Method: http.MethodGet, Path: tc.path})
			if err != nil {
				t.Fatal(err)
			}
			defer body.Close()
			got, err := io.ReadAll(body)
			if err != nil {
				t.Fatal(err)
			}
			if meta.StatusCode != tc.want || len(got) != 0 || resolverCalls != 0 {
				t.Fatalf("status = %d, body = %q, resolver calls = %d; want %d, empty, 0", meta.StatusCode, got, resolverCalls, tc.want)
			}
		})
	}
}

func TestSumDBRoutesApprovedPathsAndPassesResponseThrough(t *testing.T) {
	var mu sync.Mutex
	var requests []string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.URL.RequestURI())
		mu.Unlock()
		w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, "sumdb-bytes\x00\xff")
	}))
	defer origin.Close()

	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(originURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	resolver := resolverFunc(func(_ context.Context, target *url.URL, requirement destination.DestinationRequirement) (*destination.ResolvedTarget, error) {
		if target.Host != "sum.golang.org" || target.Scheme != "https" {
			t.Fatalf("resolver target = %s", target)
		}
		if !requirement.RequireRegistered || requirement.Transport != upstream_entity.ProtocolStatic ||
			requirement.Profile != upstream_entity.PackageProfileGoProxy || requirement.AddressPolicy != destination.PublicAddressesOnly {
			t.Fatalf("resolver requirement = %+v", requirement)
		}
		mapped := *target
		mapped.Scheme = "http"
		return &destination.ResolvedTarget{
			URL: &mapped, Authority: "sum.golang.org", Host: "sum.golang.org",
			ServerName: "sum.golang.org", DialAddress: net.JoinHostPort("127.0.0.1", port),
		}, nil
	})
	svc := NewSumDB(SumDBOptions{
		RewriteConfig:       configuredSumDBSource(upstream_entity.PackageProfileGoProxy, upstream_entity.ProtocolStatic),
		DestinationResolver: resolver,
	})

	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "lookup", path: "/sumdb/sum.golang.org/lookup/github.com/!burnt!sushi/toml@v1.4.0?x=1", want: "/lookup/github.com/!burnt!sushi/toml@v1.4.0?x=1"},
		{name: "complete tile", path: "/sumdb/sum.golang.org/tile/8/1/000", want: "/tile/8/1/000"},
		{name: "partial tile", path: "/sumdb/sum.golang.org/tile/8/1/000.p/16", want: "/tile/8/1/000.p/16"},
		{name: "latest", path: "/sumdb/sum.golang.org/latest", want: "/latest"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := url.Parse(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			body, meta, err := svc.Fetch(context.Background(), &Target{
				Method: http.MethodGet, Path: parsed.EscapedPath(), RawQuery: parsed.RawQuery,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer body.Close()
			got, err := io.ReadAll(body)
			if err != nil {
				t.Fatal(err)
			}
			if meta.StatusCode != http.StatusTeapot || string(got) != "sumdb-bytes\x00\xff" || meta.Header.Get("Content-Type") != "text/plain; charset=UTF-8" {
				t.Fatalf("status = %d, headers = %v, body = %q", meta.StatusCode, meta.Header, got)
			}
			mu.Lock()
			last := requests[len(requests)-1]
			mu.Unlock()
			if last != tc.want {
				t.Fatalf("origin URI = %q, want %q", last, tc.want)
			}
		})
	}
}

func TestSumDBRejectsUnknownPathsWithoutOriginAccess(t *testing.T) {
	resolverCalls := 0
	svc := NewSumDB(SumDBOptions{
		RewriteConfig: configuredSumDBSource(upstream_entity.PackageProfileGoProxy, upstream_entity.ProtocolStatic),
		DestinationResolver: resolverFunc(func(context.Context, *url.URL, destination.DestinationRequirement) (*destination.ResolvedTarget, error) {
			resolverCalls++
			return nil, errors.New("unexpected origin access")
		}),
	})

	paths := []string{
		"/sumdb/sum.golang.org",
		"/sumdb/sum.golang.org/lookup",
		"/sumdb/sum.golang.org/lookups/example.com/mod@v1.0.0",
		"/sumdb/sum.golang.org/tile",
		"/sumdb/sum.golang.org/latest/extra",
		"/sumdb/other.example/lookup/example.com/mod@v1.0.0",
	}
	for _, path := range paths {
		t.Run(strings.ReplaceAll(path, "/", "_"), func(t *testing.T) {
			body, meta, err := svc.Fetch(context.Background(), &Target{Method: http.MethodGet, Path: path})
			if err != nil {
				t.Fatal(err)
			}
			defer body.Close()
			if meta.StatusCode != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", meta.StatusCode)
			}
		})
	}
	if resolverCalls != 0 {
		t.Fatalf("resolver calls = %d, want 0", resolverCalls)
	}
}

func configuredSumDBSource(profile upstream_entity.PackageProfile, transport string) RewriteConfigSource {
	return sumDBRewriteSource{snapshot: &RewriteSnapshot{Upstreams: map[string]RewriteUpstream{
		"sum.golang.org": {Profile: profile, Transports: upstream_entity.ProtocolSet{transport}},
	}}}
}
