package cache_svc

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"testing"

	"github.com/cago-frame/cago/pkg/logger"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
	"github.com/CodFrm/katch/internal/service/proxy_svc"
)

func TestGet_UnavailableCompanionLogsSanitizedAdministrativeReason(t *testing.T) {
	cases := []struct {
		name       string
		configured *proxy_svc.RewriteUpstream
		wantReason string
	}{
		{name: "missing or disabled", wantReason: "not_configured_or_disabled"},
		{name: "transport incompatible", configured: &proxy_svc.RewriteUpstream{
			Profile:    upstream_entity.PackageProfileNPM,
			Transports: upstream_entity.ProtocolSet{upstream_entity.ProtocolRegistry},
		}, wantReason: "transport_incompatible"},
		{name: "profile incompatible", configured: &proxy_svc.RewriteUpstream{
			Profile:    upstream_entity.PackageProfileComposer,
			Transports: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
		}, wantReason: "profile_incompatible"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.WarnLevel)
			logger.SetLogger(zap.New(core))
			t.Cleanup(func() { logger.SetLogger(zap.NewNop()) })

			sensitive, err := url.Parse("https://operator:password@CDN.EXAMPLE.COM./pkg/-/pkg.tgz?token=secret#fragment")
			if err != nil {
				t.Fatal(err)
			}
			profile := testProfile{
				description: packageprofile.Description{Profile: upstream_entity.PackageProfileNPM, Name: "test"},
				representation: packageprofile.Representation{
					Class: packageprofile.ClassMutable, Transform: true, MediaTypes: []string{"application/json"},
				},
				transform: func(ctx context.Context, in packageprofile.TransformRequest) (*packageprofile.TransformResult, error) {
					_, rewriteErr := in.RewriteURL(ctx, sensitive, packageprofile.Companion{
						Host: "cdn.example.com", Profile: upstream_entity.PackageProfileNPM,
						Transport: upstream_entity.ProtocolStatic,
					})
					return nil, rewriteErr
				},
			}
			up := staticUpstream("metadata.example.com")
			up.PackageProfile = upstream_entity.PackageProfileNPM
			options := transformingOptions(t, profile, 1)
			snapshot := options.RewriteConfig.(fixedRewriteSource).snapshot
			if tc.configured != nil {
				snapshot.Upstreams["cdn.example.com"] = *tc.configured
			}
			o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{}`)
			})
			svc, _, _ := setupSvc(t, o, up, options)

			body, meta, getErr := svc.Get(context.Background(), target(up.Host, "/metadata"))
			if getErr != nil {
				t.Fatal(getErr)
			}
			payload, readErr := io.ReadAll(body)
			closeErr := body.Close()
			if readErr != nil || closeErr != nil {
				t.Fatalf("read/close = %v/%v", readErr, closeErr)
			}
			if meta.StatusCode != http.StatusServiceUnavailable || len(payload) != 0 || len(meta.Header) != 0 {
				t.Fatalf("anonymous response = status %d headers %v body %q", meta.StatusCode, meta.Header, payload)
			}

			entries := logs.FilterLevelExact(zapcore.WarnLevel).All()
			if len(entries) != 1 {
				t.Fatalf("warning entries = %d, want 1", len(entries))
			}
			fields := entries[0].ContextMap()
			if fields["host"] != "cdn.example.com" || fields["reason"] != tc.wantReason {
				t.Fatalf("warning fields = %#v", fields)
			}
			if len(fields) != 2 {
				t.Fatalf("warning contains unexpected fields: %#v", fields)
			}
		})
	}
}
