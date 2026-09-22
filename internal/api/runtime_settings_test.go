// 用例放在 _test 包里：它经 api.Router 装出生产路由，而 api 又依赖 service 与
// internal/proxy 下的包，同包会构成导入环。
package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cago-frame/cago/database/db"
	"github.com/cago-frame/cago/pkg/logger"
	"github.com/cago-frame/cago/server/mux/muxtest"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/CodFrm/katch/internal/api"
	"github.com/CodFrm/katch/internal/cache"
	"github.com/CodFrm/katch/internal/proxy/dispatch"
	"github.com/CodFrm/katch/internal/repository/cache_repo"
	"github.com/CodFrm/katch/internal/repository/rollup_repo"
	"github.com/CodFrm/katch/internal/repository/rule_repo"
	"github.com/CodFrm/katch/internal/repository/setting_repo"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
	"github.com/CodFrm/katch/internal/service/cache_svc"
	"github.com/CodFrm/katch/internal/service/proxy_svc"
	"github.com/CodFrm/katch/internal/service/setting_svc"
	"github.com/CodFrm/katch/migrations"
)

// 这一组用例守的是决策 3/4 共同的那条理由：运行时项改完**立刻生效，不必重启进程**。
//
// 判据只有一个形态：设置经 HTTP 管理接口写进一个**已经跑着的** handler，随后由同一个
// handler 上的下一个请求观察到新行为。没有第二次装配、没有「用新 Options 再 New 一个
// service」——那种写法证明的是构造函数认得参数，而不是设置改完生效了。

const (
	// liveAdminKey 这一组用例用的管理密钥。
	liveAdminKey = "runtime-settings-live-admin-key"
	// liveUpstreamHost 假上游的主机名，只经管理接口注册。
	liveUpstreamHost = "packages.runtime-settings.invalid"
	// liveObjectSize 每个缓存对象的字节数，配额算术都按它来。
	liveObjectSize = 4096
)

// liveOrigin 假上游源站，按路径前缀演不同的上游行为。
//
// 它同时记下「同时有几个请求压在源站上」——回源并发上限唯一说得清的判据是对面
// 看见了几个并发，而不是 katch 自己数了几个。
type liveOrigin struct {
	srv *httptest.Server
	// inflight、maxInflight 此刻与历史峰值的并发回源数。
	inflight    atomic.Int64
	maxInflight atomic.Int64
	// slowHeader /slow/ 路径在写响应头之前先睡多久。
	slowHeader time.Duration
	// holdBody /hold/ 路径在写完响应之前停留多久，用来制造重叠的回源。
	holdBody time.Duration

	mu sync.Mutex
	// attempts 每条路径被真正打到几次。重试要的是这个数，不是响应码。
	attempts map[string]int
}

func newLiveOrigin(t *testing.T) *liveOrigin {
	t.Helper()
	o := &liveOrigin{
		slowHeader: 1200 * time.Millisecond,
		holdBody:   150 * time.Millisecond,
		attempts:   map[string]int{},
	}
	o.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := o.inflight.Add(1)
		for {
			peak := o.maxInflight.Load()
			if now <= peak || o.maxInflight.CompareAndSwap(peak, now) {
				break
			}
		}
		defer o.inflight.Add(-1)

		path := r.URL.Path
		o.mu.Lock()
		o.attempts[path]++
		attempt := o.attempts[path]
		o.mu.Unlock()

		switch {
		case strings.HasPrefix(path, "/flaky/") && attempt == 1:
			// 第一次拨过来就把连接掐了：这是一次拿不到响应的回源失败，
			// 也就是重试次数这个设置唯一管得着的那种失败。
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				return
			}
			_ = conn.Close()
			return
		case strings.HasPrefix(path, "/slow/"):
			time.Sleep(o.slowHeader)
		case strings.HasPrefix(path, "/hold/"):
			defer time.Sleep(o.holdBody)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, strings.Repeat("k", liveObjectSize))
	}))
	t.Cleanup(o.srv.Close)
	return o
}

