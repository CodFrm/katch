package web

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"

	"github.com/CodFrm/katch/internal/cache"
	"github.com/CodFrm/katch/internal/model/entity/cache_entity"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/destination"
	"github.com/CodFrm/katch/internal/repository/cache_repo"
	mock_cache_repo "github.com/CodFrm/katch/internal/repository/cache_repo/mock"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
	mock_upstream_repo "github.com/CodFrm/katch/internal/repository/upstream_repo/mock"
	"github.com/CodFrm/katch/internal/service/cache_svc"
	"github.com/CodFrm/katch/internal/service/proxy_svc"
)

// countingOrigin 假源站：记下被打了几次，并且可以当场关掉。
//
// 关掉之后还能拉到的字节只可能来自磁盘——这比只数回源次数更硬，它排除了
// 「第二次其实又走了一趟网络，只是恰好也拿到同样的字节」这种解释。
func countingOrigin(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	hits := &atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

// memoryCacheObjects 把缓存记录表换成一张内存表。
//
// 逐次 EXPECT 在这里写不出来：这个用例问的是「第二次请求能不能看见第一次写下的
// 记录」，要的正是记录之间的先后，而不是某个方法被调了几次。
func memoryCacheObjects(t *testing.T) {
	t.Helper()
	var (
		mu     sync.Mutex
		rows   = map[int64]*cache_entity.CacheObject{}
		nextID = int64(1)
	)
	m := mock_cache_repo.NewMockCacheObjectRepo(gomock.NewController(t))
	m.EXPECT().FindByKey(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(_ any, upstreamID int64, key string) (*cache_entity.CacheObject, error) {
			mu.Lock()
			defer mu.Unlock()
			for _, row := range rows {
				if row.UpstreamID == upstreamID && row.Key == key {
					dst := *row
					return &dst, nil
				}
			}
			return nil, nil
		})
	m.EXPECT().Save(gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(_ any, object *cache_entity.CacheObject) error {
			mu.Lock()
			defer mu.Unlock()
			if object.ID == 0 {
				object.ID = nextID
				nextID++
			}
			dst := *object
			rows[object.ID] = &dst
			return nil
		})
	m.EXPECT().Touch(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(_ any, id int64, at int64) error {
			mu.Lock()
			defer mu.Unlock()
			if row, ok := rows[id]; ok {
				row.HitCount++
				row.LastAccessAt = at
			}
			return nil
		})
	m.EXPECT().TotalSize(gomock.Any()).AnyTimes().DoAndReturn(func(_ any) (int64, error) {
		mu.Lock()
		defer mu.Unlock()
		total := int64(0)
		for _, row := range rows {
			total += row.Size
		}
		return total, nil
	})
	cache_repo.RegisterCacheObject(m)
	t.Cleanup(func() { cache_repo.RegisterCacheObject(nil) })
}

// withDiskCache 把拉取路径上的缓存层换成「真磁盘目录 + 内存记录表」，装配形态与
// main 一致。用完恢复成出厂的纯透传，免得它漏给同包里别的用例。
func withDiskCache(t *testing.T) {
	t.Helper()
	withDiskCacheOptions(t, cache_svc.Options{})
}

func withDiskCacheOptions(t *testing.T, opt cache_svc.Options) cache_svc.CacheSvc {
	t.Helper()
	memoryCacheObjects(t)
	store, err := cache.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("建缓存目录失败：%v", err)
	}
	svc := cache_svc.New(store, opt)
	previous := cache_svc.Cache()
	cache_svc.Register(svc)
	t.Cleanup(func() { cache_svc.Register(previous) })
	return svc
}

