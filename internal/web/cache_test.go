package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"

	"github.com/CodFrm/katch/internal/cache"
	"github.com/CodFrm/katch/internal/model/entity/cache_entity"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/repository/cache_repo"
	mock_cache_repo "github.com/CodFrm/katch/internal/repository/cache_repo/mock"
	"github.com/CodFrm/katch/internal/service/cache_svc"
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
	memoryCacheObjects(t)
	store, err := cache.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("建缓存目录失败：%v", err)
	}
	cache_svc.Register(cache_svc.New(store, cache_svc.Options{}))
	t.Cleanup(func() { cache_svc.Register(cache_svc.New(nil, cache_svc.Options{})) })
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
			ID: 1, Host: "deb.debian.org", Kind: upstream_entity.KindStatic,
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
