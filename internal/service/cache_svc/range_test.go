package cache_svc

import (
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
)

const rangeLastModified = "Wed, 21 Oct 2015 07:28:00 GMT"

func setupWarmRangeCopy(t *testing.T, body, etag string) (CacheSvc, *originStub, *fakeRepo) {
	t.Helper()
	o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		if etag != "" {
			w.Header().Set("Etag", etag)
		}
		w.Header().Set("Last-Modified", rangeLastModified)
		_, _ = io.WriteString(w, body)
	})
	svc, repo, _ := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})
	pullWith(t, svc, target("deb.debian.org", "/pool/range.deb"))
	return svc, o, repo
}

func TestGet_WarmCopyServesSingleRangesAndIgnoresMalformedRanges(t *testing.T) {
	const full = "0123456789"
	cases := []struct {
		name         string
		rangeHeader  string
		status       int
		body         string
		contentRange string
	}{
		{name: "closed", rangeHeader: "bytes=2-5", status: http.StatusPartialContent, body: "2345", contentRange: "bytes 2-5/10"},
		{name: "open ended", rangeHeader: "bytes=7-", status: http.StatusPartialContent, body: "789", contentRange: "bytes 7-9/10"},
		{name: "suffix", rangeHeader: "bytes=-3", status: http.StatusPartialContent, body: "789", contentRange: "bytes 7-9/10"},
		{name: "suffix longer than representation", rangeHeader: "bytes=-20", status: http.StatusPartialContent, body: full, contentRange: "bytes 0-9/10"},
		{name: "unsatisfiable member is skipped", rangeHeader: "bytes=100-200,2-3", status: http.StatusPartialContent, body: "23", contentRange: "bytes 2-3/10"},
		{name: "unknown unit", rangeHeader: "items=0-2", status: http.StatusOK, body: full},
		{name: "malformed member", rangeHeader: "bytes=0-2,nope", status: http.StatusOK, body: full},
		{name: "reversed past-end range", rangeHeader: "bytes=10-5", status: http.StatusOK, body: full},
		{name: "integer overflow", rangeHeader: "bytes=9223372036854775808-", status: http.StatusOK, body: full},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, origin, _ := setupWarmRangeCopy(t, full, `"v1"`)
			tg := target("deb.debian.org", "/pool/range.deb")
			tg.Header.Set("Range", tc.rangeHeader)
			body, meta := pullWith(t, svc, tg)
			if meta.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", meta.StatusCode, tc.status)
			}
			if body != tc.body {
				t.Fatalf("body = %q, want %q", body, tc.body)
			}
			if got := meta.Header.Get("Content-Range"); got != tc.contentRange {
				t.Fatalf("Content-Range = %q, want %q", got, tc.contentRange)
			}
			if got := meta.Header.Get("Content-Length"); got != strconv.Itoa(len(tc.body)) {
				t.Fatalf("Content-Length = %q, want %d", got, len(tc.body))
			}
			if meta.ContentLength != int64(len(tc.body)) {
				t.Fatalf("meta length = %d, want %d", meta.ContentLength, len(tc.body))
			}
			if meta.Header.Get(cacheStatusHeader) != cacheStatusHit {
				t.Fatalf("cache status = %q", meta.Header.Get(cacheStatusHeader))
			}
			if got := origin.hits.Load(); got != 1 {
				t.Fatalf("origin hits = %d, want 1", got)
			}
		})
	}
}

func TestGet_WarmCopyServesMultipartRangesWithExactFraming(t *testing.T) {
	const full = "0123456789"
	svc, origin, _ := setupWarmRangeCopy(t, full, `"v1"`)
	tg := target("deb.debian.org", "/pool/range.deb")
	tg.Header.Set("Range", "bytes=0-1,7-9")
	body, meta := pullWith(t, svc, tg)

	if meta.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d", meta.StatusCode)
	}
	mediaType, params, err := mime.ParseMediaType(meta.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	if mediaType != "multipart/byteranges" || params["boundary"] == "" {
		t.Fatalf("Content-Type = %q", meta.Header.Get("Content-Type"))
	}
	if strings.ContainsAny(params["boundary"], "\r\n") {
		t.Fatalf("unsafe boundary = %q", params["boundary"])
	}
	mr := multipart.NewReader(strings.NewReader(body), params["boundary"])
	wantRanges := []string{"bytes 0-1/10", "bytes 7-9/10"}
	wantBodies := []string{"01", "789"}
	for i := range wantRanges {
		part, err := mr.NextPart()
		if err != nil {
			t.Fatalf("part %d: %v", i, err)
		}
		got, err := io.ReadAll(part)
		if err != nil {
			t.Fatal(err)
		}
		if part.Header.Get("Content-Range") != wantRanges[i] {
			t.Fatalf("part %d Content-Range = %q", i, part.Header.Get("Content-Range"))
		}
		if part.Header.Get("Content-Type") != "text/plain" {
			t.Fatalf("part %d Content-Type = %q", i, part.Header.Get("Content-Type"))
		}
		if string(got) != wantBodies[i] {
			t.Fatalf("part %d body = %q", i, got)
		}
	}
	if _, err := mr.NextPart(); err != io.EOF {
		t.Fatalf("multipart terminator: %v", err)
	}
	if meta.ContentLength != int64(len(body)) || meta.Header.Get("Content-Length") != strconv.Itoa(len(body)) {
		t.Fatalf("multipart lengths: meta=%d header=%q actual=%d", meta.ContentLength,
			meta.Header.Get("Content-Length"), len(body))
	}
	if meta.Header.Get("Content-Range") != "" {
		t.Fatalf("multipart response-level Content-Range = %q", meta.Header.Get("Content-Range"))
	}
	if meta.Header.Get(cacheStatusHeader) != cacheStatusHit || origin.hits.Load() != 1 {
		t.Fatalf("cache=%q origin hits=%d", meta.Header.Get(cacheStatusHeader), origin.hits.Load())
	}
}

