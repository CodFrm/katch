// 用例放在 _test 包里：它经 api.Router 装出生产路由，而 api 又依赖 internal/proxy
// 下的包，同包会构成导入环。
package extension_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

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
	"github.com/CodFrm/katch/internal/metrics"
	"github.com/CodFrm/katch/internal/proxy/backoff"
	"github.com/CodFrm/katch/internal/proxy/dispatch"
	"github.com/CodFrm/katch/internal/repository/cache_repo"
	"github.com/CodFrm/katch/internal/repository/rollup_repo"
	"github.com/CodFrm/katch/internal/repository/rule_repo"
	"github.com/CodFrm/katch/internal/repository/setting_repo"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
	"github.com/CodFrm/katch/internal/service/cache_svc"
	"github.com/CodFrm/katch/internal/service/proxy_svc"
	"github.com/CodFrm/katch/internal/service/setting_svc"
	"github.com/CodFrm/katch/internal/service/upstream_svc"
	"github.com/CodFrm/katch/migrations"
)

const (
	// fakeUpstreamHost 一个此前不存在的上游主机名。
	//
	// 它是不是「这个仓库里从没出现过」，不靠人读一遍代码来保证——
	// TestFakeUpstreamIsNotAFixture 会自己去核实。任何一处生产代码为了让这条
	// 用例通过而认识它，都会被那条用例当场抓住。
	fakeUpstreamHost = "packages.brand-new-mirror.invalid"
	// fakeUpstreamPath APT 形态的一条不可变路径，注册时用 /pool/ 这个模式盖住它。
	fakeUpstreamPath = "/pool/main/g/guard/guard_1.0_all.deb"
	fakeUpstreamBody = "假上游的一个对象，内容寻址、长期缓存。\n"
	adminKey         = "extension-guard-admin-key"
)

// fakeOrigin 假上游的源站。
//
// 「第二次拉取是命中」的判据是这个计数没有再涨，而不只是响应头上的 HIT：
// 响应头是 katch 自己写的一句话，计数是对面有没有真的被打到。
type fakeOrigin struct {
	srv  *httptest.Server
	hits atomic.Int64
}

func newFakeOrigin(t *testing.T) *fakeOrigin {
	t.Helper()
	o := &fakeOrigin{}
	o.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != fakeUpstreamPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		o.hits.Add(1)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, fakeUpstreamBody)
	}))
	t.Cleanup(o.srv.Close)
	return o
}

