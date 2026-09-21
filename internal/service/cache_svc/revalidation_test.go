package cache_svc

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/CodFrm/katch/internal/metrics"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
	"github.com/CodFrm/katch/internal/service/proxy_svc"
)

// 过期或 no-cache 的可变对象回源时带上保存的上游 validator：上游说没变（304），就续期、
// 复用盘上的字节，不再整份重下。spec「Cache and download semantics」修订段与决策 9/10。

const (
	revalidationETag         = `"origin-v1"`
	revalidationLastModified = "Wed, 21 Oct 2015 07:28:00 GMT"
)

// revalidationOrigin 一个会做条件求值的假源站：记下每次请求带来的条件头，按 respond
// 给出的决定应答。respond 返回 0 表示照常给出 200 与 body，负数表示它已自己写完应答。
type revalidationOrigin struct {
	*originStub
	mu       sync.Mutex
	requests []http.Header
	methods  []string
}

func newRevalidationOrigin(t *testing.T, body string, headers map[string]string,
	respond func(n int, r *http.Request, w http.ResponseWriter) int,
) *revalidationOrigin {
	t.Helper()
	ro := &revalidationOrigin{}
	ro.originStub = newOrigin(t, func(w http.ResponseWriter, r *http.Request) {
		ro.mu.Lock()
		ro.requests = append(ro.requests, r.Header.Clone())
		ro.methods = append(ro.methods, r.Method)
		n := len(ro.requests)
		ro.mu.Unlock()
		if respond != nil {
			status := respond(n, r, w)
			if status < 0 {
				// respond 自己写完了整个应答。
				return
			}
			if status != 0 {
				w.WriteHeader(status)
				return
			}
		}
		for name, value := range headers {
			w.Header().Set(name, value)
		}
		_, _ = io.WriteString(w, body)
	})
	return ro
}

func (ro *revalidationOrigin) method(n int) string {
	ro.mu.Lock()
	defer ro.mu.Unlock()
	if n < 1 || n > len(ro.methods) {
		return ""
	}
	return ro.methods[n-1]
}

func (ro *revalidationOrigin) request(n int) http.Header {
	ro.mu.Lock()
	defer ro.mu.Unlock()
	if n < 1 || n > len(ro.requests) {
		return nil
	}
	return ro.requests[n-1]
}

// notModifiedAfterFirst 第一次给 200，之后只要带着条件就答 304，304 上附带 extra 头。
func notModifiedAfterFirst(extra map[string]string) func(int, *http.Request, http.ResponseWriter) int {
	return func(n int, r *http.Request, w http.ResponseWriter) int {
		if n == 1 || (r.Header.Get("If-None-Match") == "" && r.Header.Get("If-Modified-Since") == "") {
			return 0
		}
		for name, value := range extra {
			w.Header().Set(name, value)
		}
		return http.StatusNotModified
	}
}

func staticValidatorHeaders() map[string]string {
	return map[string]string{
		"Content-Type":  "text/plain",
		"Etag":          revalidationETag,
		"Last-Modified": revalidationLastModified,
		"Cache-Control": "public, max-age=60",
	}
}

