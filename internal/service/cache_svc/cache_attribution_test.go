package cache_svc

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/CodFrm/katch/internal/cache"
	"github.com/CodFrm/katch/internal/metrics"
	"github.com/CodFrm/katch/internal/service/proxy_svc"
)

func TestGet_CacheUnavailableOrdinaryFetchOverridesOriginAttribution(t *testing.T) {
	o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(cacheStatusHeader, cacheStatusHit)
		w.Header().Set(metrics.MissHeader, string(metrics.MissChanged))
		w.Header().Set("X-Protocol-Revision", "7")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "origin body")
	})
	upstream := staticUpstream("files.example.com")
	_, repo, _ := setupSvc(t, o, upstream, Options{})
	svc := New(nil, Options{})

	body, meta, err := svc.Get(context.Background(), target(upstream.Host, "/ordinary"))
	if err != nil {
		t.Fatal(err)
	}
	payload, readErr := io.ReadAll(body)
	closeErr := body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read/close = %v/%v", readErr, closeErr)
	}
	if meta.StatusCode != http.StatusCreated || string(payload) != "origin body" {
		t.Fatalf("response = %d %q, want 201 origin body", meta.StatusCode, payload)
	}
	if got := meta.Header.Get(cacheStatusHeader); got != cacheStatusMiss {
		t.Fatalf("%s = %q, want %q", cacheStatusHeader, got, cacheStatusMiss)
	}
	if got := meta.Header.Get(metrics.MissHeader); got != string(metrics.MissFirst) {
		t.Fatalf("%s = %q, want %q", metrics.MissHeader, got, metrics.MissFirst)
	}
	if got := meta.Header.Get("X-Protocol-Revision"); got != "7" {
		t.Fatalf("X-Protocol-Revision = %q, want 7", got)
	}
	if o.hits.Load() != 1 || len(repo.all()) != 0 {
		t.Fatalf("origin hits/cache rows = %d/%d, want 1/0", o.hits.Load(), len(repo.all()))
	}
}

func TestGet_UncacheableOriginResponsesOverrideSpoofedAttribution(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		configure  func(http.Header)
		wantHeader string
	}{
		{name: "no-store", status: http.StatusOK, configure: func(h http.Header) {
			h.Set("Cache-Control", "no-store")
		}, wantHeader: "no-store"},
		{name: "private", status: http.StatusOK, configure: func(h http.Header) {
			h.Set("Cache-Control", "private")
		}, wantHeader: "private"},
		{name: "content-encoded", status: http.StatusOK, configure: func(h http.Header) {
			h.Set("Content-Encoding", "br")
		}, wantHeader: "br"},
		{name: "non-200", status: http.StatusNotFound, configure: func(http.Header) {}, wantHeader: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set(cacheStatusHeader, cacheStatusHit)
				w.Header().Set(metrics.MissHeader, string(metrics.MissChanged))
				w.Header().Set("X-Protocol-Revision", "7")
				tc.configure(w.Header())
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, "origin body")
			})
			svc, repo, _ := setupSvc(t, o, staticUpstream("files.example.com"), Options{})

			payload, meta := pullWith(t, svc, target("files.example.com", "/uncacheable"))
			if meta.StatusCode != tc.status || payload != "origin body" {
				t.Fatalf("response = %d %q, want %d origin body", meta.StatusCode, payload, tc.status)
			}
			if got := meta.Header.Get(cacheStatusHeader); got != cacheStatusMiss {
				t.Fatalf("%s = %q, want %q", cacheStatusHeader, got, cacheStatusMiss)
			}
			if got := meta.Header.Get(metrics.MissHeader); got != string(metrics.MissFirst) {
				t.Fatalf("%s = %q, want %q", metrics.MissHeader, got, metrics.MissFirst)
			}
			if got := meta.Header.Get("X-Protocol-Revision"); got != "7" {
				t.Fatalf("X-Protocol-Revision = %q, want 7", got)
			}
			if tc.wantHeader != "" {
				name := "Cache-Control"
				if tc.name == "content-encoded" {
					name = "Content-Encoding"
				}
				if got := meta.Header.Get(name); got != tc.wantHeader {
					t.Fatalf("%s = %q, want %q", name, got, tc.wantHeader)
				}
			}
			if o.hits.Load() != 1 || len(repo.all()) != 0 {
				t.Fatalf("origin hits/cache rows = %d/%d, want 1/0", o.hits.Load(), len(repo.all()))
			}
		})
	}
}