func TestGet_WarmCopyReturns416WhenNoRangeIsSatisfiable(t *testing.T) {
	for _, tc := range []struct {
		name        string
		body        string
		rangeHeader string
	}{
		{name: "past end", body: "0123456789", rangeHeader: "bytes=10-20,30-40"},
		{name: "empty representation", body: "", rangeHeader: "bytes=0-0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, origin, _ := setupWarmRangeCopy(t, tc.body, `"v1"`)
			tg := target("deb.debian.org", "/pool/range.deb")
			tg.Header.Set("Range", tc.rangeHeader)
			body, meta := pullWith(t, svc, tg)
			if meta.StatusCode != http.StatusRequestedRangeNotSatisfiable {
				t.Fatalf("status = %d", meta.StatusCode)
			}
			if body != "" || meta.Header.Get("Content-Length") != "" {
				t.Fatalf("416 body=%q Content-Length=%q", body, meta.Header.Get("Content-Length"))
			}
			want := "bytes */" + strconv.Itoa(len(tc.body))
			if meta.Header.Get("Content-Range") != want {
				t.Fatalf("Content-Range = %q, want %q", meta.Header.Get("Content-Range"), want)
			}
			if meta.Header.Get(cacheStatusHeader) != cacheStatusHit || origin.hits.Load() != 1 {
				t.Fatalf("cache=%q origin hits=%d", meta.Header.Get(cacheStatusHeader), origin.hits.Load())
			}
		})
	}
}

func TestGet_IfRangeRequiresStrongETagOrMatchingSecondPrecisionDate(t *testing.T) {
	const full = "0123456789"
	cases := []struct {
		name       string
		storedETag string
		ifRange    string
		status     int
		body       string
	}{
		{name: "strong ETag match", storedETag: `"v1"`, ifRange: `"v1"`, status: http.StatusPartialContent, body: "012"},
		{name: "strong ETag mismatch", storedETag: `"v1"`, ifRange: `"v2"`, status: http.StatusOK, body: full},
		{name: "weak request ETag", storedETag: `"v1"`, ifRange: `W/"v1"`, status: http.StatusOK, body: full},
		{name: "weak stored ETag", storedETag: `W/"v1"`, ifRange: `"v1"`, status: http.StatusOK, body: full},
		{name: "matching date", storedETag: `"v1"`, ifRange: rangeLastModified, status: http.StatusPartialContent, body: "012"},
		{name: "older date", storedETag: `"v1"`, ifRange: "Tue, 20 Oct 2015 07:28:00 GMT", status: http.StatusOK, body: full},
		{name: "newer nonmatching date", storedETag: `"v1"`, ifRange: "Thu, 22 Oct 2015 07:28:00 GMT", status: http.StatusOK, body: full},
		{name: "invalid date", storedETag: `"v1"`, ifRange: "not a validator", status: http.StatusOK, body: full},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, origin, _ := setupWarmRangeCopy(t, full, tc.storedETag)
			tg := target("deb.debian.org", "/pool/range.deb")
			tg.Header.Set("Range", "bytes=0-2")
			tg.Header.Set("If-Range", tc.ifRange)
			body, meta := pullWith(t, svc, tg)
			if meta.StatusCode != tc.status || body != tc.body {
				t.Fatalf("status/body = %d %q, want %d %q", meta.StatusCode, body, tc.status, tc.body)
			}
			if meta.Header.Get(cacheStatusHeader) != cacheStatusHit || origin.hits.Load() != 1 {
				t.Fatalf("cache=%q origin hits=%d", meta.Header.Get(cacheStatusHeader), origin.hits.Load())
			}
		})
	}
}