func TestGet_ExpiredObjectRevalidatesWithOriginValidators(t *testing.T) {
	ro := newRevalidationOrigin(t, "index-v1", staticValidatorHeaders(),
		notModifiedAfterFirst(map[string]string{"Cache-Control": "public, max-age=120"}))
	up := staticUpstream("deb.example.com")
	svc, repo, _ := setupSvc(t, ro.originStub, up, Options{})
	const path = "/dists/stable/InRelease"

	pullWith(t, svc, target(up.Host, path))
	stored := repo.byKey(path)
	if stored == nil {
		t.Fatal("首次拉取没有落库")
	}
	repo.expire(path)

	body, renewed := pullWith(t, svc, target(up.Host, path))
	sent := ro.request(2)
	if sent == nil {
		t.Fatalf("过期对象没有回源，origin hits = %d", ro.hits.Load())
	}
	if got := sent.Get("If-None-Match"); got != revalidationETag {
		t.Fatalf("回源 If-None-Match = %q，要的是保存的上游 ETag %q", got, revalidationETag)
	}
	if got := sent.Get("If-Modified-Since"); got != revalidationLastModified {
		t.Fatalf("回源 If-Modified-Since = %q，要的是保存的 Last-Modified %q", got, revalidationLastModified)
	}
	if got := sent.Get("Accept-Encoding"); got != "identity" {
		t.Fatalf("回源 Accept-Encoding = %q，续期请求仍是 canonical identity GET", got)
	}
	if body != "index-v1" {
		t.Fatalf("304 续期后的正文 = %q，要的是盘上那份", body)
	}
	if got := renewed.StatusCode; got != http.StatusOK {
		t.Fatalf("续期应答状态 = %d", got)
	}
	if got := renewed.Header.Get(cacheStatusHeader); got != cacheStatusRevalidated {
		t.Fatalf("X-Katch-Cache = %q，要的是 %q", got, cacheStatusRevalidated)
	}
	if got := missReasonOf(renewed); got != string(metrics.MissTTL) {
		t.Fatalf("续期的归因 = %q，要的是 %q", got, metrics.MissTTL)
	}
	if got := renewed.Header.Get("Etag"); got != revalidationETag {
		t.Fatalf("续期应答 Etag = %q", got)
	}
	// 304 带来的新鲜度信息要落进记录：新的 Cache-Control 和重算过的过期时刻。
	row := repo.byKey(path)
	if row.Digest != stored.Digest {
		t.Fatalf("304 续期不该换内容：digest %q -> %q", stored.Digest, row.Digest)
	}
	if row.CacheControl != "public, max-age=120" {
		t.Fatalf("续期后 Cache-Control = %q，要按 304 更新", row.CacheControl)
	}
	if row.Expired(time.Now().Unix()) {
		t.Fatalf("续期后记录仍是过期的：expires_at=%d", row.ExpiresAt)
	}

	_, hit := pullWith(t, svc, target(up.Host, path))
	if got := hit.Header.Get(cacheStatusHeader); got != cacheStatusHit {
		t.Fatalf("续期之后的下一次 = %q，要的是 HIT", got)
	}
	if got := hit.Header.Get("Cache-Control"); got != "public, max-age=120" {
		t.Fatalf("命中回放的 Cache-Control = %q，要的是 304 带来的那份", got)
	}
	if got := ro.hits.Load(); got != 2 {
		t.Fatalf("origin hits = %d，要的是 2（首次 + 一次条件回源）", got)
	}
}

func TestGet_RevalidationStrongETagMismatchFallsBackToUnconditionalGet(t *testing.T) {
	// 304 带着一个和保存的不一样的强 ETag：证明不了手上这份就是上游那份，只能整份再取。
	ro := newRevalidationOrigin(t, "index", staticValidatorHeaders(),
		func(n int, r *http.Request, w http.ResponseWriter) int {
			if n == 2 {
				w.Header().Set("Etag", `"origin-v2"`)
				return http.StatusNotModified
			}
			return 0
		})
	up := staticUpstream("deb.example.com")
	svc, repo, _ := setupSvc(t, ro.originStub, up, Options{})
	const path = "/dists/stable/Release"

	pullWith(t, svc, target(up.Host, path))
	repo.expire(path)
	body, meta := pullWith(t, svc, target(up.Host, path))

	if got := ro.hits.Load(); got != 3 {
		t.Fatalf("origin hits = %d，要的是 3（首次、条件回源、退回的无条件 GET）", got)
	}
	third := ro.request(3)
	if third.Get("If-None-Match") != "" || third.Get("If-Modified-Since") != "" {
		t.Fatalf("退回的 GET 仍带着条件：%v", third)
	}
	if body != "index" {
		t.Fatalf("正文 = %q", body)
	}
	if got := meta.Header.Get(cacheStatusHeader); got != cacheStatusMiss {
		t.Fatalf("整份重取的 X-Katch-Cache = %q，要的是 MISS", got)
	}
}

