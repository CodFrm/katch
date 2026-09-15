// 用例放在 _test 包里：它经 api.Router 装上生产的那道闸，而 api 包又依赖本包，
// 同包会构成导入环。走 _test 包换来的是「测的就是生产路由 + 生产中间件」。
package rulegate_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/cago-frame/cago/server/mux/muxtest"
	"github.com/gin-gonic/gin"
	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"

	"github.com/CodFrm/katch/internal/api"
	"github.com/CodFrm/katch/internal/cache"
	"github.com/CodFrm/katch/internal/metrics"
	"github.com/CodFrm/katch/internal/model/entity/rule_entity"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/dispatch"
	"github.com/CodFrm/katch/internal/repository/cache_repo"
	mock_cache_repo "github.com/CodFrm/katch/internal/repository/cache_repo/mock"
	"github.com/CodFrm/katch/internal/repository/rule_repo"
	mock_rule_repo "github.com/CodFrm/katch/internal/repository/rule_repo/mock"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
	mock_upstream_repo "github.com/CodFrm/katch/internal/repository/upstream_repo/mock"
	"github.com/CodFrm/katch/internal/service/cache_svc"
	"github.com/CodFrm/katch/internal/service/proxy_svc"
	"github.com/CodFrm/katch/internal/service/rule_svc"
)

// countingOrigin 假源站，记下真的被打了几次。
//
// 「被拒绝的请求不回源」的判据必须是这个计数，而不是某个内部标志位：断言标志位
// 只能证明代码走了我们以为的那条分支，证明不了它没在别处拨过号。
type countingOrigin struct {
	srv  *httptest.Server
	hits atomic.Int64
}

func newCountingOrigin(t *testing.T) *countingOrigin {
	t.Helper()
	o := &countingOrigin{}
	o.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		o.hits.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "Origin: Debian\n")
	}))
	t.Cleanup(o.srv.Close)
	return o
}

// pullHandler 是拉取路径那个 NoRoute 处理器的替身：入口同样是 cache_svc.Get，
// 并记下自己有没有被调到。
//
// 用替身而不是 internal/web 的那一个，是因为它的构造器不导出；换来的判据反而
// 更强——只要这个计数是 0，闸就是在**任何**下游处理器之前收的口，无论下游是谁。
func pullHandler(calls *atomic.Int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		calls.Add(1)
		kind, host, rest := dispatch.Classify(c.Request.URL.EscapedPath())
		if kind != dispatch.KindStatic && kind != dispatch.KindRegistry {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		body, meta, err := cache_svc.Cache().Get(c.Request.Context(), &proxy_svc.Target{
			Kind: kind, Host: host, Path: rest,
			Method: c.Request.Method, Header: c.Request.Header,
		})
		if err != nil {
			// 和 internal/web 那个处理器同一套映射：白名单之外是 404 且不回显
			// 主机名，回源失败才是 502。
			if errors.Is(err, proxy_svc.ErrUpstreamNotAllowed) {
				c.AbortWithStatus(http.StatusNotFound)
				return
			}
			c.AbortWithStatus(http.StatusBadGateway)
			return
		}
		defer func() { _ = body.Close() }()
		c.Writer.WriteHeader(meta.StatusCode)
		_, _ = io.Copy(c.Writer, body)
	}
}

