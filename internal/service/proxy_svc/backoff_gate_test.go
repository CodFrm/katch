package proxy_svc_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"

	"github.com/CodFrm/katch/internal/cache"
	"github.com/CodFrm/katch/internal/model/entity/cache_entity"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/backoff"
	"github.com/CodFrm/katch/internal/proxy/dispatch"
	"github.com/CodFrm/katch/internal/repository/cache_repo"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
	mock_upstream_repo "github.com/CodFrm/katch/internal/repository/upstream_repo/mock"
	"github.com/CodFrm/katch/internal/service/cache_svc"
	"github.com/CodFrm/katch/internal/service/proxy_svc"
)

// 这一组用例站在包外（proxy_svc_test）：退避闸要证明的两件事分处缓存的两侧，
// 「命中仍然服务」只有把 cache_svc 真的叠在 proxy_svc 上才说得清，而 cache_svc
// 反过来依赖 proxy_svc，包内测试引不进来。

// gatedOrigin 假源站，记下真的被打了几次。
//
// 快速失败要证明的是「没有打到上游」，所以判据是这个计数不再增长，
// 而不是「调用方拿到了一个错误」——连不上的拨号同样会给出错误。
type gatedOrigin struct {
	srv  *httptest.Server
	hits atomic.Int64
}

func newGatedOrigin(t *testing.T, handler http.HandlerFunc) *gatedOrigin {
	t.Helper()
	o := &gatedOrigin{}
	o.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(o.srv.Close)
	return o
}

// useGate 把带退避闸的 proxy_svc 装成进程内那一份，用例结束后还原：
// 默认实现是包级单例，不还原会把闸漏给同一个测试二进制里的其它用例。
func useGate(t *testing.T, gate proxy_svc.Gate) {
	t.Helper()
	prev := proxy_svc.Proxy()
	proxy_svc.Register(proxy_svc.New(proxy_svc.Options{Gate: gate}))
	t.Cleanup(func() { proxy_svc.Register(prev) })
}

// registerUpstream 注册一条指向假源站的 static 上游，装配形态与 main 一致。
func registerUpstream(t *testing.T, host string, originURL string) {
	t.Helper()
	repo := mock_upstream_repo.NewMockUpstreamRepo(gomock.NewController(t))
	repo.EXPECT().List(gomock.Any()).Return([]*upstream_entity.Upstream{{
		ID: 7, Host: host, Kind: upstream_entity.KindStatic, Origin: originURL, Enabled: true,
		ImmutablePatterns: upstream_entity.PatternList{"/pool/"}, MutableTTLSeconds: 60,
	}}, nil).AnyTimes()
	upstream_repo.RegisterUpstream(proxy_svc.NewCachedUpstreamRepo(repo))
}

// downTracker 返回一个已经把 host 判成降级的退避器，以及它走的那只假时钟。
func downTracker(host string, clock *time.Time) *backoff.Tracker {
	tracker := backoff.New(backoff.Options{
		Threshold: 3, Base: time.Minute, Max: time.Minute,
		Now: func() time.Time { return *clock },
	})
	for i := 0; i < 3; i++ {
		tracker.Failure(host)
	}
	return tracker
}

func staticTarget(host, path string) *proxy_svc.Target {
	return &proxy_svc.Target{
		Kind: dispatch.KindStatic, Host: host, Path: path,
		Method: http.MethodGet, Header: http.Header{},
	}
}

// TestFetch_BackoffFailsFastWithoutDialing 目标 (e) 的「期间快速失败」。
//
// 退避已经把 degraded 与 retry_at 报了出去，但回源那一步从没问过它：一个正在
// 超时的上游仍旧被每个请求各拨一次号，客户端等的还是完整的超时，katch 也还在
// 给一个已经限流的源站继续加压。
func TestFetch_BackoffFailsFastWithoutDialing(t *testing.T) {
	convey.Convey("上游在退避窗口里就不回源", t, func() {
		o := newGatedOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "Origin: Debian\n")
		})
		registerUpstream(t, "deb.debian.org", o.srv.URL)
		clock := time.Unix(1700000000, 0)
		tracker := downTracker("deb.debian.org", &clock)
		useGate(t, tracker)

		_, _, err := proxy_svc.Proxy().Fetch(context.Background(),
			staticTarget("deb.debian.org", "/dists/stable/InRelease"))

		// 判据在这一行，而且必须排在错误断言前面：快速失败要证明的是
		// 「没有打到上游」，连不上的拨号同样会给出一个错误。
		convey.So(o.hits.Load(), convey.ShouldEqual, 0)
		convey.So(errors.Is(err, proxy_svc.ErrUpstreamBackoff), convey.ShouldBeTrue)

		convey.Convey("窗口过去之后照常放行探测", func() {
			clock = clock.Add(time.Minute)
			body, meta, err := proxy_svc.Proxy().Fetch(context.Background(),
				staticTarget("deb.debian.org", "/dists/stable/InRelease"))
			convey.So(err, convey.ShouldBeNil)
			defer func() { _ = body.Close() }()
			convey.So(meta.StatusCode, convey.ShouldEqual, http.StatusOK)
			convey.So(o.hits.Load(), convey.ShouldEqual, 1)
		})

		convey.Convey("没进退避的上游不受影响", func() {
			convey.So(tracker.Allow("proxy.golang.org"), convey.ShouldBeTrue)
		})
	})
}