func TestGet_RevalidationWithoutValidatorsStaysUnconditional(t *testing.T) {
	ro := newRevalidationOrigin(t, "plain", map[string]string{
		"Content-Type": "text/plain", "Cache-Control": "public, max-age=60",
	}, nil)
	up := staticUpstream("deb.example.com")
	svc, repo, _ := setupSvc(t, ro.originStub, up, Options{})
	const path = "/dists/stable/plain"

	pullWith(t, svc, target(up.Host, path))
	repo.expire(path)
	_, meta := pullWith(t, svc, target(up.Host, path))

	second := ro.request(2)
	if second == nil {
		t.Fatal("过期对象没有回源")
	}
	if second.Get("If-None-Match") != "" || second.Get("If-Modified-Since") != "" {
		t.Fatalf("没有 validator 的记录发出了条件请求：%v", second)
	}
	if got := meta.Header.Get(cacheStatusHeader); got != cacheStatusMiss {
		t.Fatalf("X-Katch-Cache = %q，要的是 MISS", got)
	}
}

func TestGet_NoCacheObjectRevalidatesOnEveryReuse(t *testing.T) {
	headers := staticValidatorHeaders()
	headers["Cache-Control"] = "public, no-cache"
	ro := newRevalidationOrigin(t, "no-cache-body", headers,
		notModifiedAfterFirst(map[string]string{"Cache-Control": "public, no-cache"}))
	up := staticUpstream("deb.example.com")
	svc, _, _ := setupSvc(t, ro.originStub, up, Options{})
	const path = "/service-index.json"

	pullWith(t, svc, target(up.Host, path))
	for i := 0; i < 2; i++ {
		body, meta := pullWith(t, svc, target(up.Host, path))
		if body != "no-cache-body" {
			t.Fatalf("第 %d 次复用的正文 = %q", i+1, body)
		}
		if got := meta.Header.Get(cacheStatusHeader); got != cacheStatusRevalidated {
			t.Fatalf("第 %d 次复用 X-Katch-Cache = %q，no-cache 的每次复用都要先校验", i+1, got)
		}
	}
	if got := ro.hits.Load(); got != 3 {
		t.Fatalf("origin hits = %d，要的是 3", got)
	}
}

func TestGet_RevalidationToUnstorableServesValidatedBytesAndDropsRecord(t *testing.T) {
	ro := newRevalidationOrigin(t, "secret-index", staticValidatorHeaders(),
		notModifiedAfterFirst(map[string]string{"Cache-Control": "no-store"}))
	up := staticUpstream("deb.example.com")
	svc, repo, store := setupSvc(t, ro.originStub, up, Options{})
	const path = "/dists/stable/InRelease"

	pullWith(t, svc, target(up.Host, path))
	digest := repo.byKey(path).Digest
	repo.expire(path)

	body, meta := pullWith(t, svc, target(up.Host, path))
	if body != "secret-index" {
		t.Fatalf("本次仍应由验证过的字节应答，得到 %q", body)
	}
	if got := meta.Header.Get(cacheStatusHeader); got != cacheStatusRevalidated {
		t.Fatalf("X-Katch-Cache = %q", got)
	}
	if row := repo.byKey(path); row != nil {
		t.Fatalf("304 变成 no-store 后记录仍在：%+v", row)
	}
	if file, _, err := store.Open(digest); err == nil {
		_ = file.Close()
		t.Fatal("无人引用的字节没有删掉")
	}
}

func TestGet_RevalidationOriginFailureNeverPromotesStaleCopy(t *testing.T) {
	ro := newRevalidationOrigin(t, "index", staticValidatorHeaders(),
		func(n int, _ *http.Request, _ http.ResponseWriter) int {
			if n > 1 {
				return http.StatusServiceUnavailable
			}
			return 0
		})
	up := staticUpstream("deb.example.com")
	svc, repo, _ := setupSvc(t, ro.originStub, up, Options{})
	const path = "/dists/stable/InRelease"

	pullWith(t, svc, target(up.Host, path))
	repo.expire(path)

	_, meta := pullWith(t, svc, target(up.Host, path))
	if meta.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("上游 503 时应答 = %d，过期副本不能冒充新鲜", meta.StatusCode)
	}
	if row := repo.byKey(path); row == nil || !row.Expired(time.Now().Unix()) {
		t.Fatalf("上游失败后记录被续期了：%+v", row)
	}
	pullWith(t, svc, target(up.Host, path))
	if got := ro.request(3).Get("If-None-Match"); got != revalidationETag {
		t.Fatalf("下一次仍应带着 validator 再试，If-None-Match = %q", got)
	}
}