// cacheRequest 经**真实的** NoRoute 处理器发一次带方法/请求头的拉取。
//
// 不复用 request：它写死了 GET，而 HEAD 与条件请求正是这条用例要看的。真实
// NoRoute 而非替身，是因为这里的每一条结论都是客户端能观察到的 HTTP 行为，替身
// 漂了守卫就守了个空。
func cacheRequest(t *testing.T, method, path string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.NoRoute(newNoRouteHandlerFS(testDist()))
	req := httptest.NewRequest(method, path, nil)
	if header != nil {
		req.Header = header
	}
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

// TestProxy_HeadAndConditionalRequestsServedLocally 经真实 NoRoute 验证：持有新鲜
// 副本时 HEAD 与可本地求值的条件请求由本地回答，且它们都不回源。
//
// 服务层测得再绿，只要拉取路径没有把 Method 与条件头交给缓存层，经 HTTP 拉一次
// 仍然是回源一次——这条用例看的就是客户端的可观察结果。
func TestProxy_HeadAndConditionalRequestsServedLocally(t *testing.T) {
	convey.Convey("真实 NoRoute 上 HEAD、If-None-Match、If-Modified-Since 本地命中", t, func() {
		const payload = "hello world"
		const lastModified = "Wed, 21 Oct 2015 07:28:00 GMT"
		srv, hits := countingOrigin(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Range") != "" {
				w.Header().Set("Content-Range", "bytes 0-4/11")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.WriteString(w, "hello")
				return
			}
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Etag", `"v1"`)
			w.Header().Set("Last-Modified", lastModified)
			_, _ = io.WriteString(w, payload)
		})
		upstreamTable(t, &upstream_entity.Upstream{
			ID: 1, Host: "deb.debian.org", Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
			Origin: srv.URL, Enabled: true,
			ImmutablePatterns: upstream_entity.PatternList{"/pool/"},
			MutableTTLSeconds: 60,
		})
		withDiskCache(t)
		const path = "/deb.debian.org/pool/n/nginx.deb"

		first := cacheRequest(t, http.MethodGet, path, nil)
		convey.So(first.Code, convey.ShouldEqual, http.StatusOK)
		convey.So(first.Body.String(), convey.ShouldEqual, payload)
		convey.So(first.Header().Get("X-Katch-Cache"), convey.ShouldEqual, "MISS")
		convey.So(hits.Load(), convey.ShouldEqual, 1)

		hit := cacheRequest(t, http.MethodGet, path, nil)
		convey.So(hit.Code, convey.ShouldEqual, http.StatusOK)
		convey.So(hit.Body.String(), convey.ShouldEqual, payload)
		convey.So(hit.Header().Get("X-Katch-Cache"), convey.ShouldEqual, "HIT")

		// 普通 HEAD：与 GET 同一套状态与元数据，但不发响应体。
		head := cacheRequest(t, http.MethodHead, path, nil)
		convey.So(head.Code, convey.ShouldEqual, http.StatusOK)
		convey.So(head.Body.Len(), convey.ShouldEqual, 0)
		convey.So(head.Header().Get("X-Katch-Cache"), convey.ShouldEqual, "HIT")
		convey.So(head.Header().Get("Content-Type"), convey.ShouldEqual, "text/plain")
		convey.So(head.Header().Get("Content-Length"), convey.ShouldEqual, "11")
		convey.So(head.Header().Get("Etag"), convey.ShouldEqual, `"v1"`)

		// HEAD ignores Range and still reports the full representation length.
		headRange := cacheRequest(t, http.MethodHead, path, http.Header{
			"Range":    []string{"bytes=0-4"},
			"If-Range": []string{`"v1"`},
		})
		convey.So(headRange.Code, convey.ShouldEqual, http.StatusOK)
		convey.So(headRange.Body.Len(), convey.ShouldEqual, 0)
		convey.So(headRange.Header().Get("Content-Length"), convey.ShouldEqual, "11")
		convey.So(headRange.Header().Get("Content-Range"), convey.ShouldBeEmpty)
		convey.So(headRange.Header().Get("X-Katch-Cache"), convey.ShouldEqual, "HIT")

		// If-None-Match 命中：本地 304，不带响应体也不声明实体长度。
		notModified := cacheRequest(t, http.MethodGet, path, http.Header{"If-None-Match": []string{`W/"v1"`}})
		convey.So(notModified.Code, convey.ShouldEqual, http.StatusNotModified)
		convey.So(notModified.Body.Len(), convey.ShouldEqual, 0)
		convey.So(notModified.Header().Get("X-Katch-Cache"), convey.ShouldEqual, "HIT")
		convey.So(notModified.Header().Get("Content-Length"), convey.ShouldBeEmpty)
		convey.So(notModified.Header().Get("Content-Type"), convey.ShouldBeEmpty)

		// HEAD 的条件请求同样本地返回 304。
		headConditional := cacheRequest(t, http.MethodHead, path, http.Header{"If-None-Match": []string{`"v1"`}})
		convey.So(headConditional.Code, convey.ShouldEqual, http.StatusNotModified)
		convey.So(headConditional.Body.Len(), convey.ShouldEqual, 0)
		convey.So(headConditional.Header().Get("X-Katch-Cache"), convey.ShouldEqual, "HIT")
		convey.So(headConditional.Header().Get("Content-Length"), convey.ShouldBeEmpty)

		// If-None-Match 不匹配：本地缓存的 200。
		mismatch := cacheRequest(t, http.MethodGet, path, http.Header{"If-None-Match": []string{`"other"`}})
		convey.So(mismatch.Code, convey.ShouldEqual, http.StatusOK)
		convey.So(mismatch.Body.String(), convey.ShouldEqual, payload)
		convey.So(mismatch.Header().Get("X-Katch-Cache"), convey.ShouldEqual, "HIT")

		// If-Modified-Since：资源未晚于请求时间。
		ims := cacheRequest(t, http.MethodGet, path, http.Header{"If-Modified-Since": []string{lastModified}})
		convey.So(ims.Code, convey.ShouldEqual, http.StatusNotModified)
		convey.So(ims.Header().Get("X-Katch-Cache"), convey.ShouldEqual, "HIT")

		// 以上全部由本地答完，一次都没有回源。
		convey.So(hits.Load(), convey.ShouldEqual, 1)

		// Range 从新鲜完整副本本地选择，不再回源。
		ranged := cacheRequest(t, http.MethodGet, path, http.Header{"Range": []string{"bytes=0-4"}})
		convey.So(ranged.Code, convey.ShouldEqual, http.StatusPartialContent)
		convey.So(ranged.Body.String(), convey.ShouldEqual, "hello")
		convey.So(ranged.Header().Get("Content-Range"), convey.ShouldEqual, "bytes 0-4/11")
		convey.So(ranged.Header().Get("Content-Length"), convey.ShouldEqual, "5")
		convey.So(ranged.Header().Get("X-Katch-Cache"), convey.ShouldEqual, "HIT")
		convey.So(hits.Load(), convey.ShouldEqual, 1)

		// 强 If-Range 匹配时同样由本地返回范围。
		ifRanged := cacheRequest(t, http.MethodGet, path, http.Header{
			"Range":    []string{"bytes=6-"},
			"If-Range": []string{`"v1"`},
		})
		convey.So(ifRanged.Code, convey.ShouldEqual, http.StatusPartialContent)
		convey.So(ifRanged.Body.String(), convey.ShouldEqual, "world")
		convey.So(ifRanged.Header().Get("Content-Range"), convey.ShouldEqual, "bytes 6-10/11")
		convey.So(ifRanged.Header().Get("X-Katch-Cache"), convey.ShouldEqual, "HIT")
		convey.So(hits.Load(), convey.ShouldEqual, 1)

		full := cacheRequest(t, http.MethodGet, path, nil)
		convey.So(full.Code, convey.ShouldEqual, http.StatusOK)
		convey.So(full.Body.String(), convey.ShouldEqual, payload)
		convey.So(full.Header().Get("X-Katch-Cache"), convey.ShouldEqual, "HIT")
		convey.So(hits.Load(), convey.ShouldEqual, 1)
	})
}