func (o *liveOrigin) attemptsOf(path string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.attempts[path]
}

// livePullHandler 是拉取路径那个 NoRoute 处理器的替身：入口同样是 cache_svc.Get，
// 错误映射与 internal/web 的 serveProxy 一致。用替身是因为那个处理器的构造器不导出，
// 而本任务不该去改 internal/web。
func livePullHandler(c *gin.Context) {
	kind, host, rest := dispatch.Classify(c.Request.URL.EscapedPath())
	if kind != dispatch.KindStatic && kind != dispatch.KindRegistry {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	body, meta, err := cache_svc.Cache().Get(c.Request.Context(), &proxy_svc.Target{
		Kind: kind, Host: host, Path: rest, RawQuery: c.Request.URL.RawQuery,
		Method: c.Request.Method, Header: c.Request.Header,
	})
	if err != nil {
		if errors.Is(err, proxy_svc.ErrUpstreamNotAllowed) {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.AbortWithStatus(http.StatusBadGateway)
		return
	}
	defer func() { _ = body.Close() }()
	header := c.Writer.Header()
	for k, values := range meta.Header {
		for _, v := range values {
			header.Add(k, v)
		}
	}
	c.Writer.WriteHeader(meta.StatusCode)
	_, _ = io.Copy(c.Writer, body)
}

// startLiveKatch 把一套 katch 装起来并「跑着」，装配顺序照抄 cmd/katch/main.go。
//
// 返回之后**不再碰任何装配函数**：后面的设置写入与拉取全部经 HTTP 打进同一个
// engine，「不重启进程」这句话就是由这个结构保证的——没有第二次装配可言。
//
// 退避闸刻意不装：这几条用例会故意制造回源失败（超时、断连），装上之后连续失败会
// 让后面的请求在退避那一缝上快速失败，观察到的就不再是设置这一项的效果了。
func startLiveKatch(t *testing.T) (*gin.Engine, *liveOrigin) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	// 拉取路径上有 logger.Ctx：没有实例时它返回 nil，回源或写缓存出错那几支会当场 panic。
	logger.SetLogger(zap.NewNop())
	ctx := context.Background()

	gormDB, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "katch.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if sqlDB, err := gormDB.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := migrations.RunMigrations(gormDB); err != nil {
		t.Fatal(err)
	}
	db.SetDefault(gormDB)

	upstream_repo.RegisterUpstream(proxy_svc.NewCachedUpstreamRepo(upstream_repo.NewUpstream()))
	// 设置表照 main 装配，进程内缓存**必须**包上：它正是「改完不用重启」在热路径上
	// 的兑现点——少了它这几条用例照样绿，而生产上每个请求都要为设置查一次库。
	setting_repo.RegisterSetting(setting_svc.NewCachedSettingRepo(setting_repo.NewSetting()))
	cache_repo.RegisterCacheObject(cache_repo.NewCacheObject())
	rollup_repo.RegisterTrafficRollup(rollup_repo.NewTrafficRollup())
	rule_repo.RegisterAccessRule(rule_repo.NewAccessRule())
	if err := setting_svc.Setting().EnsureAdminKey(ctx, liveAdminKey); err != nil {
		t.Fatal(err)
	}

	store, err := cache.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cacheSvc := cache_svc.New(store, cache_svc.Options{})
	cache_svc.Register(cacheSvc)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := cacheSvc.Quiesce(ctx); err != nil {
			t.Errorf("等待后台缓存任务结束: %v", err)
		}
	})
	proxy_svc.Register(proxy_svc.New(proxy_svc.Options{}))

	testMux := muxtest.NewTestMux()
	engine, ok := testMux.IRouter.(*gin.Engine)
	if !ok {
		t.Fatal("muxtest 的 IRouter 不再是 *gin.Engine")
	}
	engine.NoRoute(livePullHandler)
	if err := api.Router(ctx, testMux.Router); err != nil {
		t.Fatal(err)
	}
	return engine, newLiveOrigin(t)
}