func TestGet_TransformedMetadataRevalidatesWithOriginValidatorNotKatchETag(t *testing.T) {
	ro := newRevalidationOrigin(t, `{"url":"origin"}`, map[string]string{
		"Content-Type":  "application/json",
		"Etag":          revalidationETag,
		"Last-Modified": revalidationLastModified,
	}, notModifiedAfterFirst(nil))
	var calls int
	var mu sync.Mutex
	profile := testProfile{
		description: packageprofile.Description{Profile: upstream_entity.PackageProfileNPM, Name: "test"},
		representation: packageprofile.Representation{Class: packageprofile.ClassMutable, Transform: true,
			MediaTypes: []string{"application/json"}},
		transform: func(_ context.Context, in packageprofile.TransformRequest) (*packageprofile.TransformResult, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			return &packageprofile.TransformResult{Body: bytes.ToUpper(in.Body), ContentType: "application/json"}, nil
		},
	}
	up := staticUpstream("registry.example.com")
	up.PackageProfile = upstream_entity.PackageProfileNPM
	svc, repo, _ := setupSvc(t, ro.originStub, up, transformingOptions(t, profile, 3))

	_, first := pullWith(t, svc, target(up.Host, "/left-pad"))
	katchETag := first.Header.Get("Etag")
	rows := repo.all()
	if len(rows) != 1 {
		t.Fatalf("cache rows = %d", len(rows))
	}
	if rows[0].ETag != katchETag || rows[0].OriginETag != revalidationETag ||
		rows[0].OriginLastModified != revalidationLastModified {
		t.Fatalf("转换后元数据的 validator 落库不对：etag=%q origin_etag=%q origin_last_modified=%q",
			rows[0].ETag, rows[0].OriginETag, rows[0].OriginLastModified)
	}
	repo.expire(rows[0].Key)

	tg := target(up.Host, "/left-pad")
	tg.Header.Set("If-None-Match", katchETag)
	got, meta, err := svc.Get(context.Background(), tg)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := io.ReadAll(got)
	_ = got.Close()

	sent := ro.request(2)
	if sent == nil {
		t.Fatal("过期的转换元数据没有回源")
	}
	if got := sent.Get("If-None-Match"); got != revalidationETag {
		t.Fatalf("回源 If-None-Match = %q，只能发上游自己的 ETag，不能发 katch 的 %q", got, katchETag)
	}
	if got := sent.Get("If-Modified-Since"); got != revalidationLastModified {
		t.Fatalf("回源 If-Modified-Since = %q", got)
	}
	// 客户端自己的条件在续期之后按本地表示求值：它拿着 katch ETag，于是本地 304。
	if meta.StatusCode != http.StatusNotModified || len(payload) != 0 {
		t.Fatalf("客户端条件的本地求值 = %d / %q", meta.StatusCode, payload)
	}
	if got := meta.Header.Get(cacheStatusHeader); got != cacheStatusRevalidated {
		t.Fatalf("X-Katch-Cache = %q", got)
	}
	if got := meta.Header.Get("Etag"); got != katchETag {
		t.Fatalf("续期后的 Etag = %q，要的仍是 katch 的表示 ETag", got)
	}
	if got := meta.Header.Get("Last-Modified"); got != "" {
		t.Fatalf("转换元数据不回放上游 Last-Modified，得到 %q", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("304 续期不该重新转换，transform 调用 %d 次", calls)
	}
}

func TestAttachOrFetch_WaiterOnRenewedFlightReadsTheRenewedCopy(t *testing.T) {
	// 等在一趟 304 续期上的请求没有下载可以共读：它们也从盘上读那份刚被确认过的副本，
	// 而不是各自再去打一次上游。
	ro := newRevalidationOrigin(t, "shared-index", staticValidatorHeaders(), nil)
	up := staticUpstream("deb.example.com")
	svc, repo, store := setupSvc(t, ro.originStub, up, Options{})
	const path = "/dists/stable/InRelease"
	pullWith(t, svc, target(up.Host, path))

	current := newFlight(store)
	current.startRenewed(repo.byKey(path))
	body, meta, err := svc.(*cacheSvc).attachOrFetch(context.Background(), current,
		target(up.Host, path), up, path)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := io.ReadAll(body)
	_ = body.Close()
	if string(payload) != "shared-index" {
		t.Fatalf("等待者读到 %q", payload)
	}
	if got := meta.Header.Get(cacheStatusHeader); got != cacheStatusRevalidated {
		t.Fatalf("等待者 X-Katch-Cache = %q", got)
	}
	if got := ro.hits.Load(); got != 1 {
		t.Fatalf("等待者又回源了：origin hits = %d", got)
	}
}

// 过期对象碰上 HEAD、Range 或客户端自己的条件请求，同样走 canonical identity GET 刷新：
// 带的是保存的上游 validator，不转发客户端的方法、范围与条件；刷新之后按本地表示求值，
// 与命中时一样。只有冷请求（手上没有副本）才原样透传。spec「An expired mutable object,
// or one stored with no-cache…」一段。
func TestGet_ExpiredObjectRefreshesCanonicallyForHeadRangeAndConditional(t *testing.T) {
	const path = "/dists/stable/InRelease"
	type expect struct {
		status int
		body   string
		cache  string
	}
	cases := []struct {
		name    string
		mutate  func(tg *proxy_svc.Target)
		respond func(int, *http.Request, http.ResponseWriter) int
		body2   string
		want    expect
	}{
		{
			name:    "HEAD 经 304 续期",
			mutate:  func(tg *proxy_svc.Target) { tg.Method = http.MethodHead },
			respond: notModifiedAfterFirst(nil),
			want:    expect{status: http.StatusOK, body: "", cache: cacheStatusRevalidated},
		},
		{
			name:    "客户端条件不转发，续期后本地求值为 304",
			mutate:  func(tg *proxy_svc.Target) { tg.Header.Set("If-None-Match", revalidationETag) },
			respond: notModifiedAfterFirst(nil),
			want:    expect{status: http.StatusNotModified, body: "", cache: cacheStatusRevalidated},
		},
		{
			name:    "客户端条件对不上，续期后本地给整份",
			mutate:  func(tg *proxy_svc.Target) { tg.Header.Set("If-None-Match", `"client-held"`) },
			respond: notModifiedAfterFirst(nil),
			want:    expect{status: http.StatusOK, body: "index-v1", cache: cacheStatusRevalidated},
		},
		{
			name:    "Range 经 304 续期后本地切片",
			mutate:  func(tg *proxy_svc.Target) { tg.Header.Set("Range", "bytes=0-4") },
			respond: notModifiedAfterFirst(nil),
			want:    expect{status: http.StatusPartialContent, body: "index", cache: cacheStatusRevalidated},
		},
		{
			name:   "Range 碰上上游 200：整份替换后本地切片",
			mutate: func(tg *proxy_svc.Target) { tg.Header.Set("Range", "bytes=6-7") },
			respond: func(n int, _ *http.Request, w http.ResponseWriter) int {
				if n > 1 {
					w.Header().Set("Content-Type", "text/plain")
					w.Header().Set("Etag", `"origin-v2"`)
					_, _ = io.WriteString(w, "index-v2")
					return -1
				}
				return 0
			},
			body2: "index-v2",
			want:  expect{status: http.StatusPartialContent, body: "v2", cache: cacheStatusMiss},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ro := newRevalidationOrigin(t, "index-v1", staticValidatorHeaders(), tc.respond)
			up := staticUpstream("deb.example.com")
			svc, repo, _ := setupSvc(t, ro.originStub, up, Options{})
			pullWith(t, svc, target(up.Host, path))
			repo.expire(path)

			tg := target(up.Host, path)
			tg.Header.Set("Accept-Encoding", "gzip")
			tc.mutate(tg)
			body, meta := pullWith(t, svc, tg)

			if got := ro.method(2); got != http.MethodGet {
				t.Fatalf("刷新回源的方法 = %q，要的是 canonical GET", got)
			}
			sent := ro.request(2)
			if got := sent.Get("If-None-Match"); got != revalidationETag {
				t.Fatalf("刷新回源 If-None-Match = %q，要的是保存的上游 ETag", got)
			}
			if got := sent.Get("If-Modified-Since"); got != revalidationLastModified {
				t.Fatalf("刷新回源 If-Modified-Since = %q", got)
			}
			if sent.Get("Range") != "" || sent.Get("If-Range") != "" {
				t.Fatalf("客户端的范围被转发给了上游：%v", sent)
			}
			if got := sent.Get("Accept-Encoding"); got != "identity" {
				t.Fatalf("刷新回源 Accept-Encoding = %q", got)
			}
			if meta.StatusCode != tc.want.status || body != tc.want.body {
				t.Fatalf("本地求值 = %d / %q，要的是 %d / %q", meta.StatusCode, body, tc.want.status, tc.want.body)
			}
			if got := meta.Header.Get(cacheStatusHeader); got != tc.want.cache {
				t.Fatalf("X-Katch-Cache = %q，要的是 %q", got, tc.want.cache)
			}
			if got := missReasonOf(meta); got != string(metrics.MissTTL) {
				t.Fatalf("归因 = %q，要的是 %q", got, metrics.MissTTL)
			}
			row := repo.byKey(path)
			if row == nil || row.Expired(time.Now().Unix()) {
				t.Fatalf("刷新之后记录仍过期或不见了：%+v", row)
			}
			if tc.body2 != "" && row.Digest != digestOfString(tc.body2) {
				t.Fatalf("上游 200 之后记录没换成新内容：digest=%q", row.Digest)
			}
			if got := ro.hits.Load(); got != 2 {
				t.Fatalf("origin hits = %d，要的是 2（首次 + 一次刷新）", got)
			}
		})
	}
}