// setupPullPath 装出一条生产形态的拉取路径：先挂 NoRoute，再由 api.Router 装闸，
// 顺序和 main 里 web.MountSPA → mux.HTTP(api.Router) 一致。
func setupPullPath(t *testing.T, rules []*rule_entity.AccessRule, origin string) (
	*gin.Engine, *atomic.Int64,
) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctrl := gomock.NewController(t)

	upRepo := mock_upstream_repo.NewMockUpstreamRepo(ctrl)
	upRepo.EXPECT().FindByHost(gomock.Any(), "deb.debian.org").Return(&upstream_entity.Upstream{
		ID: 7, Host: "deb.debian.org", Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic}, Origin: origin,
		Enabled: true, DefaultPolicy: upstream_entity.PolicyAllowAll,
		ImmutablePatterns: upstream_entity.PatternList{"/pool/"},
	}, nil).AnyTimes()
	upRepo.EXPECT().FindByHost(gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	upstream_repo.RegisterUpstream(upRepo)

	ruleRepo := mock_rule_repo.NewMockAccessRuleRepo(ctrl)
	ruleRepo.EXPECT().List(gomock.Any()).Return(rules, nil).AnyTimes()
	rule_repo.RegisterAccessRule(ruleRepo)
	prev := rule_svc.Rule()
	rule_svc.Register(rule_svc.New())
	t.Cleanup(func() { rule_svc.Register(prev) })

	testMux := muxtest.NewTestMux()
	engine, ok := testMux.IRouter.(*gin.Engine)
	if !ok {
		t.Fatal("muxtest 的 IRouter 不再是 *gin.Engine")
	}
	// 计数中间件按生产形态排在闸之前：拉取路径的结果由响应本身判定，403 就是
	// denied。装配顺序和 main 一致（mux.RegisterMiddleware 先于 api.Router）。
	engine.Use(metrics.Default().Middleware(metrics.Hooks{
		Lookup: func(_ context.Context, host string) bool { return host == "deb.debian.org" },
	}))
	calls := &atomic.Int64{}
	engine.NoRoute(pullHandler(calls))
	if err := api.Router(context.Background(), testMux.Router); err != nil {
		t.Fatal(err)
	}
	return engine, calls
}

// countFiles 数一数缓存目录里落了几个文件。
func countFiles(t *testing.T, root string) int {
	t.Helper()
	n := 0
	if err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			n++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestPull_GlobalDenyBeatsEqualUpstreamAllow 任务目标 (a) 与 (c)。
//
// 全局 deny 与上游 allow 的字面前缀、通配符数量完全相同——也就是说具体度定序
// 分不出胜负，决定结果的只能是「全局先于上游内」这一层顺序（决策 14）。这条
// 请求必须 403，而且全程不回源、不写缓存。
func TestPull_GlobalDenyBeatsEqualUpstreamAllow(t *testing.T) {
	convey.Convey("全局 deny 压过同样具体的上游 allow，拉取被 403 拒绝", t, func() {
		o := newCountingOrigin(t)
		engine, downstream := setupPullPath(t, []*rule_entity.AccessRule{
			{ID: 1, UpstreamID: 0, Action: rule_entity.ActionDeny, Pattern: "dists/*"},
			{ID: 2, UpstreamID: 7, Action: rule_entity.ActionAllow, Pattern: "dists/*"},
		}, o.srv.URL)

		// 缓存是真的：盘上有没有多出文件，是「不写缓存」唯一说得清的判据。
		dir := t.TempDir()
		store, err := cache.NewStore(dir)
		convey.So(err, convey.ShouldBeNil)
		// 缓存记录表一个 EXPECT 都没有：只要判定放行进了缓存层，mock 当场失败。
		cache_repo.RegisterCacheObject(mock_cache_repo.NewMockCacheObjectRepo(gomock.NewController(t)))
		prev := cache_svc.Cache()
		cache_svc.Register(cache_svc.New(store, cache_svc.Options{}))
		t.Cleanup(func() { cache_svc.Register(prev) })
		before := countFiles(t, dir)
		metrics.Drain() // 清掉别的用例留下的桶，下面数的才是这一次。

		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(
			http.MethodGet, "/deb.debian.org/dists/stable/InRelease", nil))

		convey.So(w.Code, convey.ShouldEqual, http.StatusForbidden)
		convey.Convey("不回源", func() {
			convey.So(o.hits.Load(), convey.ShouldEqual, 0)
		})
		convey.Convey("不写缓存：闸在拉取处理器之前就收了口，盘上没多出任何文件", func() {
			convey.So(downstream.Load(), convey.ShouldEqual, 0)
			convey.So(countFiles(t, dir), convey.ShouldEqual, before)
		})
		convey.Convey("403 的响应体里不回显主机名", func() {
			convey.So(w.Body.String(), convey.ShouldNotContainSubstring, "deb.debian.org")
		})
		convey.Convey("这次拒绝计进了 denied，且没有一个字节算在回源头上", func() {
			buckets := metrics.Drain()
			convey.So(len(buckets), convey.ShouldEqual, 1)
			convey.So(buckets[0].Host, convey.ShouldEqual, "deb.debian.org")
			convey.So(buckets[0].Requests, convey.ShouldEqual, 1)
			convey.So(buckets[0].Denied, convey.ShouldEqual, 1)
			convey.So(buckets[0].BytesOrigin, convey.ShouldEqual, 0)
		})
	})
}

