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
	// miss 缓存层留下的回源原因，空表示这次响应上没有归因。
	miss string
	body string
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
		if resp.miss != "" {
			c.Header(MissHeader, resp.miss)
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

// TestRecorder_SpecMetricFamilies 可观测性一节点名的每一个 katch_* 指标都要在
// /metrics 上看得见。
//
// 按「名字 + 标签」而不是按数值断言：这一条守的是「这个族有没有被导出」，
// 数值由各自的用例去验。族名写死成字面量而不是引用常量——常量改名时这里应该
// 红，因为 /metrics 是对外契约，改名就是破坏采集方。
func TestRecorder_SpecMetricFamilies(t *testing.T) {
	convey.Convey("spec 可观测性一节列出的指标族全部出现在 /metrics 上", t, func() {
		reg := prometheus.NewRegistry()
		rec := New(Options{Registerer: reg})

		// 拉取路径那三族由中间件产出。
		hooks := Hooks{Lookup: knownUpstreams("deb.debian.org")}
		get(newTestEngine(rec, hooks, &upstreamResponse{
			status: http.StatusOK, cache: "HIT", body: "cached",
		}), "/deb.debian.org/pool/main/a.deb")

		// 回源侧：一次成功的回源，外加一个还没结束的在途请求。
		done := rec.OriginStarted("deb.debian.org")
		rec.RecordOrigin("deb.debian.org", http.StatusOK, 120*time.Millisecond)
		rec.SetBackoff("deb.debian.org", false)

		// 缓存侧。
		rec.SetCacheUsage([]CacheUsage{{Upstream: "deb.debian.org", Objects: 3, Bytes: 4096}})
		rec.RecordEviction(EvictionLRU, 2)
		rec.RecordEviction(EvictionTTL, 1)
		rec.RecordEviction(EvictionManual, 5)
		rec.RecordIntegrityFailure("deb.debian.org")

		// 规则与 token。
		rec.RecordRuleDecision("global", "deny")
		rec.RecordTokenExchange("docker.io", "success")

		body := scrape(reg)
		for _, want := range []string{
			`katch_requests_total{kind="static",result="hit",upstream="deb.debian.org"} 1`,
			`katch_request_duration_seconds_count{kind="static",upstream="deb.debian.org"} 1`,
			`katch_bytes_served_total{source="cache",upstream="deb.debian.org"} 6`,
			`katch_origin_requests_total{status="200",upstream="deb.debian.org"} 1`,
			`katch_origin_duration_seconds_count{upstream="deb.debian.org"} 1`,
			`katch_origin_inflight{upstream="deb.debian.org"} 1`,
			`katch_origin_backoff{upstream="deb.debian.org"} 0`,
			`katch_cache_objects{upstream="deb.debian.org"} 3`,
			`katch_cache_bytes{upstream="deb.debian.org"} 4096`,
			`katch_cache_evictions_total{reason="lru"} 2`,
			`katch_cache_evictions_total{reason="ttl"} 1`,
			`katch_cache_evictions_total{reason="manual"} 5`,
			`katch_cache_integrity_failures_total{upstream="deb.debian.org"} 1`,
			`katch_rule_decisions_total{decision="deny",scope="global"} 1`,
			`katch_token_exchanges_total{result="success",upstream="docker.io"} 1`,
		} {
			convey.So(body, convey.ShouldContainSubstring, want)
		}

		convey.Convey("在途请求结束后 inflight 回到零", func() {
			done()
			convey.So(scrape(reg), convey.ShouldContainSubstring,
				`katch_origin_inflight{upstream="deb.debian.org"} 0`)
		})
	})
}

// TestRecorder_CacheUsageReplacesPreviousSnapshot 缓存占用是 gauge，不是累加量。
//
// 每轮刷新必须覆盖上一轮，而且要把这一轮不再出现的上游清零：一个被清空缓存的
// 上游若留着上一轮的数字，界面和采集方都会一直看到一份早已不存在的占用。
func TestRecorder_CacheUsageReplacesPreviousSnapshot(t *testing.T) {
	convey.Convey("缓存占用按快照覆盖，消失的上游归零", t, func() {
		reg := prometheus.NewRegistry()
		rec := New(Options{Registerer: reg})

		rec.SetCacheUsage([]CacheUsage{
			{Upstream: "deb.debian.org", Objects: 3, Bytes: 4096},
			{Upstream: "docker.io", Objects: 9, Bytes: 8192},
		})
		rec.SetCacheUsage([]CacheUsage{{Upstream: "deb.debian.org", Objects: 1, Bytes: 512}})

		body := scrape(reg)
		convey.So(body, convey.ShouldContainSubstring, `katch_cache_objects{upstream="deb.debian.org"} 1`)
		convey.So(body, convey.ShouldContainSubstring, `katch_cache_bytes{upstream="deb.debian.org"} 512`)
		convey.So(body, convey.ShouldContainSubstring, `katch_cache_objects{upstream="docker.io"} 0`)
		convey.So(body, convey.ShouldContainSubstring, `katch_cache_bytes{upstream="docker.io"} 0`)
	})
}

// TestRecorder_MissReasonsPartitionEveryRequest 未命中的四个原因加上命中、拒绝与
// 回源失败，要正好把这一分钟的请求分完。
//
// 分完这件事本身就是断言：界面上那条占比条的分母是「未命中数」，而未命中数是
// requests 减去其余三项。四个原因要是漏掉某一类未命中，占比之和就不是 100%，
// 而少掉的那一块在图上看不出来——它只是让别的几项看起来比实际大。
func TestRecorder_MissReasonsPartitionEveryRequest(t *testing.T) {
	convey.Convey("四个回源原因把未命中分完，一个请求都不落下", t, func() {
		reg := prometheus.NewRegistry()
		clock := time.Unix(1700000045, 0)
		rec := New(Options{Registerer: reg, Now: func() time.Time { return clock }})
		hooks := Hooks{Lookup: knownUpstreams("deb.debian.org")}

		pull := func(resp *upstreamResponse) {
			get(newTestEngine(rec, hooks, resp), "/deb.debian.org/pool/main/a.deb")
		}
		pull(&upstreamResponse{status: http.StatusOK, cache: "HIT", body: "x"})
		pull(&upstreamResponse{status: http.StatusOK, cache: "HIT", body: "x"})
		pull(&upstreamResponse{status: http.StatusOK, cache: "MISS", miss: "first", body: "x"})
		pull(&upstreamResponse{status: http.StatusOK, cache: "MISS", miss: "ttl", body: "x"})
		pull(&upstreamResponse{status: http.StatusOK, cache: "MISS", miss: "evicted", body: "x"})
		pull(&upstreamResponse{status: http.StatusOK, cache: "MISS", miss: "changed", body: "x"})
		// 没有归因的未命中：透传的 HEAD/Range 和缓存层根本没碰的请求都长这样。
		pull(&upstreamResponse{status: http.StatusOK, cache: "MISS", body: "x"})
		pull(&upstreamResponse{status: http.StatusForbidden})
		pull(&upstreamResponse{status: http.StatusBadGateway})

		got := rec.Drain()
		convey.So(len(got), convey.ShouldEqual, 1)
		b := got[0]
		convey.So(b.Requests, convey.ShouldEqual, 9)
		convey.So(b.Hits, convey.ShouldEqual, 2)
		convey.So(b.Denied, convey.ShouldEqual, 1)
		convey.So(b.OriginErrors, convey.ShouldEqual, 1)
		// 认不出的那次算首次拉取：那条路上没有任何一份被判定为可复用的副本，
		// 而四项之和必须等于未命中数，否则占比条会少一块。
		convey.So(b.MissFirst, convey.ShouldEqual, 2)
		convey.So(b.MissTTL, convey.ShouldEqual, 1)
		convey.So(b.MissEvicted, convey.ShouldEqual, 1)
		convey.So(b.MissChanged, convey.ShouldEqual, 1)
		convey.So(b.MissFirst+b.MissTTL+b.MissEvicted+b.MissChanged,
			convey.ShouldEqual, b.Requests-b.Hits-b.Denied-b.OriginErrors)
	})
}

// TestRecorder_HitRecordsNoMissReason 命中一个原因计数都不落。
func TestRecorder_HitRecordsNoMissReason(t *testing.T) {
	convey.Convey("缓存命中不落任何回源原因", t, func() {
		reg := prometheus.NewRegistry()
		rec := New(Options{Registerer: reg})
		hooks := Hooks{Lookup: knownUpstreams("deb.debian.org")}

		// 命中的响应上不会有归因头，但就算缓存层留了一个（比如同一次回源的
		// 尾随读者最后读到的是已经落库的副本），命中也不该记成回源。
		get(newTestEngine(rec, hooks, &upstreamResponse{
			status: http.StatusOK, cache: "HIT", miss: "ttl", body: "x",
		}), "/deb.debian.org/pool/main/a.deb")

		got := rec.Drain()
		convey.So(len(got), convey.ShouldEqual, 1)
		convey.So(got[0].Hits, convey.ShouldEqual, 1)
		convey.So(got[0].MissFirst, convey.ShouldEqual, 0)
		convey.So(got[0].MissTTL, convey.ShouldEqual, 0)
		convey.So(got[0].MissEvicted, convey.ShouldEqual, 0)
		convey.So(got[0].MissChanged, convey.ShouldEqual, 0)
	})
}