func TestGet_ExpiredManifestHeadRevalidatesWithOriginValidators(t *testing.T) {
	// docker pull 先 HEAD tag manifest：过期之后这一步同样要带上游 validator 去问。
	const manifest = `{"schemaVersion":2}`
	ro := newRevalidationOrigin(t, manifest, map[string]string{
		"Content-Type": ociManifestType, "Etag": revalidationETag,
		"Docker-Content-Digest": digestOfString(manifest),
	}, notModifiedAfterFirst(nil))
	up := registryUpstream("registry.test")
	svc, repo, _ := setupSvc(t, ro.originStub, up, Options{})
	const path = "/library/redis/manifests/7"

	head := registryTarget(up.Host, path, ociManifestType)
	head.Method = http.MethodHead
	pullWith(t, svc, head)
	key := cacheKey(head)
	if repo.byKey(key) == nil {
		t.Fatal("冷 HEAD 没有填充缓存")
	}
	repo.expire(key)

	body, meta := pullWith(t, svc, head)
	if got := ro.request(2).Get("If-None-Match"); got != revalidationETag {
		t.Fatalf("过期 manifest 的 HEAD 回源 If-None-Match = %q", got)
	}
	if got := ro.method(2); got != http.MethodGet {
		t.Fatalf("回源方法 = %q", got)
	}
	if body != "" || meta.StatusCode != http.StatusOK {
		t.Fatalf("HEAD 应答 = %d / %q", meta.StatusCode, body)
	}
	if got := meta.Header.Get(cacheStatusHeader); got != cacheStatusRevalidated {
		t.Fatalf("X-Katch-Cache = %q", got)
	}
	if got := meta.Header.Get("Docker-Content-Digest"); got != digestOfString(manifest) {
		t.Fatalf("Docker-Content-Digest = %q", got)
	}
}