func TestAttachOrFetch_UncacheableWaiterFallbackOverridesOriginAttribution(t *testing.T) {
	o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(cacheStatusHeader, cacheStatusHit)
		w.Header().Set(metrics.MissHeader, string(metrics.MissChanged))
		w.Header().Set("X-Protocol-Revision", "7")
		_, _ = io.WriteString(w, "fallback body")
	})
	up := staticUpstream("files.example.com")
	svc, _, store := setupSvc(t, o, up, Options{})
	current := newFlight(store)
	current.startUncacheable()

	body, meta, err := svc.(*cacheSvc).attachOrFetch(
		context.Background(), current, target("files.example.com", "/fallback"), up, "/fallback",
	)
	if err != nil {
		t.Fatal(err)
	}
	payload, readErr := io.ReadAll(body)
	closeErr := body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read/close = %v/%v", readErr, closeErr)
	}
	if meta.StatusCode != http.StatusOK || string(payload) != "fallback body" {
		t.Fatalf("response = %d %q, want 200 fallback body", meta.StatusCode, payload)
	}
	if got := meta.Header.Get(cacheStatusHeader); got != cacheStatusMiss {
		t.Fatalf("%s = %q, want %q", cacheStatusHeader, got, cacheStatusMiss)
	}
	if got := meta.Header.Get(metrics.MissHeader); got != string(metrics.MissFirst) {
		t.Fatalf("%s = %q, want %q", metrics.MissHeader, got, metrics.MissFirst)
	}
	if got := meta.Header.Get("X-Protocol-Revision"); got != "7" {
		t.Fatalf("X-Protocol-Revision = %q, want 7", got)
	}
	if o.hits.Load() != 1 {
		t.Fatalf("origin hits = %d, want 1", o.hits.Load())
	}
}

func TestGet_StoreCreateFailureOverridesOriginAttribution(t *testing.T) {
	o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(cacheStatusHeader, cacheStatusHit)
		w.Header().Set(metrics.MissHeader, string(metrics.MissChanged))
		w.Header().Set("X-Protocol-Revision", "7")
		_, _ = io.WriteString(w, "degraded body")
	})
	svc, repo, store := setupSvc(t, o, staticUpstream("files.example.com"), Options{})
	probe, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	tmpDir := filepath.Dir(probe.Name())
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(tmpDir); err != nil {
		t.Fatal(err)
	}

	payload, meta := pullWith(t, svc, target("files.example.com", "/degraded"))
	if meta.StatusCode != http.StatusOK || payload != "degraded body" {
		t.Fatalf("response = %d %q, want 200 degraded body", meta.StatusCode, payload)
	}
	if got := meta.Header.Get(cacheStatusHeader); got != cacheStatusMiss {
		t.Fatalf("%s = %q, want %q", cacheStatusHeader, got, cacheStatusMiss)
	}
	if got := meta.Header.Get(metrics.MissHeader); got != string(metrics.MissFirst) {
		t.Fatalf("%s = %q, want %q", metrics.MissHeader, got, metrics.MissFirst)
	}
	if got := meta.Header.Get("X-Protocol-Revision"); got != "7" {
		t.Fatalf("X-Protocol-Revision = %q, want 7", got)
	}
	if o.hits.Load() != 1 || len(repo.all()) != 0 {
		t.Fatalf("origin hits/cache rows = %d/%d, want 1/0", o.hits.Load(), len(repo.all()))
	}
}

func TestFlight_ActiveReaderAndCompletedAttachmentAttribution(t *testing.T) {
	store, err := cache.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	current := newFlight(store)
	writer, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	current.start(&proxy_svc.Meta{
		StatusCode: http.StatusOK,
		Header: http.Header{
			cacheStatusHeader:  []string{cacheStatusMiss},
			metrics.MissHeader: []string{string(metrics.MissFirst)},
		},
		ContentLength: int64(len("cached body")),
	}, writer.Name())

	activeBody, activeMeta, err := current.attach(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := activeMeta.Header.Get(cacheStatusHeader); got != cacheStatusMiss {
		t.Fatalf("active %s = %q, want %q", cacheStatusHeader, got, cacheStatusMiss)
	}
	if got := activeMeta.Header.Get(metrics.MissHeader); got != string(metrics.MissFirst) {
		t.Fatalf("active %s = %q, want %q", metrics.MissHeader, got, metrics.MissFirst)
	}
	if err := activeBody.Close(); err != nil {
		t.Fatal(err)
	}

	n, err := writer.Write([]byte("cached body"))
	if err != nil {
		t.Fatal(err)
	}
	current.publish(int64(n))
	digest, _, err := writer.Commit()
	if err != nil {
		t.Fatal(err)
	}
	current.finish(digest, nil)

	completedBody, completedMeta, err := current.attach(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	payload, readErr := io.ReadAll(completedBody)
	closeErr := completedBody.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read/close = %v/%v", readErr, closeErr)
	}
	if string(payload) != "cached body" {
		t.Fatalf("completed body = %q, want cached body", payload)
	}
	if got := completedMeta.Header.Get(cacheStatusHeader); got != cacheStatusHit {
		t.Fatalf("completed %s = %q, want %q", cacheStatusHeader, got, cacheStatusHit)
	}
	if got := completedMeta.Header.Get(metrics.MissHeader); got != "" {
		t.Fatalf("completed %s = %q, want empty", metrics.MissHeader, got)
	}
}
