package cache_svc

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/CodFrm/katch/internal/metrics"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
)

func TestGet_ByteTransparentIdentityRejectionIsStampedAsMiss(t *testing.T) {
	o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(cacheStatusHeader, cacheStatusHit)
		w.Header().Set(metrics.MissHeader, string(metrics.MissChanged))
		_, _ = io.WriteString(w, "compressed upstream bytes")
	})
	svc, repo, _ := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})
	tg := target("deb.debian.org", "/pool/main/p/pkg.deb")
	tg.Header.Set("Accept-Encoding", "gzip, identity;q=0")

	body, meta, err := svc.Get(context.Background(), tg)
	if err != nil {
		t.Fatal(err)
	}
	payload, readErr := io.ReadAll(body)
	closeErr := body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read/close = %v/%v", readErr, closeErr)
	}
	if string(payload) != "compressed upstream bytes" || meta.StatusCode != http.StatusOK {
		t.Fatalf("response = %d %q", meta.StatusCode, payload)
	}
	if got := meta.Header.Get(cacheStatusHeader); got != cacheStatusMiss {
		t.Fatalf("%s = %q, want %q", cacheStatusHeader, got, cacheStatusMiss)
	}
	if got := meta.Header.Get(metrics.MissHeader); got != string(metrics.MissFirst) {
		t.Fatalf("%s = %q, want %q", metrics.MissHeader, got, metrics.MissFirst)
	}
	if o.hits.Load() != 1 || len(repo.all()) != 0 {
		t.Fatalf("origin hits/cache rows = %d/%d", o.hits.Load(), len(repo.all()))
	}
}

func TestGet_TransformableIdentityRejectionRemainsNotAcceptable(t *testing.T) {
	o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("transformable identity rejection must not reach origin")
	})
	profile := testProfile{
		description: packageprofile.Description{Profile: upstream_entity.PackageProfileNPM, Name: "test"},
		representation: packageprofile.Representation{
			Class: packageprofile.ClassMutable, Transform: true, MediaTypes: []string{"application/json"},
		},
		transform: func(context.Context, packageprofile.TransformRequest) (*packageprofile.TransformResult, error) {
			t.Fatal("transformable identity rejection must not transform")
			return nil, nil
		},
	}
	up := staticUpstream("metadata.example.com")
	up.PackageProfile = upstream_entity.PackageProfileNPM
	svc, _, _ := setupSvc(t, o, up, transformingOptions(t, profile, 1))
	tg := target(up.Host, "/pkg")
	tg.Header.Set("Accept-Encoding", "identity;q=0, gzip")

	body, meta, err := svc.Get(context.Background(), tg)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if meta.StatusCode != http.StatusNotAcceptable || o.hits.Load() != 0 {
		t.Fatalf("status/origin hits = %d/%d", meta.StatusCode, o.hits.Load())
	}
}