func TestGet_HeadIgnoresRangeAndKeepsFullLength(t *testing.T) {
	const full = "0123456789"
	svc, origin, _ := setupWarmRangeCopy(t, full, `"v1"`)
	tg := target("deb.debian.org", "/pool/range.deb")
	tg.Method = http.MethodHead
	tg.Header.Set("Range", "bytes=0-2")
	tg.Header.Set("If-Range", `"v1"`)
	body, meta := pullWith(t, svc, tg)
	if meta.StatusCode != http.StatusOK || body != "" {
		t.Fatalf("status/body = %d %q", meta.StatusCode, body)
	}
	if meta.Header.Get("Content-Length") != strconv.Itoa(len(full)) || meta.ContentLength != int64(len(full)) {
		t.Fatalf("HEAD lengths: header=%q meta=%d", meta.Header.Get("Content-Length"), meta.ContentLength)
	}
	if meta.Header.Get("Content-Range") != "" {
		t.Fatalf("HEAD Content-Range = %q", meta.Header.Get("Content-Range"))
	}
	if meta.Header.Get(cacheStatusHeader) != cacheStatusHit || origin.hits.Load() != 1 {
		t.Fatalf("cache=%q origin hits=%d", meta.Header.Get(cacheStatusHeader), origin.hits.Load())
	}
}

func TestGet_TransformedColdRangeUsesCanonicalFullRepresentation(t *testing.T) {
	var originRange string
	o := newOrigin(t, func(w http.ResponseWriter, r *http.Request) {
		originRange = r.Header.Get("Range")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	})
	profile := testProfile{
		description: packageprofile.Description{Profile: upstream_entity.PackageProfileNPM, Name: "test"},
		representation: packageprofile.Representation{
			Class: packageprofile.ClassMutable, Transform: true, MediaTypes: []string{"application/json"},
		},
		transform: func(context.Context, packageprofile.TransformRequest) (*packageprofile.TransformResult, error) {
			return &packageprofile.TransformResult{Body: []byte("abcdefghij"), ContentType: "application/json"}, nil
		},
	}
	up := staticUpstream("registry.example.com")
	up.PackageProfile = upstream_entity.PackageProfileNPM
	svc, repo, _ := setupSvc(t, o, up, transformingOptions(t, profile, 41))
	tg := target(up.Host, "/metadata")
	tg.Header.Set("Range", "bytes=2-5")
	body, meta := pullWith(t, svc, tg)
	if meta.StatusCode != http.StatusPartialContent || body != "cdef" {
		t.Fatalf("status/body = %d %q", meta.StatusCode, body)
	}
	if meta.Header.Get("Content-Range") != "bytes 2-5/10" || meta.Header.Get(cacheStatusHeader) != cacheStatusMiss {
		t.Fatalf("headers = %#v", meta.Header)
	}
	if originRange != "" {
		t.Fatalf("canonical origin Range = %q", originRange)
	}
	if len(repo.all()) != 1 {
		t.Fatalf("cache rows = %d", len(repo.all()))
	}
	full, hit := pullWith(t, svc, target(up.Host, "/metadata"))
	if full != "abcdefghij" || hit.Header.Get(cacheStatusHeader) != cacheStatusHit || o.hits.Load() != 1 {
		t.Fatalf("warm full body=%q cache=%q origin hits=%d", full,
			hit.Header.Get(cacheStatusHeader), o.hits.Load())
	}
}

func TestGet_ColdRangePassesThroughAndNeverCachesPartialOriginResponse(t *testing.T) {
	var gotRange, gotIfRange string
	o := newOrigin(t, func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.Header.Get("Range")
		gotIfRange = r.Header.Get("If-Range")
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Range", "bytes 2-4/10")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "234")
	})
	svc, repo, _ := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})
	for i := 1; i <= 2; i++ {
		tg := target("deb.debian.org", "/pool/cold-range.deb")
		tg.Header.Set("Range", "bytes=2-4")
		tg.Header.Set("If-Range", `"v1"`)
		body, meta := pullWith(t, svc, tg)
		if meta.StatusCode != http.StatusPartialContent || body != "234" {
			t.Fatalf("request %d status/body = %d %q", i, meta.StatusCode, body)
		}
		if meta.Header.Get(cacheStatusHeader) != cacheStatusMiss {
			t.Fatalf("request %d cache = %q", i, meta.Header.Get(cacheStatusHeader))
		}
		if gotRange != "bytes=2-4" || gotIfRange != `"v1"` {
			t.Fatalf("origin conditions = Range %q If-Range %q", gotRange, gotIfRange)
		}
		if len(repo.all()) != 0 {
			t.Fatalf("partial response cached after request %d", i)
		}
	}
	if got := o.hits.Load(); got != 2 {
		t.Fatalf("origin hits = %d, want 2", got)
	}
}
