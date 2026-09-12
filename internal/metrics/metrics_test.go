package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/smartystreets/goconvey/convey"
)

// 这一层要验的是「拉一次，计数器上多一条什么」。它坐在 SPA/拉取共用的 NoRoute
// 之前，靠响应本身（状态码与 cache_svc 留下的 X-Katch-Cache）判定这次的结果，
// 因此用例也按响应来构造，不 mock 服务层。

// upstreamResponse 假的下游处理器：按测试给的形状产出一次响应。
type upstreamResponse struct {
	status int
	cache  string
	body   string
}

// newTestEngine 拼出和生产一样的形状：中间件在前，拉取处理器挂在 NoRoute 上。
func newTestEngine(r *Recorder, hooks Hooks, resp *upstreamResponse) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(r.Middleware(hooks))
	engine.NoRoute(func(c *gin.Context) {
		if resp.cache != "" {
			c.Header("X-Katch-Cache", resp.cache)
		}
		if resp.body != "" {
			c.Data(resp.status, "application/octet-stream", []byte(resp.body))
			return
		}
		c.AbortWithStatus(resp.status)
	})
	return engine
}

func get(engine *gin.Engine, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

// knownUpstreams 把一组主机名当成上游表里已启用的记录。
func knownUpstreams(hosts ...string) Lookup {
	set := map[string]bool{}
	for _, h := range hosts {
		set[h] = true
	}
	return func(_ context.Context, host string) bool { return set[host] }
}

func TestRecorder_RequestsTotal(t *testing.T) {
	convey.Convey("N 次拉取之后 /metrics 上的计数与实际结果吻合", t, func() {
		reg := prometheus.NewRegistry()
		rec := New(Options{Registerer: reg})
		hooks := Hooks{Lookup: knownUpstreams("deb.debian.org")}

		hit := &upstreamResponse{status: http.StatusOK, cache: "HIT", body: "cached"}
		get(newTestEngine(rec, hooks, hit), "/deb.debian.org/dists/stable/InRelease")
		get(newTestEngine(rec, hooks, hit), "/deb.debian.org/dists/stable/InRelease")
		miss := &upstreamResponse{status: http.StatusOK, cache: "MISS", body: "fromorigin"}
		get(newTestEngine(rec, hooks, miss), "/deb.debian.org/dists/stable/Release")
		bad := &upstreamResponse{status: http.StatusBadGateway}
		get(newTestEngine(rec, hooks, bad), "/deb.debian.org/dists/x")

		body := scrape(reg)
		convey.So(body, convey.ShouldContainSubstring,
			`katch_requests_total{kind="static",result="hit",upstream="deb.debian.org"} 2`)
		convey.So(body, convey.ShouldContainSubstring,
			`katch_requests_total{kind="static",result="miss",upstream="deb.debian.org"} 1`)
		convey.So(body, convey.ShouldContainSubstring,
			`katch_requests_total{kind="static",result="origin_error",upstream="deb.debian.org"} 1`)
		// 命中率这类比值不导出：采样窗口不一致时它会和 hit/miss 自相矛盾，
		// 由读取方用 hit/(hit+miss) 推导（可观测性一节）。
		convey.So(body, convey.ShouldNotContainSubstring, "katch_cache_hit_ratio")
		convey.So(body, convey.ShouldContainSubstring,
			`katch_bytes_served_total{source="cache",upstream="deb.debian.org"} 12`)
		convey.So(body, convey.ShouldContainSubstring,
			`katch_bytes_served_total{source="origin",upstream="deb.debian.org"} 10`)
		convey.So(body, convey.ShouldContainSubstring,
			`katch_request_duration_seconds_count{kind="static",upstream="deb.debian.org"} 4`)
	})
}

func TestRecorder_DeniedAndUnknownHost(t *testing.T) {
	convey.Convey("白名单之外的主机计成 denied，且不把主机名变成新的标签值", t, func() {
		reg := prometheus.NewRegistry()
		rec := New(Options{Registerer: reg})
		hooks := Hooks{Lookup: knownUpstreams("deb.debian.org")}
		notFound := &upstreamResponse{status: http.StatusNotFound}

		get(newTestEngine(rec, hooks, notFound), "/evil.internal/x")
		get(newTestEngine(rec, hooks, notFound), "/another.internal/x")

		body := scrape(reg)
		// 任何人都能用一个没见过的主机名往标签里塞一个新的时间序列，
		// 那是一条不需要密码的内存放大路径，所以未知主机共用一个占位标签。
		convey.So(body, convey.ShouldContainSubstring,
			`katch_requests_total{kind="static",result="denied",upstream="unknown"} 2`)
		convey.So(body, convey.ShouldNotContainSubstring, "evil.internal")

		convey.Convey("未知主机也不该在 rollup 里占一行", func() {
			convey.So(rec.Drain(), convey.ShouldBeEmpty)
		})
	})
}

func TestRecorder_SkipsNonProxyPaths(t *testing.T) {
	convey.Convey("界面与自身端点不计进拉取指标", t, func() {
		reg := prometheus.NewRegistry()
		rec := New(Options{Registerer: reg})
		engine := newTestEngine(rec, Hooks{Lookup: knownUpstreams("deb.debian.org")},
			&upstreamResponse{status: http.StatusOK, body: "<html>"})

		get(engine, "/")
		get(engine, "/api/v1/version")
		get(engine, "/assets/index-abc.js")

		convey.So(scrape(reg), convey.ShouldNotContainSubstring, "katch_requests_total")
	})
}

func TestRecorder_NoLookupRecordsNothing(t *testing.T) {
	convey.Convey("认不出上游时宁可不记", t, func() {
		// 没有 Lookup 就只剩两条路：把请求里的主机名原样当标签（内存放大），
		// 或者全部记成拒绝（一整份错数）。两条都比不记更糟。
		reg := prometheus.NewRegistry()
		rec := New(Options{Registerer: reg})
		get(newTestEngine(rec, Hooks{}, &upstreamResponse{status: http.StatusOK, cache: "HIT", body: "x"}),
			"/deb.debian.org/a")

		convey.So(scrape(reg), convey.ShouldNotContainSubstring, "katch_requests_total")
		convey.So(rec.Drain(), convey.ShouldBeEmpty)
	})
}

func TestRecorder_Drain(t *testing.T) {
	convey.Convey("进程内计数器按分钟桶攒着，等着落进 traffic_rollup", t, func() {
		reg := prometheus.NewRegistry()
		clock := time.Unix(1700000045, 0)
		rec := New(Options{Registerer: reg, Now: func() time.Time { return clock }})
		hooks := Hooks{Lookup: knownUpstreams("deb.debian.org")}

		get(newTestEngine(rec, hooks, &upstreamResponse{status: http.StatusOK, cache: "HIT", body: "abc"}),
			"/deb.debian.org/a")
		get(newTestEngine(rec, hooks, &upstreamResponse{status: http.StatusOK, cache: "MISS", body: "abcde"}),
			"/deb.debian.org/b")
		get(newTestEngine(rec, hooks, &upstreamResponse{status: http.StatusBadGateway}), "/deb.debian.org/c")

		got := rec.Drain()
		convey.So(len(got), convey.ShouldEqual, 1)
		convey.So(got[0].Host, convey.ShouldEqual, "deb.debian.org")
		// 桶是整分钟：45 秒这一刻的请求落在 1700000040 上。
		convey.So(got[0].Bucket, convey.ShouldEqual, int64(1700000040))
		convey.So(got[0].Requests, convey.ShouldEqual, 3)
		convey.So(got[0].Hits, convey.ShouldEqual, 1)
		convey.So(got[0].OriginErrors, convey.ShouldEqual, 1)
		convey.So(got[0].BytesServed, convey.ShouldEqual, 8)
		convey.So(got[0].BytesOrigin, convey.ShouldEqual, 5)

		convey.Convey("取走就清零，下一分钟不会把这一分钟再落一遍", func() {
			convey.So(rec.Drain(), convey.ShouldBeEmpty)
		})

		convey.Convey("跨分钟的请求各自成桶", func() {
			clock = time.Unix(1700000105, 0)
			get(newTestEngine(rec, hooks, &upstreamResponse{status: http.StatusOK, cache: "HIT", body: "x"}),
				"/deb.debian.org/a")
			got := rec.Drain()
			convey.So(len(got), convey.ShouldEqual, 1)
			convey.So(got[0].Bucket, convey.ShouldEqual, int64(1700000100))
		})
	})
}

// fakeGate 记录中间件对退避的反馈。
type fakeGate struct {
	degraded  bool
	failures  int
	successes int
}

func (f *fakeGate) Failure(string)       { f.failures++ }
func (f *fakeGate) Success(string)       { f.successes++ }
func (f *fakeGate) Degraded(string) bool { return f.degraded }

func TestRecorder_BackoffFeedback(t *testing.T) {
	convey.Convey("回源的成败喂给退避，降级状态出现在指标上", t, func() {
		reg := prometheus.NewRegistry()
		rec := New(Options{Registerer: reg})

		convey.Convey("回源失败记一次失败，并把降级标到指标上", func() {
			gate := &fakeGate{degraded: true}
			hooks := Hooks{Lookup: knownUpstreams("deb.debian.org"), Gate: gate}
			get(newTestEngine(rec, hooks, &upstreamResponse{status: http.StatusBadGateway}), "/deb.debian.org/a")

			convey.So(gate.failures, convey.ShouldEqual, 1)
			convey.So(scrape(reg), convey.ShouldContainSubstring,
				`katch_origin_backoff{upstream="deb.debian.org"} 1`)
		})

		convey.Convey("回源成功清掉退避", func() {
			gate := &fakeGate{}
			hooks := Hooks{Lookup: knownUpstreams("deb.debian.org"), Gate: gate}
			get(newTestEngine(rec, hooks, &upstreamResponse{status: http.StatusOK, cache: "MISS", body: "x"}),
				"/deb.debian.org/a")

			convey.So(gate.successes, convey.ShouldEqual, 1)
			convey.So(scrape(reg), convey.ShouldContainSubstring,
				`katch_origin_backoff{upstream="deb.debian.org"} 0`)
		})

		convey.Convey("缓存命中不动退避", func() {
			// 命中根本没碰上游，拿它当「上游恢复了」的证据，只会让一个还在挂的
			// 上游刚出退避就被全量打回去。
			gate := &fakeGate{degraded: true}
			hooks := Hooks{Lookup: knownUpstreams("deb.debian.org"), Gate: gate}
			get(newTestEngine(rec, hooks, &upstreamResponse{status: http.StatusOK, cache: "HIT", body: "x"}),
				"/deb.debian.org/a")

			convey.So(gate.successes, convey.ShouldEqual, 0)
			convey.So(gate.failures, convey.ShouldEqual, 0)
		})

		convey.Convey("被白名单挡住的请求不影响退避", func() {
			gate := &fakeGate{}
			get(newTestEngine(rec, Hooks{Lookup: knownUpstreams(), Gate: gate},
				&upstreamResponse{status: http.StatusNotFound}), "/evil.internal/x")
			convey.So(gate.failures, convey.ShouldEqual, 0)
			convey.So(gate.successes, convey.ShouldEqual, 0)
		})
	})
}

// scrape 按 Prometheus 抓取端点的形态取一次指标文本。
//
// 直接读 collector 的值也能验，但那验不到「/metrics 上看得见」——
// 而任务要的就是拉取之后在那个端点上看到吻合的数值。
func scrape(reg *prometheus.Registry) string {
	w := httptest.NewRecorder()
	promhttp.HandlerFor(reg, promhttp.HandlerOpts{}).
		ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return w.Body.String()
}