// TestProxy_MissPassthroughCarriesMissHeader 由缓存判定而发生的真实回源，即使
// 不能写缓存，响应头也必须标成 MISS。
//
// 规格：缓存未命中、已过期、损坏或 validator 不足而发生的真实回源仍按现有指标与
// 响应头归为未命中。只靠「没有 HIT」不是「归为 MISS」——运维验证以 X-Katch-Cache
// 为准，README 也把缺 validator 的那次透传写成 MISS。
func TestProxy_MissPassthroughCarriesMissHeader(t *testing.T) {
	convey.Convey("static HEAD 无副本与条件请求缺 validator 的透传标 MISS", t, func() {
		srv, hits := countingOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "hello world")
		})
		upstreamTable(t, &upstream_entity.Upstream{
			ID: 1, Host: "deb.debian.org", Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
			Origin: srv.URL, Enabled: true,
			ImmutablePatterns: upstream_entity.PatternList{"/pool/"},
			MutableTTLSeconds: 60,
		})
		withDiskCache(t)
		const path = "/deb.debian.org/pool/n/nginx.deb"

		// 无副本的 static HEAD：真实回源，标 MISS，但不写缓存记录。
		head := cacheRequest(t, http.MethodHead, path, nil)
		convey.So(head.Code, convey.ShouldEqual, http.StatusOK)
		convey.So(head.Header().Get("X-Katch-Cache"), convey.ShouldEqual, "MISS")
		convey.So(head.Body.Len(), convey.ShouldEqual, 0)
		convey.So(hits.Load(), convey.ShouldEqual, 1)

		// 先落一份没有 validator 的副本，条件请求就会因缺 validator 而透传。
		first := cacheRequest(t, http.MethodGet, path, nil)
		convey.So(first.Code, convey.ShouldEqual, http.StatusOK)
		convey.So(hits.Load(), convey.ShouldEqual, 2)

		conditional := cacheRequest(t, http.MethodGet, path,
			http.Header{"If-None-Match": []string{`"v1"`}})
		convey.So(conditional.Code, convey.ShouldEqual, http.StatusOK)
		convey.So(conditional.Body.String(), convey.ShouldEqual, "hello world")
		convey.So(conditional.Header().Get("X-Katch-Cache"), convey.ShouldEqual, "MISS")
		convey.So(hits.Load(), convey.ShouldEqual, 3)
	})
}

