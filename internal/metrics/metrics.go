// Package metrics 是拉取路径的计数器：一次拉取结束后，它记下这次的结果、耗时和字节数。
//
// 两个出口共用同一批计数（决策 16）：
//
//   - Prometheus：`/metrics` 上的 katch_* 指标，供外部采集；
//   - 进程内分钟桶：由 stat_svc 每分钟取走一次落进 traffic_rollup，界面上的
//     请求量和命中率从那张表聚合——一个自部署的镜像站不该为了看自己的命中率
//     就要先搭一套监控。
//
// 它挂在拉取路径的最外层（和 SPA 共用的 NoRoute 之前），按响应本身判定结果：
// 状态码加 cache_svc 留下的 X-Katch-Cache。这样计数只有一个出处，而不必在
// 缓存层和代理层各插一次埋点。
package metrics

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/cago-frame/cago"
	"github.com/cago-frame/cago/configs"
	"github.com/cago-frame/cago/server/mux"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/CodFrm/katch/internal/proxy/dispatch"
)

// Result 一次拉取的结果。
type Result string

const (
	// ResultHit 由缓存服务。
	ResultHit Result = "hit"
	// ResultMiss 回源取回（含上游的 4xx/5xx 透传）。
	ResultMiss Result = "miss"
	// ResultDenied 被白名单或访问规则挡住。
	ResultDenied Result = "denied"
	// ResultOriginError 上游不可达或超时。
	ResultOriginError Result = "origin_error"
)

const (
	// cacheStatusHeader 缓存层在响应上留下的命中标记。
	cacheStatusHeader = "X-Katch-Cache"
	cacheStatusHit    = "HIT"
	// unknownUpstream 未知主机共用的标签值。
	//
	// 不能把请求里的主机名原样当标签：拉取路径是公开的，任何人都能用一串没见过的
	// 主机名给每次探测造一个新的时间序列，那是一条不需要密码的内存放大路径。
	unknownUpstream = "unknown"
)

// Event 一次拉取的记录。
type Event struct {
	// Upstream 上游主机名。空表示不在白名单里的主机。
	Upstream string
	// Kind 请求形态，registry 或 static，由 dispatch 判定。
	Kind   string
	Result Result
	// BytesServed 发给客户端的字节数。
	BytesServed int64
	// BytesOrigin 其中来自上游的字节数。命中时为 0——界面上的「节省流量」
	// 就是这两者的差。
	BytesOrigin int64
	Duration    time.Duration
}

// Bucket 一个上游在某一整分钟内的累计量，与 traffic_rollup 的列一一对应。
type Bucket struct {
	Host   string
	Bucket int64
	// Requests 总请求数。miss 数由 Requests 减去其余三项推出，不单独存一列。
	Requests     int64
	Hits         int64
	Denied       int64
	OriginErrors int64
	BytesServed  int64
	BytesOrigin  int64
}

// Gate 退避状态。中间件只喂给它回源的成败，并把降级标到指标上；
// 「这次要不要真的打到上游」由回源那一层问它。
type Gate interface {
	Failure(host string)
	Success(host string)
	Degraded(host string) bool
}

// Lookup 回答「这个主机名此刻是不是一个可服务的上游」。
//
// 中间件不自己查上游表：白名单那道闸只有一个出处（proxy_svc），这里要的只是
// 「该不该给它一个独立的标签值」这一个判断，由 main 用 upstream_svc 接上。
type Lookup func(ctx context.Context, host string) bool

// Hooks 中间件的外部依赖。
//
// Gate 可以为 nil（不反馈退避）；Lookup 为 nil 时中间件什么都不记——
// 认不出哪个主机是上游，就只剩下「把请求里的主机名原样当标签」和「全部记成
// 拒绝」两条路，前者是内存放大，后者是一整份错数，不如不记。
type Hooks struct {
	Gate   Gate
	Lookup Lookup
}