// liveCall 发一次请求给这个已经跑着的 katch。
func liveCall(engine *gin.Engine, method, path, body string, header http.Header) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	for k, values := range header {
		for _, v := range values {
			req.Header.Add(k, v)
		}
	}
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

// liveAdmin 带管理密钥发一次请求。
func liveAdmin(engine *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	return liveCall(engine, method, path, body, http.Header{
		"Authorization": []string{"Bearer " + liveAdminKey},
		"Content-Type":  []string{"application/json"},
	})
}

// registerLiveUpstream 只经管理接口注册那条假上游。
func registerLiveUpstream(t *testing.T, engine *gin.Engine, o *liveOrigin) {
	t.Helper()
	w := liveAdmin(engine, http.MethodPost, "/api/v1/admin/upstreams", `{
		"host": "`+liveUpstreamHost+`",
		"protocols": ["static"],
		"origin": "`+o.srv.URL+`",
		"enabled": true,
		"immutable_patterns": ["/pool/", "/hold/", "/slow/", "/flaky/"]
	}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"code":0`) {
		t.Fatalf("注册上游失败：%d %s", w.Code, w.Body.String())
	}
}

// saveLiveSettings 经管理接口写设置，这是「改设置」唯一被允许的入口。
func saveLiveSettings(t *testing.T, engine *gin.Engine, settings string) {
	t.Helper()
	w := liveAdmin(engine, http.MethodPost, "/api/v1/admin/settings", `{"settings":`+settings+`}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"code":0`) {
		t.Fatalf("写设置失败：%d %s", w.Code, w.Body.String())
	}
}

// livePull 拉一个对象。
func livePull(engine *gin.Engine, path string) *httptest.ResponseRecorder {
	return liveCall(engine, http.MethodGet, "/"+liveUpstreamHost+path, "", nil)
}

// cachedObjects 问管理接口此刻缓存里有几条记录。
func cachedObjects(t *testing.T, engine *gin.Engine) int64 {
	t.Helper()
	w := liveAdmin(engine, http.MethodGet, "/api/v1/admin/cache/objects?size=200", "")
	if w.Code != http.StatusOK {
		t.Fatalf("查缓存对象失败：%d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Code int `json:"code"`
		Data struct {
			Total int64 `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析缓存对象响应失败：%v", err)
	}
	return resp.Data.Total
}

// cachedObjectOf 取一条缓存记录在管理接口上的样子，没有就返回 nil。
func cachedObjectOf(t *testing.T, engine *gin.Engine, key string) *liveCacheObject {
	t.Helper()
	w := liveAdmin(engine, http.MethodGet, "/api/v1/admin/cache/objects?size=200", "")
	if w.Code != http.StatusOK {
		t.Fatalf("查缓存对象失败：%d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			List []*liveCacheObject `json:"list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析缓存对象响应失败：%v", err)
	}
	for _, object := range resp.Data.List {
		if object.Key == key {
			return object
		}
	}
	return nil
}

// liveCacheObject 管理接口给出的缓存记录里这几条用例用得上的部分。
type liveCacheObject struct {
	Key       string `json:"key"`
	Immutable bool   `json:"immutable"`
	ExpiresAt int64  `json:"expires_at"`
}

// eventually 等一个条件成立。
//
// 淘汰发生在下载收尾的那个 goroutine 里，客户端拿到最后一个字节时它可能还没跑完；
// 直接断言等于在赌调度顺序。
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等不到：%s", what)
}