// TestProxy_SecondPullIsServedFromDisk
//
// 目标 (a)：两次相同拉取只回源一次，第二次由磁盘服务。这条此前只在 cache_svc
// 上验过，而拉取路径根本没接上缓存层——处理器直接调 proxy_svc.Fetch，
// 于是服务层测得再绿，经 HTTP 拉两次仍然是回源两次。
func TestProxy_SecondPullIsServedFromDisk(t *testing.T) {
	convey.Convey("两次相同拉取只回源一次且第二次由磁盘服务", t, func() {
		payload := strings.Repeat("Package: nginx\n", 4096)
		srv, hits := countingOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, payload)
		})
		upstreamTable(t, &upstream_entity.Upstream{
			ID: 1, Host: "deb.debian.org", Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
			Origin: srv.URL, Enabled: true,
			ImmutablePatterns: upstream_entity.PatternList{"/pool/"},
			MutableTTLSeconds: 60,
		})
		withDiskCache(t)

		const path = "/deb.debian.org/pool/n/nginx/nginx_1.24.0-1_amd64.deb"
		first := request(t, http.MethodGet, path)
		convey.So(first.Code, convey.ShouldEqual, http.StatusOK)
		convey.So(first.Body.String(), convey.ShouldEqual, payload)
		convey.So(hits.Load(), convey.ShouldEqual, 1)

		// 把源站彻底关掉再拉第二次：此后拿得到的字节没有第二种来源。
		srv.Close()

		second := request(t, http.MethodGet, path)
		convey.So(second.Code, convey.ShouldEqual, http.StatusOK)
		convey.So(second.Body.String(), convey.ShouldEqual, payload)
		convey.So(second.Header().Get("Content-Type"), convey.ShouldEqual, "text/plain")
		convey.So(second.Header().Get("X-Katch-Cache"), convey.ShouldEqual, "HIT")
		convey.So(hits.Load(), convey.ShouldEqual, 1)
	})
}

type mutableWebRewriteSource struct {
	mu       sync.Mutex
	snapshot *proxy_svc.RewriteSnapshot
}

func (s *mutableWebRewriteSource) Snapshot(context.Context) (*proxy_svc.RewriteSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	copy := *s.snapshot
	copy.Upstreams = make(map[string]proxy_svc.RewriteUpstream, len(s.snapshot.Upstreams))
	for host, upstream := range s.snapshot.Upstreams {
		upstream.Transports = append(upstream_entity.ProtocolSet(nil), upstream.Transports...)
		copy.Upstreams[host] = upstream
	}
	return &copy, nil
}

