package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cago-frame/cago/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
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

// TestRecorder_SumDBAliasPullsAreCounted
//
// checksum database 的两条公开别名也是拉取：它们照常打上游、照常占缓存，
// 只是路径上多了一段保留命名空间。漏掉它们，命中率会被冲淡成另一个数，
// 最近请求页上也再看不到 go 的校验流量。
func TestRecorder_SumDBAliasPullsAreCounted(t *testing.T) {
	convey.Convey("sumdb 别名路径按 static 记在 sum.golang.org 名下", t, func() {
		reg := prometheus.NewRegistry()
		rec := New(Options{Registerer: reg})
		hooks := Hooks{Lookup: knownUpstreams("sum.golang.org")}
		miss := &upstreamResponse{status: http.StatusOK, cache: "MISS",
			miss: string(MissFirst), body: "checksum"}

		get(newTestEngine(rec, hooks, miss), "/sumdb/sum.golang.org/lookup/example.com/mod@v1.0.0")
		get(newTestEngine(rec, hooks, miss),
			"/proxy.golang.org/sumdb/sum.golang.org/tile/8/1/000.p/16")

		body := scrape(reg)
		convey.So(body, convey.ShouldContainSubstring,
			`katch_requests_total{kind="static",result="miss",upstream="sum.golang.org"} 2`)
		convey.So(body, convey.ShouldContainSubstring,
			`katch_bytes_served_total{source="origin",upstream="sum.golang.org"} 16`)
		convey.So(rec.DrainRecent(), convey.ShouldHaveLength, 2)
	})
}