// TestCacheQuota_TakesEffectOnNextPull 目标 (a)：运行中改小缓存配额，
// 下一次超额拉取立刻按新配额触发回收。
func TestCacheQuota_TakesEffectOnNextPull(t *testing.T) {
	convey.Convey("经管理接口改小缓存配额，下一次拉取就按新配额回收", t, func() {
		engine, origin := startLiveKatch(t)
		registerLiveUpstream(t, engine, origin)

		// ① 出厂配额是 10 GiB，三个对象一条都不该被淘汰。
		for _, name := range []string{"a", "b", "c"} {
			convey.So(livePull(engine, "/pool/"+name+".deb").Code, convey.ShouldEqual, http.StatusOK)
		}
		eventually(t, "三条缓存记录都落库", func() bool { return cachedObjects(t, engine) == 3 })

		// ② 把配额改到两个对象都放不下，回收水位 50%。
		saveLiveSettings(t, engine, fmt.Sprintf(
			`{"cache_quota_bytes": %d, "cache_reclaim_percent": 50}`, 2*liveObjectSize+808))

		// ③ 同一个进程、同一个 handler：下一次拉取就该按新配额回收到水位之下。
		//    水位是 9000 * 50% = 4500 字节，只放得下一个对象。
		convey.So(livePull(engine, "/pool/d.deb").Code, convey.ShouldEqual, http.StatusOK)
		eventually(t, "缓存被回收到新配额的水位之下", func() bool { return cachedObjects(t, engine) == 1 })
	})
}

// TestMutableTTL_TakesEffectOnNextPull 目标 (a) 的另一半：可变对象的默认 TTL。
//
// 可变对象（tag、InRelease）靠 TTL 过期而不是靠淘汰（决策 7），所以这一项改完不
// 生效的表现是「缓存久了会发出过期内容」——比配额失效更不容易被看见。
func TestMutableTTL_TakesEffectOnNextPull(t *testing.T) {
	convey.Convey("经管理接口改可变对象的默认 TTL，下一次写进缓存就按新值过期", t, func() {
		engine, origin := startLiveKatch(t)
		registerLiveUpstream(t, engine, origin)

		// ① 出厂默认是 300 秒。/dists/ 不在这条上游的不可变模式里，所以它是可变对象。
		convey.So(livePull(engine, "/dists/InRelease").Code, convey.ShouldEqual, http.StatusOK)
		var first *liveCacheObject
		eventually(t, "第一个可变对象落库", func() bool {
			first = cachedObjectOf(t, engine, "/dists/InRelease")
			return first != nil
		})
		convey.So(first.Immutable, convey.ShouldBeFalse)
		convey.So(first.ExpiresAt-time.Now().Unix(), convey.ShouldBeBetweenOrEqual, 290, 300)

		// ② 改成 60 秒。
		saveLiveSettings(t, engine, `{"mutable_ttl_seconds": 60}`)

		// ③ 同一个进程：下一个写进缓存的可变对象就按新 TTL 过期。
		convey.So(livePull(engine, "/dists/Release").Code, convey.ShouldEqual, http.StatusOK)
		var second *liveCacheObject
		eventually(t, "第二个可变对象落库", func() bool {
			second = cachedObjectOf(t, engine, "/dists/Release")
			return second != nil
		})
		convey.So(second.ExpiresAt-time.Now().Unix(), convey.ShouldBeBetweenOrEqual, 50, 60)
	})
}

// TestOriginTimeout_TakesEffectOnNextPull 目标 (b) 的上游超时那一半。
func TestOriginTimeout_TakesEffectOnNextPull(t *testing.T) {
	convey.Convey("经管理接口改小上游超时，下一次回源就按新值超时", t, func() {
		engine, origin := startLiveKatch(t)
		registerLiveUpstream(t, engine, origin)

		// ① 出厂超时是 30 秒，一个慢 1.2 秒才给响应头的上游照样拉得通。
		convey.So(livePull(engine, "/slow/1.deb").Code, convey.ShouldEqual, http.StatusOK)

		// ② 改成 1 秒。
		saveLiveSettings(t, engine, `{"origin_timeout_seconds": 1}`)

		// ③ 同一个上游、同样的慢，这一次必须超时：拿不到响应是 502，
		//    而不是被当成「这个对象不存在」的 404。
		convey.So(livePull(engine, "/slow/2.deb").Code, convey.ShouldEqual, http.StatusBadGateway)
	})
}