func (s *mutableWebRewriteSource) setProfile(profile upstream_entity.PackageProfile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	upstream := s.snapshot.Upstreams["sum.golang.org"]
	upstream.Profile = profile
	s.snapshot.Upstreams["sum.golang.org"] = upstream
}

func TestProxy_SumDBWarmCacheRequiresCurrentGoProxyProfile(t *testing.T) {
	// warmHit 区分两条 sumdb 路径的暖形态：/supported 是合成应答，按约定压根不进
	// 对象缓存；lookup 会落盘，撤销 profile 之前必须先证明它真的暖着——否则
	// 「撤销后拿不到」可能只是因为它从来没被缓存过，这条用例就没有牙。
	//
	// lookup 那条还带上一条 immutable_patterns：撤销 profile 之后这条路径不再被
	// go profile 认领，落回上游自己的不可变模式，缓存层若不显式复核当前 profile，
	// 暖对象就会被当成内容寻址结果长期原样重放。
	for _, tc := range []struct {
		path              string
		warmHit           bool
		immutablePatterns upstream_entity.PatternList
	}{
		{path: "/sumdb/sum.golang.org/supported"},
		{
			path:    "/proxy.golang.org/sumdb/sum.golang.org/lookup/example.com/mod@v1.0.0",
			warmHit: true, immutablePatterns: upstream_entity.PatternList{"/lookup/"},
		},
	} {
		path := tc.path
		t.Run(path, func(t *testing.T) {
			srv, hits := countingOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				_, _ = io.WriteString(w, "checksum-record")
			})
			current := &upstream_entity.Upstream{
				ID: 9, Host: "sum.golang.org", Origin: srv.URL, Enabled: true,
				Protocols:         upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
				PackageProfile:    upstream_entity.PackageProfileGoProxy,
				ImmutablePatterns: tc.immutablePatterns,
			}
			repo := mock_upstream_repo.NewMockUpstreamRepo(gomock.NewController(t))
			repo.EXPECT().List(gomock.Any()).AnyTimes().DoAndReturn(func(context.Context) ([]*upstream_entity.Upstream, error) {
				copy := *current
				copy.Protocols = append(upstream_entity.ProtocolSet(nil), current.Protocols...)
				return []*upstream_entity.Upstream{&copy}, nil
			})
			repo.EXPECT().Save(gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, updated *upstream_entity.Upstream) error {
				copy := *updated
				copy.Protocols = append(upstream_entity.ProtocolSet(nil), updated.Protocols...)
				current = &copy
				return nil
			})
			cachedRepo := proxy_svc.NewCachedUpstreamRepo(repo)
			previousRepo := upstream_repo.Upstream()
			upstream_repo.RegisterUpstream(cachedRepo)
			t.Cleanup(func() { upstream_repo.RegisterUpstream(previousRepo) })

			source := &mutableWebRewriteSource{snapshot: configuredWebSumDB()}
			originURL, err := url.Parse(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			_, port, err := net.SplitHostPort(originURL.Host)
			if err != nil {
				t.Fatal(err)
			}
			resolver := webResolverFunc(func(_ context.Context, target *url.URL, _ destination.DestinationRequirement) (*destination.ResolvedTarget, error) {
				mapped := *target
				mapped.Scheme = "http"
				return &destination.ResolvedTarget{
					URL: &mapped, Authority: target.Host, Host: target.Host,
					ServerName: target.Hostname(), DialAddress: net.JoinHostPort("127.0.0.1", port),
				}, nil
			})
			previousProxy := proxy_svc.Proxy()
			proxy_svc.Register(proxy_svc.New(proxy_svc.Options{RewriteConfig: source, DestinationResolver: resolver}))
			t.Cleanup(func() { proxy_svc.Register(previousProxy) })
			cacheSvc := withDiskCacheOptions(t, cache_svc.Options{RewriteConfig: source})

			first := cacheRequest(t, http.MethodGet, path, nil)
			if first.Code != http.StatusOK {
				t.Fatalf("first GET %s = %d, body %q", path, first.Code, first.Body.String())
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := cacheSvc.Quiesce(ctx); err != nil {
				t.Fatal(err)
			}
			originHits := hits.Load()

			warm := cacheRequest(t, http.MethodGet, path, nil)
			if warm.Code != http.StatusOK {
				t.Fatalf("warm GET %s before revoke = %d, body %q", path, warm.Code, warm.Body.String())
			}
			if got := warm.Header().Get("X-Katch-Cache"); tc.warmHit && got != "HIT" {
				t.Fatalf("warm GET %s before revoke X-Katch-Cache = %q, want HIT", path, got)
			}
			if hits.Load() != originHits {
				t.Fatalf("origin hits before revoke = %d, want %d", hits.Load(), originHits)
			}

			updated := *current
			updated.PackageProfile = upstream_entity.PackageProfileNone
			if err := cachedRepo.Save(context.Background(), &updated); err != nil {
				t.Fatal(err)
			}
			source.setProfile(upstream_entity.PackageProfileNone)

			second := cacheRequest(t, http.MethodGet, path, nil)
			if second.Code != http.StatusServiceUnavailable || second.Body.Len() != 0 {
				t.Fatalf("warm GET %s after profile revoke = %d, body %q; want empty 503", path, second.Code, second.Body.String())
			}
			if second.Header().Get("X-Katch-Cache") == "HIT" {
				t.Fatalf("warm GET %s replayed cache after profile revoke", path)
			}
			if hits.Load() != originHits {
				t.Fatalf("origin hits after revoke = %d, want %d", hits.Load(), originHits)
			}
		})
	}
}

