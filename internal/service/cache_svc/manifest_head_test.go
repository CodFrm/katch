package cache_svc

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"testing"
)

type observedManifestRequest struct {
	method         string
	accept         string
	acceptEncoding string
}

func TestGet_ColdRegistryManifestHeadWarmsCanonicalGET(t *testing.T) {
	const (
		manifest = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`
		digest   = "sha256:0123456789abcdef"
	)
	for _, path := range []string{
		"/stefanprodan/podinfo/manifests/6.9.2",
		"/stefanprodan/podinfo/manifests/sha256:0123456789abcdef",
	} {
		t.Run(path, func(t *testing.T) {
			observed := make(chan observedManifestRequest, 1)
			o := newOrigin(t, func(w http.ResponseWriter, r *http.Request) {
				observed <- observedManifestRequest{
					method: r.Method, accept: r.Header.Get("Accept"),
					acceptEncoding: r.Header.Get("Accept-Encoding"),
				}
				w.Header().Set("Content-Type", ociManifestType)
				w.Header().Set("Docker-Content-Digest", digest)
				_, _ = io.WriteString(w, manifest)
			})
			svc, repo, _ := setupSvc(t, o, registryUpstream("registry.test"), Options{})

			head := registryTarget("registry.test", path, ociManifestType, ociIndexType)
			head.Method = http.MethodHead
			head.Header.Set("Accept-Encoding", "gzip")
			body, meta, err := svc.Get(context.Background(), head)
			if err != nil {
				t.Fatalf("cold HEAD: %v", err)
			}
			payload, readErr := io.ReadAll(body)
			if readErr != nil || body.Close() != nil || len(payload) != 0 {
				t.Fatalf("cold HEAD body = %q, read error = %v", payload, readErr)
			}
			request := <-observed
			if request.method != http.MethodGet || request.accept != head.Header.Get("Accept") ||
				request.acceptEncoding != "identity" {
				t.Fatalf("promoted request = %+v, want GET with original Accept and identity encoding", request)
			}
			if meta.StatusCode != http.StatusOK || meta.Header.Get(cacheStatusHeader) != cacheStatusMiss ||
				meta.Header.Get("Docker-Content-Digest") != digest ||
				meta.Header.Get("Content-Length") != strconv.Itoa(len(manifest)) ||
				meta.ContentLength != int64(len(manifest)) {
				t.Fatalf("cold HEAD meta = %+v", meta)
			}
			if repo.byKey(cacheKey(head)) == nil {
				t.Fatalf("canonical GET representation was not persisted under %q", cacheKey(head))
			}

			warmBody, warmMeta, err := svc.Get(context.Background(), head)
			if err != nil {
				t.Fatalf("warm HEAD: %v", err)
			}
			warmPayload, readErr := io.ReadAll(warmBody)
			if readErr != nil || warmBody.Close() != nil || len(warmPayload) != 0 {
				t.Fatalf("warm HEAD body = %q, read error = %v", warmPayload, readErr)
			}
			if warmMeta.Header.Get(cacheStatusHeader) != cacheStatusHit ||
				warmMeta.Header.Get("Docker-Content-Digest") != digest || o.hits.Load() != 1 {
				t.Fatalf("warm HEAD meta/origin hits = %+v/%d", warmMeta, o.hits.Load())
			}

			get := registryTarget("registry.test", path, ociManifestType, ociIndexType)
			get.Header.Set("Accept-Encoding", "gzip")
			got, getMeta := pullWith(t, svc, get)
			if got != manifest || getMeta.Header.Get(cacheStatusHeader) != cacheStatusHit || o.hits.Load() != 1 {
				t.Fatalf("warm GET body/meta/origin hits = %q/%+v/%d", got, getMeta, o.hits.Load())
			}
		})
	}
}

func TestGet_RegistryManifestHeadPromotionIsStrictlyScoped(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		header http.Header
	}{
		{name: "blob", path: "/stefanprodan/podinfo/blobs/sha256:0123456789abcdef"},
		{name: "range manifest", path: "/stefanprodan/podinfo/manifests/6.9.2",
			header: http.Header{"Range": {"bytes=0-3"}}},
		{name: "conditional manifest", path: "/stefanprodan/podinfo/manifests/6.9.2",
			header: http.Header{"If-None-Match": {`"v1"`}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			methods := make(chan string, 1)
			o := newOrigin(t, func(w http.ResponseWriter, r *http.Request) {
				methods <- r.Method
				w.Header().Set("Content-Length", "1048576")
			})
			svc, repo, _ := setupSvc(t, o, registryUpstream("registry.test"), Options{})
			head := registryTarget("registry.test", tc.path, ociManifestType)
			head.Method = http.MethodHead
			for name, values := range tc.header {
				head.Header[name] = values
			}

			body, meta, err := svc.Get(context.Background(), head)
			if err != nil {
				t.Fatalf("HEAD passthrough: %v", err)
			}
			if closeErr := body.Close(); closeErr != nil {
				t.Fatalf("close HEAD body: %v", closeErr)
			}
			if method := <-methods; method != http.MethodHead {
				t.Fatalf("origin method = %s, want HEAD", method)
			}
			if meta.Header.Get(cacheStatusHeader) != cacheStatusMiss || len(repo.all()) != 0 {
				t.Fatalf("passthrough meta/records = %+v/%+v", meta, repo.all())
			}
		})
	}
}

func TestGet_UncacheableRegistryManifestHeadReturnsGETMetadataWithoutRecord(t *testing.T) {
	const manifest = `{"schemaVersion":2,"uncacheable":true}`
	methods := make(chan string, 2)
	o := newOrigin(t, func(w http.ResponseWriter, r *http.Request) {
		methods <- r.Method
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", ociManifestType)
		w.Header().Set("Docker-Content-Digest", "sha256:uncacheable")
		_, _ = io.WriteString(w, manifest)
	})
	svc, repo, _ := setupSvc(t, o, registryUpstream("registry.test"), Options{})
	head := registryTarget("registry.test", "/o/r/manifests/latest", ociManifestType)
	head.Method = http.MethodHead

	for attempt := 1; attempt <= 2; attempt++ {
		body, meta, err := svc.Get(context.Background(), head)
		if err != nil {
			t.Fatalf("HEAD attempt %d: %v", attempt, err)
		}
		payload, readErr := io.ReadAll(body)
		if readErr != nil || body.Close() != nil || len(payload) != 0 {
			t.Fatalf("HEAD attempt %d body = %q, read error = %v", attempt, payload, readErr)
		}
		if method := <-methods; method != http.MethodGet {
			t.Fatalf("origin method = %s, want GET", method)
		}
		if meta.Header.Get(cacheStatusHeader) != cacheStatusMiss ||
			meta.Header.Get("Content-Length") != strconv.Itoa(len(manifest)) ||
			meta.ContentLength != int64(len(manifest)) {
			t.Fatalf("HEAD attempt %d meta = %+v", attempt, meta)
		}
	}
	if len(repo.all()) != 0 || o.hits.Load() != 2 {
		t.Fatalf("uncacheable records/origin hits = %+v/%d", repo.all(), o.hits.Load())
	}
}

func TestGet_RegistryManifestHeadPropagatesPromotedBodyReadFailure(t *testing.T) {
	o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", ociManifestType)
		w.Header().Set("Content-Length", "64")
		_, _ = io.WriteString(w, "short")
	})
	svc, repo, _ := setupSvc(t, o, registryUpstream("registry.test"), Options{})
	head := registryTarget("registry.test", "/o/r/manifests/broken", ociManifestType)
	head.Method = http.MethodHead

	body, meta, err := svc.Get(context.Background(), head)
	if err == nil || body != nil || meta != nil {
		t.Fatalf("body/meta/error = %#v/%#v/%v, want propagated read failure", body, meta, err)
	}
	if len(repo.all()) != 0 {
		t.Fatalf("interrupted representation created records: %+v", repo.all())
	}
}