// Options 构造参数。
type Options struct {
	// Registerer 指标注册到哪里。nil 表示 prometheus 的默认 registry——
	// component.Core() 暴露的 /metrics 就是从那里收集的，另起一个 registry
	// 的指标在那个端点上根本看不见。
	Registerer prometheus.Registerer
	// Now 取当前时间，用例注入假时钟用。
	Now func() time.Time
}

// Recorder 拉取路径的计数器。
type Recorder struct {
	now func() time.Time

	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	bytes    *prometheus.CounterVec
	backoff  *prometheus.GaugeVec

	// mu 护住分钟桶。
	mu      sync.Mutex
	buckets map[bucketKey]*Bucket
}

type bucketKey struct {
	host   string
	bucket int64
}

// New 构造计数器并把指标注册上去。
func New(opt Options) *Recorder {
	if opt.Registerer == nil {
		opt.Registerer = prometheus.DefaultRegisterer
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	factory := promauto.With(opt.Registerer)
	return &Recorder{
		now: opt.Now,
		requests: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "katch_requests_total",
			Help: "拉取请求数，按上游、请求形态与结果分。",
		}, []string{"upstream", "kind", "result"}),
		duration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "katch_request_duration_seconds",
			Help:    "拉取请求耗时。",
			Buckets: prometheus.DefBuckets,
		}, []string{"upstream", "kind"}),
		bytes: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "katch_bytes_served_total",
			Help: "发给客户端的字节数，按来自缓存还是上游分。",
		}, []string{"upstream", "source"}),
		backoff: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: "katch_origin_backoff",
			Help: "上游是否处于回源退避（降级）状态。",
		}, []string{"upstream"}),
		buckets: map[bucketKey]*Bucket{},
	}
}

// 不导出命中率这类比值：它由 hit / (hit + miss) 推导，导出成 gauge 只会在采样
// 窗口不一致时给出两个互相矛盾的数字（可观测性一节）。界面上的命中率同样是推导值。

var (
	defaultOnce     sync.Once
	defaultRecorder *Recorder
)

// Default 返回进程级的计数器，注册在 prometheus 的默认 registry 上。
//
// 用 once 而不是由 main 注入：同名指标注册两次会 panic，而 Default 可能先被
// 装配之外的调用方碰到。
func Default() *Recorder {
	defaultOnce.Do(func() {
		defaultRecorder = New(Options{})
	})
	return defaultRecorder
}

// RecordRequest 记一次拉取，走进程级计数器。
func RecordRequest(ev Event) {
	Default().RecordRequest(ev)
}

// Drain 取走并清空进程级的分钟桶。
func Drain() []Bucket {
	return Default().Drain()
}

// Mount 把进程级计数器的中间件挂到 gin 上，由 main 作为组件注册。
//
// 和 web.MountSPA 一样走 mux.RegisterMiddleware：中间件必须在 mux.HTTP 启动
// 之前注册完。它排在 NoRoute 之前，所以拉取路径上的每一次响应都经过它——
// gin 的 Use 会顺带重建 NoRoute 的处理链，两者的注册先后无所谓。
func Mount(hooks Hooks) cago.FuncComponent {
	return func(_ context.Context, _ *configs.Config) error {
		mux.RegisterMiddleware(func(_ *configs.Config, engine *gin.Engine) error {
			engine.Use(Default().Middleware(hooks))
			return nil
		})
		return nil
	}
}

// RecordRequest 记一次拉取。
func (r *Recorder) RecordRequest(ev Event) {
	upstream := ev.Upstream
	if upstream == "" {
		upstream = unknownUpstream
	}
	r.requests.WithLabelValues(upstream, ev.Kind, string(ev.Result)).Inc()
	r.duration.WithLabelValues(upstream, ev.Kind).Observe(ev.Duration.Seconds())
	if ev.BytesServed > 0 {
		source := "cache"
		if ev.BytesOrigin > 0 {
			source = "origin"
		}
		r.bytes.WithLabelValues(upstream, source).Add(float64(ev.BytesServed))
	}
	if ev.Upstream == "" {
		// 未知主机不进 rollup：那张表按上游 id 存，一个查不到的主机名没有 id
		// 可挂，留 90 天也没人会去看。
		return
	}
	r.addBucket(ev)
}