func TestProxy_WarmHitRechecksCurrentTransport(t *testing.T) {
	convey.Convey("移除 static 协议后旧缓存不再可见并返回普通 404", t, func() {
		srv, hits := countingOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = io.WriteString(w, "cached package bytes")
		})
		current := &upstream_entity.Upstream{
			ID: 1, Host: "packages.example.com", Origin: srv.URL, Enabled: true,
			Protocols:         upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
			ImmutablePatterns: upstream_entity.PatternList{"/pool/"}, MutableTTLSeconds: 60,
		}
		repo := mock_upstream_repo.NewMockUpstreamRepo(gomock.NewController(t))
		repo.EXPECT().List(gomock.Any()).AnyTimes().DoAndReturn(func(context.Context) ([]*upstream_entity.Upstream, error) {
			copy := *current
			copy.Protocols = append(upstream_entity.ProtocolSet(nil), current.Protocols...)
			return []*upstream_entity.Upstream{&copy}, nil
		})
		repo.EXPECT().Save(gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, updated *upstream_entity.Upstream) error {
			copy := *updated
			copy.Protocols = append(upstream_entity.ProtocolSet(nil), updated.Protocols...)
			current = &copy
			return nil
		})
		cachedRepo := proxy_svc.NewCachedUpstreamRepo(repo)
		previousRepo := upstream_repo.Upstream()
		upstream_repo.RegisterUpstream(cachedRepo)
		t.Cleanup(func() { upstream_repo.RegisterUpstream(previousRepo) })
		withDiskCache(t)
		const path = "/packages.example.com/pool/main/p/package.deb"

		first := cacheRequest(t, http.MethodGet, path, nil)
		convey.So(first.Code, convey.ShouldEqual, http.StatusOK)
		convey.So(first.Body.String(), convey.ShouldEqual, "cached package bytes")
		convey.So(hits.Load(), convey.ShouldEqual, int64(1))

		updated := *current
		updated.Protocols = upstream_entity.ProtocolSet{upstream_entity.ProtocolRegistry}
		convey.So(cachedRepo.Save(context.Background(), &updated), convey.ShouldBeNil)

		second := cacheRequest(t, http.MethodGet, path, nil)
		convey.So(second.Code, convey.ShouldEqual, http.StatusNotFound)
		convey.So(second.Body.Len(), convey.ShouldEqual, 0)
		convey.So(second.Header().Get("X-Katch-Cache"), convey.ShouldBeEmpty)
		convey.So(hits.Load(), convey.ShouldEqual, int64(1))
	})
}