// TestRecorder_SumDBAliasIsNeverGit
//
// git 端点寄生在 static 的路径空间里，所以只有 static 会按路径形态认 git
// （web/embed.go 里的同一条判据）。sumdb 的保留命名空间下不存在 git 端点：
// 拿它当 git 记，等于任何人都能拼一个 /sumdb/... 的路径往 git 那一维里灌数。
func TestRecorder_SumDBAliasIsNeverGit(t *testing.T) {
	convey.Convey("sumdb 别名下形如 git 的路径仍记成 static", t, func() {
		reg := prometheus.NewRegistry()
		rec := New(Options{Registerer: reg})
		hooks := Hooks{Lookup: knownUpstreams("sum.golang.org")}
		miss := &upstreamResponse{status: http.StatusOK, cache: "MISS",
			miss: string(MissFirst), body: "checksum"}

		get(newTestEngine(rec, hooks, miss),
			"/sumdb/sum.golang.org/lookup/example.com/info/refs?service=git-upload-pack")

		body := scrape(reg)
		convey.So(body, convey.ShouldContainSubstring,
			`katch_requests_total{kind="static",result="miss",upstream="sum.golang.org"} 1`)
		convey.So(body, convey.ShouldNotContainSubstring, `kind="git"`)
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

// captureLogs 把全局日志换成一个按级别过滤的内存 core。
//
// 用 observer 的 core 而不是自己接一个求值函数：级别拦截必须真的走 zap 的
// level 检查，这样「默认级别下这一行根本不写」才和线上是同一件事。
func captureLogs(t *testing.T, level zapcore.Level) *observer.ObservedLogs {
	t.Helper()
	core, logs := observer.New(level)
	logger.SetLogger(zap.New(core))
	t.Cleanup(func() { logger.SetLogger(zap.NewNop()) })
	return logs
}

// TestRecorder_PullLogLevel 逐请求的拉取行从 info 降到了 debug（决策 1）。
//
// 它唯一的消费者是「最近请求」面板，面板改读库之后这行没有消费者；默认级别
// 把它整个摘掉，排障时把级别调到 debug 仍要拿回同一行、同样的字段。
func TestRecorder_PullLogLevel(t *testing.T) {
	convey.Convey("逐请求的拉取行只在 debug 级别出现，字段一个不少", t, func() {
		reg := prometheus.NewRegistry()
		clock := time.Unix(1700000045, 0)
		rec := New(Options{Registerer: reg, Now: func() time.Time { return clock }})
		hooks := Hooks{Lookup: knownUpstreams("deb.debian.org")}
		pull := &upstreamResponse{status: http.StatusOK, cache: "MISS", body: "abcde"}

		convey.Convey("默认级别（info）下这一行整个消失", func() {
			logs := captureLogs(t, zap.InfoLevel)
			get(newTestEngine(rec, hooks, pull), "/deb.debian.org/pool/main/a.deb")

			convey.So(logs.FilterMessage(PullLogMessage).Len(), convey.ShouldEqual, 0)
		})

		convey.Convey("调到 debug 之后同一行的字段与旧行一字不差", func() {
			logs := captureLogs(t, zap.DebugLevel)
			get(newTestEngine(rec, hooks, pull), "/deb.debian.org/pool/main/a.deb")

			entries := logs.FilterMessage(PullLogMessage).All()
			convey.So(entries, convey.ShouldHaveLength, 1)
			convey.So(entries[0].Level, convey.ShouldEqual, zap.DebugLevel)
			// 字段名与取值逐个比对，不只看有没有那一行：降级别时最容易
			// 顺手改掉的就是字段，而面板排障读的就是这几个。
			convey.So(entries[0].ContextMap(), convey.ShouldResemble, map[string]any{
				"at":          int64(1700000045),
				"upstream":    "deb.debian.org",
				"object":      "/pool/main/a.deb",
				"result":      "miss",
				"bytes":       int64(5),
				"duration_ms": int64(0),
			})
		})
	})
}

// TestRecorder_RecentBuffer 一次拉取结束后记进进程内的环形缓冲（记录与落库一节）。
//
// 热路径在这里只做一次内存写入：不查库、不写库、不等待。落库由 request_svc 每秒
// 取走一批，因此这一层只负责攒住与交出。
func TestRecorder_RecentBuffer(t *testing.T) {
	convey.Convey("拉取结束后记进环形缓冲", t, func() {
		reg := prometheus.NewRegistry()
		clock := time.Unix(1700000045, 0)
		rec := New(Options{Registerer: reg, Now: func() time.Time { return clock }})
		hooks := Hooks{Lookup: knownUpstreams("deb.debian.org")}

		get(newTestEngine(rec, hooks, &upstreamResponse{status: http.StatusOK, cache: "MISS", body: "abcde"}),
			"/deb.debian.org/pool/main/a.deb")

		got := rec.DrainRecent()
		convey.So(got, convey.ShouldHaveLength, 1)
		// at 是这次拉取**结束**的时刻，与落库用的那一列同源。
		convey.So(got[0], convey.ShouldResemble, RecentRequest{
			Upstream: "deb.debian.org",
			At:       1700000045,
			Object:   "/pool/main/a.deb",
			Result:   ResultMiss,
			Bytes:    5,
		})

		convey.Convey("取走即清空，落库那一侧不会把同一批落两遍", func() {
			convey.So(rec.DrainRecent(), convey.ShouldBeEmpty)
		})
	})

	convey.Convey("满了丢最旧，剩下的顺序不变", t, func() {
		rec := New(Options{Registerer: prometheus.NewRegistry()})
		for i := 0; i < RecentRequestCapacity+3; i++ {
			rec.appendRecent(RecentRequest{Upstream: "deb.debian.org", DurationMS: int64(i)})
		}

		got := rec.DrainRecent()
		convey.So(got, convey.ShouldHaveLength, RecentRequestCapacity)
		convey.So(got[0].DurationMS, convey.ShouldEqual, 3)
		convey.So(got[len(got)-1].DurationMS, convey.ShouldEqual, int64(RecentRequestCapacity+2))
	})

	convey.Convey("并发 append 与 drain 不丢不重", t, func() {
		rec := New(Options{Registerer: prometheus.NewRegistry()})
		const writers, perWriter = 8, 500

		var writersDone sync.WaitGroup
		for w := 0; w < writers; w++ {
			writersDone.Add(1)
			go func(base int) {
				defer writersDone.Done()
				for i := 0; i < perWriter; i++ {
					rec.appendRecent(RecentRequest{
						Upstream:   "deb.debian.org",
						DurationMS: int64(base*perWriter + i),
					})
				}
			}(w)
		}

		var (
			drained []RecentRequest
			stop    = make(chan struct{})
			drainer sync.WaitGroup
		)
		drainer.Add(1)
		go func() {
			defer drainer.Done()
			for {
				drained = append(drained, rec.DrainRecent()...)
				select {
				case <-stop:
					drained = append(drained, rec.DrainRecent()...)
					return
				default:
					runtime.Gosched()
				}
			}
		}()

		writersDone.Wait()
		close(stop)
		drainer.Wait()

		seen := make(map[int64]bool, writers*perWriter)
		for _, item := range drained {
			seen[item.DurationMS] = true
		}
		convey.So(drained, convey.ShouldHaveLength, writers*perWriter)
		convey.So(seen, convey.ShouldHaveLength, writers*perWriter)
	})

	convey.Convey("超长的 object 截到缓冲/表那一列的宽度", t, func() {
		// 路径长度由调用方决定：不截的话，一行最长可以是一整条请求行，而缓冲
		// 按行数封顶、库里的那一列又只有 512——一个超长路径足以让整批插入在
		// MySQL 上失败，把同一秒里其余的行一起带走。
		rec := New(Options{Registerer: prometheus.NewRegistry()})
		hooks := Hooks{Lookup: knownUpstreams("deb.debian.org")}
		long := "/" + strings.Repeat("a", RecentRequestObjectLimit+200)
		get(newTestEngine(rec, hooks, &upstreamResponse{status: http.StatusOK, cache: "MISS"}),
			"/deb.debian.org"+long)

		got := rec.DrainRecent()
		convey.So(got, convey.ShouldHaveLength, 1)
		convey.So(got[0].Object, convey.ShouldEqual, long[:RecentRequestObjectLimit])
	})

	convey.Convey("未知主机不进缓冲", t, func() {
		// 和日志、分钟桶一致：一个查不到的主机名没有 upstream_id 可挂，
		// 让任何人往缓冲里写字符串还是一条放大路径。
		rec := New(Options{Registerer: prometheus.NewRegistry()})
		hooks := Hooks{Lookup: knownUpstreams("deb.debian.org")}
		get(newTestEngine(rec, hooks, &upstreamResponse{status: http.StatusNotFound}), "/evil.internal/x")

		convey.So(rec.DrainRecent(), convey.ShouldBeEmpty)
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

// gitResponse 假的 git 应答：拉取路径在 git 的响应上留下的是 X-Katch-Git。
func gitEngine(r *Recorder, hooks Hooks, source string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(r.Middleware(hooks))
	engine.NoRoute(func(c *gin.Context) {
		if source != "" {
			c.Header(GitSourceHeader, source)
		}
		c.Data(http.StatusOK, "application/x-git-upload-pack-advertisement", []byte("0000"))
	})
	return engine
}

// TestRecorder_GitPullsCarryTheirOwnDimension 目标 (e)：git 的拉取和静态对象分得开，
// 而且本地应答与穿透各自成一档（可观测性一节）。
//
// 没有这一维时，一次由本地镜像答完、一个字节都没问上游的 clone，在指标上和一次
// 打穿到上游的拉取长得一模一样——镜像到底有没有在干活就无从回答。
func TestRecorder_GitPullsCarryTheirOwnDimension(t *testing.T) {
	convey.Convey("git 的拉取带自己的类别与来源", t, func() {
		reg := prometheus.NewRegistry()
		rec := New(Options{Registerer: reg})
		hooks := Hooks{Lookup: knownUpstreams("git.example.com")}
		advertise := "/git.example.com/foo/bar/info/refs?service=git-upload-pack"

		get(gitEngine(rec, hooks, "local"), advertise)
		get(gitEngine(rec, hooks, "local"), advertise)
		get(gitEngine(rec, hooks, "passthrough"), advertise)

		body := scrape(reg)
		convey.So(body, convey.ShouldContainSubstring,
			`katch_requests_total{kind="git",result="local",upstream="git.example.com"} 2`)
		convey.So(body, convey.ShouldContainSubstring,
			`katch_requests_total{kind="git",result="passthrough",upstream="git.example.com"} 1`)
		convey.Convey("git 的拉取不再混在 static 里记成未命中", func() {
			convey.So(body, convey.ShouldNotContainSubstring,
				`kind="static",result="miss",upstream="git.example.com"`)
		})

		convey.Convey("界面那张表上，本地应答算命中、穿透算未命中", func() {
			// 分钟桶只有四种结果可落：本地应答确实是「没问上游」，
			// 穿透确实是「问了上游」，而未命中数是由总数减出来的，
			// 四个回源原因之和必须还等于它。
			var bucket Bucket
			for _, b := range rec.Drain() {
				if b.Host == "git.example.com" {
					bucket = b
				}
			}
			convey.So(bucket.Requests, convey.ShouldEqual, 3)
			convey.So(bucket.Hits, convey.ShouldEqual, 2)
			misses := bucket.Requests - bucket.Hits - bucket.Denied - bucket.OriginErrors
			convey.So(misses, convey.ShouldEqual, 1)
			convey.So(bucket.MissFirst+bucket.MissTTL+bucket.MissEvicted+bucket.MissChanged,
				convey.ShouldEqual, misses)
		})
	})
}

// TestRecorder_RejectedGitRequestsAreStillGit push 的 403 与没开协议的 404 同样是
// git 这一类，只是结果不同——类别说的是「这是哪种请求」，不是「它成功了没有」。
func TestRecorder_RejectedGitRequestsAreStillGit(t *testing.T) {
	convey.Convey("被拒的 git 请求也记在 git 这一类下", t, func() {
		reg := prometheus.NewRegistry()
		rec := New(Options{Registerer: reg})
		hooks := Hooks{Lookup: knownUpstreams("git.example.com")}

		engine := newTestEngine(rec, hooks, &upstreamResponse{status: http.StatusForbidden})
		get(engine, "/git.example.com/foo/bar/info/refs?service=git-receive-pack")

		convey.So(scrape(reg), convey.ShouldContainSubstring,
			`katch_requests_total{kind="git",result="denied",upstream="git.example.com"} 1`)
	})
}