// pullHandler 是拉取路径那个 NoRoute 处理器的替身：入口同样是 cache_svc.Get，
// 错误映射、响应头与状态码的处理和 internal/web 的 serveProxy 一致。
//
// 用替身是因为那个处理器的构造器不导出，而本任务不该去改 internal/web。生产的
// 那一个由 scripts/smoke.sh 在真二进制上覆盖——两处守卫合起来才是完整的一条路。
func pullHandler(c *gin.Context) {
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

// startKatch 把一套 katch 装起来并「跑着」：装配顺序照抄 cmd/katch/main.go，
// 库是真 sqlite、缓存是真磁盘、仓储一个 mock 都没有。
//
// 这条用例要验的恰恰是「写进库的一条记录能不能被拉取路径读到」，中间任何一层换成
// mock，都等于把待验的那件事替换成用例自己的假设。
//
// 返回之后**不再碰任何装配函数**：后面的注册与拉取全部经 HTTP 打进同一个 engine，
// 「不重启进程」这句话就是由这个结构保证的——没有第二次装配可言。
func startKatch(t *testing.T) (*gin.Engine, *fakeOrigin) {
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

	// 仓储照 main 装配，上游那层进程内缓存**必须**包上：它正是「改完不用重启」
	// 这句话在拉取热路径上的兑现点，去掉它这条用例就只是在测一次数据库读写。
	upstream_repo.RegisterUpstream(proxy_svc.NewCachedUpstreamRepo(upstream_repo.NewUpstream()))
	setting_repo.RegisterSetting(setting_repo.NewSetting())
	cache_repo.RegisterCacheObject(cache_repo.NewCacheObject())
	rollup_repo.RegisterTrafficRollup(rollup_repo.NewTrafficRollup())
	rule_repo.RegisterAccessRule(rule_repo.NewAccessRule())
	if err := setting_svc.Setting().EnsureAdminKey(ctx, adminKey); err != nil {
		t.Fatal(err)
	}

	store, err := cache.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cache_svc.Register(cache_svc.New(store, cache_svc.Options{}))
	tracker := backoff.New(backoff.Options{})
	proxy_svc.Register(proxy_svc.New(proxy_svc.Options{Gate: tracker}))

	testMux := muxtest.NewTestMux()
	engine, ok := testMux.IRouter.(*gin.Engine)
	if !ok {
		t.Fatal("muxtest 的 IRouter 不再是 *gin.Engine")
	}
	// 顺序同 main：计数中间件与 NoRoute 由 mux.RegisterMiddleware 先装，
	// 规则闸由 api.Router 随后挂上。
	engine.Use(metrics.Default().Middleware(metrics.Hooks{
		Gate: tracker,
		// 和 main 一样问 upstream_svc，而不是在用例里写死一个主机名：
		// 「新上游在指标上也是一等公民」同样属于这条边界。
		Lookup: func(ctx context.Context, host string) bool {
			upstream, err := upstream_svc.Upstream().FindByHost(ctx, host)
			return err == nil && upstream != nil
		},
	}))
	engine.NoRoute(pullHandler)
	if err := api.Router(ctx, testMux.Router); err != nil {
		t.Fatal(err)
	}
	return engine, newFakeOrigin(t)
}

// call 发一次请求给这个已经跑着的 katch。
func call(engine *gin.Engine, method, path, body string, header http.Header) *httptest.ResponseRecorder {
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

// TestNewUpstream_RegisteredThroughAdminAPIOnly 任务目标 (a) 与 (b)。
//
// 一条 katch 从没见过的上游，只经管理接口加一条记录，就要在同一个还跑着的进程里
// 完成「拉取 → 缓存 → 命中」。这条用例是 spec 那句「不改代码、不重启进程」唯一
// 说得清的判据：它不 import 任何为它开的口子，用的全是对外的 HTTP 契约。
//
// 三件事因此会被同时守住：管理接口能把一条上游完整地写进去；拉取路径能不经任何
// 重新装配就看见它；缓存层认得它记录上的不可变模式。任何一件要靠改生产代码才能
// 成立，这里就会红。
func TestNewUpstream_RegisteredThroughAdminAPIOnly(t *testing.T) {
	convey.Convey("经管理接口注册一个此前不存在的上游，不重启即可拉取并命中缓存", t, func() {
		engine, origin := startKatch(t)
		pullPath := "/" + fakeUpstreamHost + fakeUpstreamPath
		metrics.Drain() // 清掉之前的桶，后面数的才是这条用例自己的。

		// ① 注册之前：这个主机名对拉取路径来说根本不存在。
		//
		// 这一步不只是对照组，它还把「表里没有这个主机」这个答案塞进了上游那层
		// 进程内快照。后面那次拉取要成立，管理接口的写入就必须把快照掀掉——
		// 这正是「不重启进程」在实现上要兑现的那件事。
		before := call(engine, http.MethodGet, pullPath, "", nil)
		convey.So(before.Code, convey.ShouldEqual, http.StatusNotFound)
		convey.So(origin.hits.Load(), convey.ShouldEqual, 0)

		// ② 只经管理接口注册。没有 SQL、没有配置文件、没有任何装配函数，
		// 整条记录就是 spec 说的那四样：主机名、协议类别、回源地址、不可变模式。
		saved := call(engine, http.MethodPost, "/api/v1/admin/upstreams", `{
			"host": "`+fakeUpstreamHost+`",
			"kind": "static",
			"origin": "`+origin.srv.URL+`",
			"enabled": true,
			"immutable_patterns": ["/pool/"]
		}`, http.Header{
			"Authorization": []string{"Bearer " + adminKey},
			"Content-Type":  []string{"application/json"},
		})
		convey.So(saved.Code, convey.ShouldEqual, http.StatusOK)
		convey.So(saved.Body.String(), convey.ShouldContainSubstring, `"code":0`)

		// ③ 同一个进程、同一个 engine，立刻就能拉通。
		first := call(engine, http.MethodGet, pullPath, "", nil)
		convey.So(first.Code, convey.ShouldEqual, http.StatusOK)
		convey.So(first.Body.String(), convey.ShouldEqual, fakeUpstreamBody)
		convey.So(first.Header().Get("X-Katch-Cache"), convey.ShouldEqual, "MISS")
		convey.So(origin.hits.Load(), convey.ShouldEqual, 1)

		// ④ 第二次由磁盘服务：源站计数不再涨，是它没被打到的判据。
		second := call(engine, http.MethodGet, pullPath, "", nil)
		convey.So(second.Code, convey.ShouldEqual, http.StatusOK)
		convey.So(second.Body.String(), convey.ShouldEqual, fakeUpstreamBody)
		convey.So(second.Header().Get("X-Katch-Cache"), convey.ShouldEqual, "HIT")
		convey.So(origin.hits.Load(), convey.ShouldEqual, 1)

		// ⑤ 新上游在统计上也是一等公民：它有自己的标签，而不是落进 unknown。
		// 跨整分钟时同一个上游会有两个桶，所以按主机名求和。
		var requests, hits int64
		for _, bucket := range metrics.Drain() {
			if bucket.Host == fakeUpstreamHost {
				requests += bucket.Requests
				hits += bucket.Hits
			}
		}
		convey.So(requests, convey.ShouldEqual, 2)
		convey.So(hits, convey.ShouldEqual, 1)
	})
}

// TestNewUpstream_StaysUnknownWithoutRegistration 上面那条用例的反面。
//
// 少了它，一个「所有主机一律放行」的实现也能让上面全绿——那样的话这条边界守的
// 就不再是扩展性，而是把白名单拆了。
func TestNewUpstream_StaysUnknownWithoutRegistration(t *testing.T) {
	convey.Convey("没经管理接口注册过的主机，拉多少次都是 404 且不回源", t, func() {
		engine, origin := startKatch(t)
		pullPath := "/" + fakeUpstreamHost + fakeUpstreamPath

		for range 2 {
			w := call(engine, http.MethodGet, pullPath, "", nil)
			convey.So(w.Code, convey.ShouldEqual, http.StatusNotFound)
			convey.So(w.Body.String(), convey.ShouldNotContainSubstring, fakeUpstreamHost)
		}
		convey.So(origin.hits.Load(), convey.ShouldEqual, 0)
	})
}

// TestFakeUpstreamIsNotAFixture 守卫的守卫。
//
// 上面那条用例只有在「生产代码根本不认识这个主机名」时才证明得了扩展点：一旦
// 有人往某个 switch 或某张默认表里加上它，用例照样绿，而那时它测的已经是一条
// 内置上游，不是扩展点。所以这里直接去仓库里核实一遍，不靠约定。
func TestFakeUpstreamIsNotAFixture(t *testing.T) {
	convey.Convey("假上游的主机名不出现在任何生产 Go 代码里", t, func() {
		_, thisFile, _, ok := runtime.Caller(0)
		convey.So(ok, convey.ShouldBeTrue)
		root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))

		var mentions []string
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				// 前端依赖与产物目录跟这条边界无关，进去只会白读几万个文件。
				switch entry.Name() {
				case ".git", "node_modules", "dist", "bin", "data", "runtime":
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			content, err := os.ReadFile(path) // #nosec G304 -- 路径来自本仓库的遍历
			if err != nil {
				return err
			}
			if strings.Contains(string(content), fakeUpstreamHost) {
				mentions = append(mentions, path)
			}
			return nil
		})
		convey.So(err, convey.ShouldBeNil)
		convey.So(mentions, convey.ShouldBeEmpty)
	})
}