// TestOriginConcurrency_TakesEffectOnNextPull 目标 (b) 的回源并发那一半。
func TestOriginConcurrency_TakesEffectOnNextPull(t *testing.T) {
	convey.Convey("经管理接口改小回源并发上限，下一批回源就按新值排队", t, func() {
		engine, origin := startLiveKatch(t)
		registerLiveUpstream(t, engine, origin)

		pullAll := func(names ...string) {
			var wg sync.WaitGroup
			for _, name := range names {
				wg.Add(1)
				go func() {
					defer wg.Done()
					livePull(engine, "/hold/"+name+".deb")
				}()
			}
			wg.Wait()
		}

		// ① 出厂并发上限是 32：三个互不相同的对象会同时压在源站上。
		pullAll("a", "b", "c")
		convey.So(origin.maxInflight.Load(), convey.ShouldBeGreaterThan, 1)

		// ② 改成 1。
		saveLiveSettings(t, engine, `{"origin_concurrency": 1}`)
		origin.maxInflight.Store(0)

		// ③ 同一个进程：下一批回源只能一个一个来。
		pullAll("d", "e", "f")
		convey.So(origin.maxInflight.Load(), convey.ShouldEqual, 1)
	})
}

// TestOriginRetries_TakesEffectOnNextPull 目标 (b) 的回源重试那一项。
func TestOriginRetries_TakesEffectOnNextPull(t *testing.T) {
	convey.Convey("经管理接口改回源重试次数，下一次回源就按新值重试", t, func() {
		engine, origin := startLiveKatch(t)
		registerLiveUpstream(t, engine, origin)

		// ① 先关掉重试：第一次拨过去就被掐断，这次拉取只能是 502。
		saveLiveSettings(t, engine, `{"origin_retries": 0}`)
		convey.So(livePull(engine, "/flaky/1.deb").Code, convey.ShouldEqual, http.StatusBadGateway)
		convey.So(origin.attemptsOf("/flaky/1.deb"), convey.ShouldEqual, 1)

		// ② 开到 2 次。
		saveLiveSettings(t, engine, `{"origin_retries": 2}`)

		// ③ 同样的上游、同样的掐断，这一次由重试救回来。
		convey.So(livePull(engine, "/flaky/2.deb").Code, convey.ShouldEqual, http.StatusOK)
		convey.So(origin.attemptsOf("/flaky/2.deb"), convey.ShouldEqual, 2)
	})
}

// TestSiteDomain_TakesEffectOnNextRequest 目标 (c)：改站点域名后，
// 公开接口给出的拉取命令随之变化。
func TestSiteDomain_TakesEffectOnNextRequest(t *testing.T) {
	convey.Convey("经管理接口改站点域名，下一个公开请求给出的命令就换了主机名", t, func() {
		engine, origin := startLiveKatch(t)
		registerLiveUpstream(t, engine, origin)

		// ① 没配域名时公开接口不编一个出来：命令里的站点地址由调用方自己的
		//    地址栏兜底，服务端不替它猜。
		before := liveCall(engine, http.MethodGet, "/api/v1/site", "", nil)
		convey.So(before.Code, convey.ShouldEqual, http.StatusOK)
		convey.So(before.Body.String(), convey.ShouldContainSubstring, `"base_url":""`)
		convey.So(before.Body.String(), convey.ShouldContainSubstring, `"name":"katch"`)

		// ② 配上站点名称与域名。
		saveLiveSettings(t, engine, `{"site_name": "我的镜像站", "site_domain": "mirror.example.com"}`)

		// ③ 同一个进程：下一个请求给出的就是新域名拼出来的命令。
		after := liveCall(engine, http.MethodGet, "/api/v1/site", "", nil)
		convey.So(after.Code, convey.ShouldEqual, http.StatusOK)
		convey.So(after.Body.String(), convey.ShouldContainSubstring, `"base_url":"https://mirror.example.com"`)
		convey.So(after.Body.String(), convey.ShouldContainSubstring, `"name":"我的镜像站"`)
	})
}