// TestGet_BackoffStillServesCachedObject 闸装在回源缝上而不是缓存前面的理由。
//
// 决策 17 把退避限定在回源失败上：盘上已有的副本不花上游任何成本，上游降级期间
// 它们必须照常服务——否则一个上游抖动会把已经拉过的镜像层一起变成 502。
func TestGet_BackoffStillServesCachedObject(t *testing.T) {
	convey.Convey("上游降级期间缓存命中照常服务", t, func() {
		const body = "deb package bytes"
		o := newGatedOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/vnd.debian.binary-package")
			_, _ = io.WriteString(w, body)
		})
		registerUpstream(t, "deb.debian.org", o.srv.URL)
		cache_repo.RegisterCacheObject(newMemCacheRepo())
		store, err := cache.NewStore(t.TempDir())
		convey.So(err, convey.ShouldBeNil)
		svc := cache_svc.New(store, cache_svc.Options{})
		ctx := context.Background()

		clock := time.Unix(1700000000, 0)
		tracker := backoff.New(backoff.Options{
			Threshold: 3, Base: time.Minute, Max: time.Minute,
			Now: func() time.Time { return clock },
		})
		useGate(t, tracker)

		// 上游还好着的时候先拉一次，把这个对象留在盘上。
		first, _, err := svc.Get(ctx, staticTarget("deb.debian.org", "/pool/main/n/nginx.deb"))
		convey.So(err, convey.ShouldBeNil)
		got, err := io.ReadAll(first)
		convey.So(err, convey.ShouldBeNil)
		convey.So(first.Close(), convey.ShouldBeNil)
		convey.So(string(got), convey.ShouldEqual, body)
		convey.So(o.hits.Load(), convey.ShouldEqual, 1)

		// 然后上游连续失败进入退避。
		for i := 0; i < 3; i++ {
			tracker.Failure("deb.debian.org")
		}
		convey.So(tracker.Degraded("deb.debian.org"), convey.ShouldBeTrue)

		second, meta, err := svc.Get(ctx, staticTarget("deb.debian.org", "/pool/main/n/nginx.deb"))
		convey.So(err, convey.ShouldBeNil)
		got2, err := io.ReadAll(second)
		convey.So(err, convey.ShouldBeNil)
		convey.So(second.Close(), convey.ShouldBeNil)
		convey.So(string(got2), convey.ShouldEqual, body)
		convey.So(meta.StatusCode, convey.ShouldEqual, http.StatusOK)
		// 命中没有再打上游一次，也没有被闸挡住。
		convey.So(o.hits.Load(), convey.ShouldEqual, 1)

		convey.Convey("同一个上游上没缓存过的对象仍然快速失败", func() {
			_, _, err := svc.Get(ctx, staticTarget("deb.debian.org", "/pool/main/n/other.deb"))
			convey.So(o.hits.Load(), convey.ShouldEqual, 1)
			convey.So(errors.Is(err, proxy_svc.ErrUpstreamBackoff), convey.ShouldBeTrue)
		})
	})
}

// memCacheRepo 内存缓存记录表。只实现命中/落库这条路上用到的方法，
// 其余方法留给内嵌的 nil 接口——真被调到就 panic，而不是悄悄答一个假值。
type memCacheRepo struct {
	cache_repo.CacheObjectRepo
	mu   sync.Mutex
	next int64
	rows map[string]*cache_entity.CacheObject
}

func newMemCacheRepo() *memCacheRepo {
	return &memCacheRepo{next: 1, rows: map[string]*cache_entity.CacheObject{}}
}

func (m *memCacheRepo) key(upstreamID int64, key string) string {
	return strconv.FormatInt(upstreamID, 10) + "\x00" + key
}

func (m *memCacheRepo) FindByKey(_ context.Context, upstreamID int64, key string) (*cache_entity.CacheObject, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[m.key(upstreamID, key)]
	if !ok {
		return nil, nil
	}
	dup := *row
	return &dup, nil
}

func (m *memCacheRepo) Save(_ context.Context, object *cache_entity.CacheObject) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if object.ID == 0 {
		object.ID = m.next
		m.next++
	}
	dup := *object
	m.rows[m.key(object.UpstreamID, object.Key)] = &dup
	return nil
}

func (m *memCacheRepo) Touch(_ context.Context, id int64, at int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, row := range m.rows {
		if row.ID == id {
			row.HitCount++
			row.LastAccessAt = at
		}
	}
	return nil
}

func (m *memCacheRepo) TotalSize(_ context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	total := int64(0)
	for _, row := range m.rows {
		total += row.Size
	}
	return total, nil
}