// TestPull_AllowedPathStillReachesOrigin 闸不是一把全关的开关：没有被规则拒绝的
// 路径必须照常回源，否则上面那条 403 只是因为什么都过不去。
func TestPull_AllowedPathStillReachesOrigin(t *testing.T) {
	convey.Convey("没被拒绝的路径照常回源", t, func() {
		o := newCountingOrigin(t)
		engine, _ := setupPullPath(t, []*rule_entity.AccessRule{
			{ID: 1, UpstreamID: 0, Action: rule_entity.ActionDeny, Pattern: "dists/*"},
		}, o.srv.URL)
		// cache_svc 保持出厂的纯透传形态，这条用例只看回源那一段。
		prev := cache_svc.Cache()
		cache_svc.Register(cache_svc.New(nil, cache_svc.Options{}))
		t.Cleanup(func() { cache_svc.Register(prev) })

		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(
			http.MethodGet, "/deb.debian.org/pool/main/n/nginx.deb", nil))

		convey.So(w.Code, convey.ShouldEqual, http.StatusOK)
		convey.So(o.hits.Load(), convey.ShouldEqual, 1)
		convey.So(w.Body.String(), convey.ShouldEqual, "Origin: Debian\n")
	})
}

// TestPull_UnknownUpstreamStaysA404 白名单之外的主机不能因为多了一道规则闸就
// 换一种回应：403 和 404 之间的差别，会把 katch 变成一个探测内网主机是否存在的
// 工具（决策 6）。
func TestPull_UnknownUpstreamStaysA404(t *testing.T) {
	convey.Convey("不在上游表里的主机仍然是 404", t, func() {
		o := newCountingOrigin(t)
		engine, _ := setupPullPath(t, []*rule_entity.AccessRule{
			{ID: 1, UpstreamID: 0, Action: rule_entity.ActionDeny, Pattern: "*"},
		}, o.srv.URL)
		prev := cache_svc.Cache()
		cache_svc.Register(cache_svc.New(nil, cache_svc.Options{}))
		t.Cleanup(func() { cache_svc.Register(prev) })

		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/evil.example.com/x", nil))

		convey.So(w.Code, convey.ShouldEqual, http.StatusNotFound)
		convey.So(o.hits.Load(), convey.ShouldEqual, 0)
	})
}

// TestPull_AdminAndSPAPathsAreNotGated 闸挂在 gin 引擎上，所以每条路由都会经过
// 它。它只认拉取路径，管理接口与前端路由必须原样通过——否则一条 deny "*" 的
// 规则会把界面本身一起关掉，连进去把它删了都做不到。
func TestPull_AdminAndSPAPathsAreNotGated(t *testing.T) {
	convey.Convey("管理接口与前端路由不受访问规则影响", t, func() {
		o := newCountingOrigin(t)
		engine, _ := setupPullPath(t, []*rule_entity.AccessRule{
			{ID: 1, UpstreamID: 0, Action: rule_entity.ActionDeny, Pattern: "*"},
		}, o.srv.URL)

		// /api/v1/version 是公开端点，/admin/rules 是前端路由：一条 deny "*"
		// 不能把这两条路一起关掉。
		for _, path := range []string{"/api/v1/version", "/admin/rules"} {
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			convey.So(w.Code, convey.ShouldNotEqual, http.StatusForbidden)
		}
	})
}
