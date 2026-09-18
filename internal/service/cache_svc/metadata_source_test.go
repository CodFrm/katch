package cache_svc

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/destination"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
	mock_upstream_repo "github.com/CodFrm/katch/internal/repository/upstream_repo/mock"
	"github.com/CodFrm/katch/internal/service/proxy_svc"
)

type metadataSourceResolver struct {
	addresses  map[string]string
	registered map[string]destination.RewriteUpstream
}

func (r metadataSourceResolver) Resolve(
	_ context.Context, target *url.URL, requirement destination.DestinationRequirement,
) (*destination.ResolvedTarget, error) {
	host := strings.ToLower(strings.TrimSuffix(target.Hostname(), "."))
	if requirement.RequireRegistered {
		configured, ok := r.registered[host]
		if !ok || requirement.Transport != "" && !configured.Transports.Has(requirement.Transport) ||
			requirement.Profile != "" && upstream_entity.NormalizePackageProfile(configured.Profile) != requirement.Profile {
			return nil, destination.ErrDestinationNotAllowed
		}
	}
	dial, ok := r.addresses[target.Host]
	if !ok {
		return nil, destination.ErrDestinationNotAllowed
	}
	cloned := *target
	return &destination.ResolvedTarget{
		URL: &cloned, Authority: target.Host, Host: target.Host,
		ServerName: target.Hostname(), DialAddress: dial,
	}, nil
}

func TestGet_RelativeMetadataUsesFinalRedirectURL(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RequestURI() != "/repository/npm/pkg?metadata=full" {
			t.Errorf("final request URI = %q", r.URL.RequestURI())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"versions":{"1.0.0":{"dist":{"tarball":"./-/pkg-1.0.0.tgz?download=1"}}}}`)
	}))
	defer final.Close()
	finalURL := mappedMetadataURL(t, final.URL, "registry.npmjs.org")

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/configured/base/pkg" {
			t.Errorf("initial request path = %q", r.URL.Path)
		}
		http.Redirect(w, r, finalURL+"/repository/npm/pkg?metadata=full", http.StatusFound)
	}))
	defer redirect.Close()
	redirectURL := mappedMetadataURL(t, redirect.URL, "origin.example.test")

	snapshot := &proxy_svc.RewriteSnapshot{
		SiteBaseURL: "https://katch.example.com",
		Generation:  41,
		Upstreams: map[string]proxy_svc.RewriteUpstream{
			"registry.npmjs.org": {
				Profile:    upstream_entity.PackageProfileNPM,
				Transports: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
			},
		},
	}
	resolver := metadataSourceResolver{
		addresses: map[string]string{
			mustMetadataURL(t, redirectURL).Host: mustMetadataURL(t, redirect.URL).Host,
			mustMetadataURL(t, finalURL).Host:    mustMetadataURL(t, final.URL).Host,
			"registry.npmjs.org":                 mustMetadataURL(t, final.URL).Host,
		},
		registered: map[string]destination.RewriteUpstream{
			"registry.npmjs.org": {
				Profile:    upstream_entity.PackageProfileNPM,
				Transports: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
			},
		},
	}

	upstream := &upstream_entity.Upstream{
		ID: 9, Host: "registry.npmjs.org", Origin: redirectURL + "/configured/base", Enabled: true,
		Protocols:      upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
		PackageProfile: upstream_entity.PackageProfileNPM, MutableTTLSeconds: 60,
	}
	repo := mock_upstream_repo.NewMockUpstreamRepo(gomock.NewController(t))
	repo.EXPECT().List(gomock.Any()).Return([]*upstream_entity.Upstream{upstream}, nil).AnyTimes()
	previousUpstream := upstream_repo.Upstream()
	previousRewrite := upstream_repo.RewriteConfig()
	upstream_repo.RegisterUpstream(proxy_svc.NewCachedUpstreamRepo(repo))
	t.Cleanup(func() {
		upstream_repo.RegisterUpstream(previousUpstream)
		upstream_repo.RegisterRewriteConfig(previousRewrite)
	})

	previousProxy := proxy_svc.Proxy()
	proxy_svc.Register(proxy_svc.New(proxy_svc.Options{
		RewriteConfig: fixedRewriteSource{snapshot: snapshot}, DestinationResolver: resolver,
	}))
	t.Cleanup(func() { proxy_svc.Register(previousProxy) })

	npm, ok := packageprofile.Lookup(upstream_entity.PackageProfileNPM)
	if !ok {
		t.Fatal("npm profile is not registered")
	}
	profiles := packageprofile.NewRegistry()
	if err := profiles.Register(npm); err != nil {
		t.Fatal(err)
	}
	svc := New(nil, Options{
		Profiles: profiles, RewriteConfig: fixedRewriteSource{snapshot: snapshot}, DestinationResolver: resolver,
	})
	body, meta, err := svc.Get(context.Background(), target(upstream.Host, "/pkg"))
	if err != nil {
		t.Fatal(err)
	}
	payload, readErr := io.ReadAll(body)
	closeErr := body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read/close = %v/%v", readErr, closeErr)
	}
	if meta.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %q", meta.StatusCode, payload)
	}
	var document struct {
		Versions map[string]struct {
			Dist struct {
				Tarball string `json:"tarball"`
			} `json:"dist"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatal(err)
	}
	want := "https://katch.example.com/registry.npmjs.org/repository/npm/-/pkg-1.0.0.tgz?download=1"
	if got := document.Versions["1.0.0"].Dist.Tarball; got != want {
		t.Fatalf("rewritten tarball = %q, want %q", got, want)
	}
	if strings.Contains(string(payload), "origin.example.test") ||
		strings.Contains(string(payload), mustMetadataURL(t, finalURL).Host) || meta.Header.Get("Location") != "" {
		t.Fatalf("anonymous response exposed origin URL: headers=%v body=%s", meta.Header, payload)
	}
}

func mappedMetadataURL(t *testing.T, serverURL, hostname string) string {
	t.Helper()
	parsed := mustMetadataURL(t, serverURL)
	parsed.Host = hostname + ":" + parsed.Port()
	return parsed.String()
}

func mustMetadataURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