func (r *Recorder) addBucket(ev Event) {
	key := bucketKey{host: ev.Upstream, bucket: r.now().Truncate(time.Minute).Unix()}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.buckets[key]
	if !ok {
		b = &Bucket{Host: key.host, Bucket: key.bucket}
		r.buckets[key] = b
	}
	b.Requests++
	switch ev.Result {
	case ResultHit:
		b.Hits++
	case ResultDenied:
		b.Denied++
	case ResultOriginError:
		b.OriginErrors++
	case ResultMiss:
	}
	b.BytesServed += ev.BytesServed
	b.BytesOrigin += ev.BytesOrigin
}

// Drain 取走并清空分钟桶。
//
// 取走即清零：桶是「这一段时间新增了多少」，落库那一侧按桶累加，
// 不清零会让同一分钟的量被反复写进去。
func (r *Recorder) Drain() []Bucket {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := make([]Bucket, 0, len(r.buckets))
	for _, b := range r.buckets {
		list = append(list, *b)
	}
	r.buckets = map[bucketKey]*Bucket{}
	return list
}

// Middleware 构造拉取路径的计数中间件。
func (r *Recorder) Middleware(hooks Hooks) gin.HandlerFunc {
	return func(c *gin.Context) {
		kind, host, _ := dispatch.Classify(c.Request.URL.EscapedPath())
		if hooks.Lookup == nil || (kind != dispatch.KindRegistry && kind != dispatch.KindStatic) {
			// 界面、静态产物和 katch 自身的端点不是拉取，计进来只会把命中率冲淡。
			c.Next()
			return
		}
		known := hooks.Lookup(c.Request.Context(), host)
		start := r.now()
		c.Next()
		ev := Event{
			Kind:     kindLabel(kind),
			Duration: r.now().Sub(start),
		}
		if !known {
			// 白名单之外的主机：结果就是拒绝，状态码不必再看一遍。
			ev.Result = ResultDenied
			r.RecordRequest(ev)
			return
		}
		ev.Upstream = host
		ev.Result = classify(c.Writer.Status(), c.Writer.Header().Get(cacheStatusHeader))
		if size := int64(c.Writer.Size()); size > 0 {
			ev.BytesServed = size
			if ev.Result == ResultMiss {
				ev.BytesOrigin = size
			}
		}
		r.RecordRequest(ev)
		r.feedGate(hooks.Gate, host, ev.Result)
	}
}

// feedGate 把这次回源的成败喂给退避，并把降级状态标到指标上。
func (r *Recorder) feedGate(gate Gate, host string, result Result) {
	if gate == nil {
		return
	}
	switch result {
	case ResultOriginError:
		gate.Failure(host)
	case ResultMiss:
		// 只有真的回源成功才算「上游还活着」。命中根本没碰上游，拿它当恢复的
		// 证据，会让一个还在挂的上游刚出退避就被全量打回去。
		gate.Success(host)
	case ResultHit, ResultDenied:
		return
	}
	degraded := float64(0)
	if gate.Degraded(host) {
		degraded = 1
	}
	r.backoff.WithLabelValues(host).Set(degraded)
}

// classify 按响应判定这次拉取的结果。
//
//   - 502 是回源失败（embed.go 对拿不到响应的情况就发这个）；
//   - 403 是访问规则拒绝；
//   - 带 HIT 标记的是缓存命中；
//   - 其余都算回源取回，包括上游自己的 404/500——那是上游对这个对象的判断，
//     不是 katch 拒绝了谁。
func classify(status int, cacheStatus string) Result {
	switch {
	case status == http.StatusBadGateway:
		return ResultOriginError
	case status == http.StatusForbidden:
		return ResultDenied
	case cacheStatus == cacheStatusHit:
		return ResultHit
	default:
		return ResultMiss
	}
}

func kindLabel(kind dispatch.Kind) string {
	if kind == dispatch.KindRegistry {
		return "registry"
	}
	return "static"
}
