// Package metrics 是拉取路径的计数器：一次拉取结束后，它记下这次的结果、耗时和字节数。
//
// 三个出口共用同一批计数（决策 16）：
//
//   - Prometheus：`/metrics` 上的 katch_* 指标，供外部采集；
//   - 进程内分钟桶：由 stat_svc 每分钟取走一次落进 traffic_rollup，界面上的
//     请求量和命中率从那张表聚合——一个自部署的镜像站不该为了看自己的命中率
//     就要先搭一套监控；
//   - 进程内环形缓冲：由 request_svc 每秒取走一批落进 recent_request，
//     上游详情的「最近请求」从那张表读。热路径在这里只多一次内存写入，
//     不查库、不写库、不等待（记录与落库一节）。
//
// 回源原因（traffic_rollup 上的四列）只进分钟桶，不在 /metrics 上另开一族：
// 可观测性一节把要导出的 katch_* 列全了，而这四个数是给界面上那条占比条用的，
// spec 的数据模型把它们定在 traffic_rollup 的列上。
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
	"github.com/cago-frame/cago/pkg/logger"
	"github.com/cago-frame/cago/server/mux"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"

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
	// ResultLocal git 的这次拉取由本地镜像答完，一个字节都没问上游。
	//
	// 和 ResultHit 分开：命中说的是「这个对象在盘上」，而 git 的一次应答不是
	// 一个对象——它是按这个客户端手上有什么现打出来的（可观测性一节）。
	ResultLocal Result = "local"
	// ResultPassthrough git 的这次拉取穿透到了上游。
	ResultPassthrough Result = "passthrough"
)

// git 的应答上带的归因头，由拉取路径写、由这里读。
//
// 定义放在这里而不是拉取路径那一侧，同 MissHeader：读的人只有一个，而写的人
// 可能有好几处，把常量放在读的人身上，两边就不会各自漂。
const (
	// GitSourceHeader 这次 git 应答是谁答的。
	GitSourceHeader = "X-Katch-Git"
	// GitSourceLocal 本地镜像答的。
	GitSourceLocal = "local"
	// GitSourcePassthrough 穿透上游拿到的。
	GitSourcePassthrough = "passthrough"
)

const (
	// CacheStatusHeader 缓存层在响应上留下的命中标记。值是对外契约（用例与
	// make smoke 逐字比对），cache_svc 与这里的中间件共用这一份——两边各写一遍
	// 字符串，改一处就会让指标悄悄把命中记成回源（同 GitSourceHeader 的理由）。
	CacheStatusHeader = "X-Katch-Cache"
	CacheStatusHit    = "HIT"
	// CacheStatusRevalidated 过期副本经上游 304 续期：问过上游，所以记未命中；正文出自
	// 盘上那份，所以没有一个字节算作来自上游。
	CacheStatusRevalidated = "REVALIDATED"
	// CacheStatusMiss 本跳回源后交给客户端的应答。
	CacheStatusMiss = "MISS"
	// MissHeader 缓存层在未命中的响应上留下的归因。
	//
	// 和命中标记分成两个头而不是把原因拼进 X-Katch-Cache 的值里：那个值是已经
	// 发出去的约定（拉取路径的用例与 make smoke 都按 HIT / MISS 逐字比对），
	// 往里塞后缀等于改一份对外契约，而归因是新加的一维。
	MissHeader = "X-Katch-Miss"
	// unknownUpstream 未知主机共用的标签值。
	//
	// 不能把请求里的主机名原样当标签：拉取路径是公开的，任何人都能用一串没见过的
	// 主机名给每次探测造一个新的时间序列，那是一条不需要密码的内存放大路径。
	unknownUpstream = "unknown"
)

// MissReason 一次未命中是为什么发生的，对应 traffic_rollup 上的四列。
//
// 这四个是缓存层判出来的，中间件只负责转运：按响应反推不出「从来没缓存过」
// 和「缓存过但被淘汰了」的区别——两者在表上都只是「查不到记录」。
type MissReason string

const (
	// MissFirst 从来没缓存过。没留下归因的未命中也算这一档，见 missReason。
	MissFirst MissReason = "first"
	// MissTTL 可变对象的 TTL 过期了。
	MissTTL MissReason = "ttl"
	// MissEvicted 不可变对象被 LRU 淘汰了。
	MissEvicted MissReason = "evicted"
	// MissChanged 上游的 digest 和手上那份对不上。
	MissChanged MissReason = "changed"
)

// Event 一次拉取的记录。
type Event struct {
	// Upstream 上游主机名。空表示不在白名单里的主机。
	Upstream string
	// Kind 请求形态，registry、static 或 git，由 dispatch 判定。
	Kind   string
	Result Result
	// MissReason 未命中的归因，Result 不是 ResultMiss 时无意义。
	MissReason MissReason
	// BytesServed 发给客户端的字节数。
	BytesServed int64
	// BytesOrigin 其中来自上游的字节数。命中时为 0——界面上的「节省流量」
	// 就是这两者的差。
	BytesOrigin int64
	Duration    time.Duration
}

// RecentRequestCapacity 待落库环形缓冲的硬行数上限（决策 5）。
//
// 这是防「库变慢 → 内存涨」的那道闸，和保留期不是一回事：保留期管库里留多久，
// 这个上限管内存里最多攒多少。1 秒的窗口里只有库卡住时才可能被摸到。
const RecentRequestCapacity = 20000

// RecentRequestObjectLimit 一行里最多留多长的 object（字节）。
//
// 行数有上限不等于内存有上限：object 来自 URL，长度由调用方决定，不截的话
// 一行可以是一整条请求行。这个数和 recent_request.object 那一列的宽度
// （VARCHAR(512)）一致，两边一起改：超长的值在 MySQL 严格模式下会让整批插入
// 失败，把同一秒里其余的行一起带走。
const RecentRequestObjectLimit = 512

// RecentRequest 一次拉取的明细，环形缓冲里存的就是这个。
//
// 它和 Event 分开：Event 是计数需要的那些维度，这里多一个上游内的路径，
// 而且只有落库那一侧关心。
//
// Upstream 是主机名而不是 upstream_id：热路径上不碰库，翻 id 是落库那一侧的事。
type RecentRequest struct {
	Upstream string
	// At 这次拉取**结束**的时刻（秒）。
	At         int64
	Object     string
	Result     Result
	Bytes      int64
	DurationMS int64
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
	// 四个回源原因，加起来正好是未命中数（Requests 减去其余三项）。
	// 界面上的回源原因分解读的就是这四个数的占比。
	MissFirst   int64
	MissTTL     int64
	MissEvicted int64
	MissChanged int64
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

	// 回源侧：这三族记的是 katch 与上游之间那一跳，和上面按响应判定的拉取计数
	// 不是一回事——一次命中根本没有回源，一次回源也可能服务多个等待者。
	originRequests *prometheus.CounterVec
	originDuration *prometheus.HistogramVec
	originInflight *prometheus.GaugeVec

	// 缓存侧。objects/bytes 是快照式的 gauge，由统计层每分钟刷一次；
	// evictions/integrity 是发生即加的计数器。
	cacheObjects   *prometheus.GaugeVec
	cacheBytes     *prometheus.GaugeVec
	evictions      *prometheus.CounterVec
	integrityFails *prometheus.CounterVec

	ruleDecisions  *prometheus.CounterVec
	tokenExchanges *prometheus.CounterVec

	// mu 护住分钟桶。
	mu      sync.Mutex
	buckets map[bucketKey]*Bucket
	// recentMu 护住待落库的环形缓冲。
	//
	// 两个 mu 而不是一个：分钟桶一分钟才取走一次，而拉取每走一次都要 append，
	// 共用一个会让落库那一侧在取桶时把整条热路径堵住。
	recentMu   sync.Mutex
	recent     []RecentRequest
	recentHead int
	recentLen  int
	// usageMu 护住上一轮缓存占用快照，见 SetCacheUsage。
	usageMu   sync.Mutex
	lastUsage map[string]struct{}
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
		originRequests: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "katch_origin_requests_total",
			Help: "向上游发起的回源请求数，按上游与上游给出的状态码分。",
		}, []string{"upstream", "status"}),
		originDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "katch_origin_duration_seconds",
			Help:    "回源耗时，量到拿着响应头为止。",
			Buckets: prometheus.DefBuckets,
		}, []string{"upstream"}),
		originInflight: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: "katch_origin_inflight",
			Help: "此刻正压在上游那一侧的回源数。",
		}, []string{"upstream"}),
		cacheObjects: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: "katch_cache_objects",
			Help: "缓存里的对象数。",
		}, []string{"upstream"}),
		cacheBytes: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: "katch_cache_bytes",
			Help: "缓存占用的字节数。",
		}, []string{"upstream"}),
		evictions: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "katch_cache_evictions_total",
			Help: "被淘汰的缓存对象数，按淘汰原因分。",
		}, []string{"reason"}),
		integrityFails: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "katch_cache_integrity_failures_total",
			Help: "读缓存时校验失败的次数——磁盘或写入路径出了问题。",
		}, []string{"upstream"}),
		ruleDecisions: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "katch_rule_decisions_total",
			Help: "访问规则的判定数，按判定发生在哪一层与判定结果分。",
		}, []string{"scope", "decision"}),
		tokenExchanges: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "katch_token_exchanges_total",
			Help: "registry 上游的 token 交换次数。",
		}, []string{"upstream", "result"}),
		buckets:   map[bucketKey]*Bucket{},
		lastUsage: map[string]struct{}{},
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

// DrainRecent 取走并清空进程级的待落库明细。
func DrainRecent() []RecentRequest {
	return Default().DrainRecent()
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

// recordRecent 把这次拉取 append 进待落库的环形缓冲。
//
// 它和计数、拉取行共用同一个缝，因为三者要的是同一批事实，分开记迟早会出现
// 「指标上有、库里没有」的那一类对不上。热路径在这里只付一次内存写入：
// 主机名不翻 id、不查库、不写库、不等待（记录与落库一节）。
func (r *Recorder) recordRecent(object string, ev Event) {
	r.appendRecent(RecentRequest{
		Upstream:   ev.Upstream,
		At:         r.now().Unix(),
		Object:     clipObject(object),
		Result:     ev.Result,
		Bytes:      ev.BytesServed,
		DurationMS: ev.Duration.Milliseconds(),
	})
}

// clipObject 把上游内的路径截到缓冲/表那一列的宽度以内。
//
// EscapedPath 交出来的是转义过的 ASCII，按字节截不会切断一个字符。
func clipObject(object string) string {
	if len(object) <= RecentRequestObjectLimit {
		return object
	}
	return object[:RecentRequestObjectLimit]
}

// appendRecent 往环形缓冲里写一行，满了丢最旧。
func (r *Recorder) appendRecent(rec RecentRequest) {
	r.recentMu.Lock()
	defer r.recentMu.Unlock()
	if r.recent == nil {
		// 按上限一次开好：环形缓冲没有扩容，满了只能是丢最旧。
		r.recent = make([]RecentRequest, RecentRequestCapacity)
	}
	if r.recentLen < len(r.recent) {
		r.recent[(r.recentHead+r.recentLen)%len(r.recent)] = rec
		r.recentLen++
		return
	}
	// 满了丢最旧：指针往前挪一格，新的一行盖住它。
	r.recent[r.recentHead] = rec
	r.recentHead = (r.recentHead + 1) % len(r.recent)
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
	case ResultHit, ResultLocal:
		// 本地镜像答的那一次没问上游，界面上那条命中率里它就是一次命中。
		b.Hits++
	case ResultPassthrough:
		// 穿透落在未命中那一档（未命中数由总数减其余三项得出）。归因记「首次」：
		// git 的应答从不进对象缓存（决策 9），另外三个原因一个都不成立，而四项
		// 之和必须仍然等于未命中数，否则占比条会凭空少掉一块。
		b.MissFirst++
	case ResultDenied:
		b.Denied++
	case ResultOriginError:
		b.OriginErrors++
	case ResultMiss:
		switch ev.MissReason {
		case MissTTL:
			b.MissTTL++
		case MissEvicted:
			b.MissEvicted++
		case MissChanged:
			b.MissChanged++
		case MissFirst:
			b.MissFirst++
		default:
			// 归因不认得就算首次拉取，不能不记：四项之和必须等于未命中数，
			// 少掉的那一块在占比条上看不出来，只会让别的几项显得比实际大。
			b.MissFirst++
		}
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

// DrainRecent 取走并清空待落库的明细，由旧到新。
//
// 取走即清空：落库那一侧每轮拿到的只是「上一轮之后新增的行」，
// 不清零会让同一批被反复写进去。失败时这一批就没了（决策 9）——
// 一个不可用的库不该顺带把内存变成一个需要自己设上限的重试队列。
func (r *Recorder) DrainRecent() []RecentRequest {
	r.recentMu.Lock()
	defer r.recentMu.Unlock()
	if r.recentLen == 0 {
		return nil
	}
	out := make([]RecentRequest, 0, r.recentLen)
	for i := 0; i < r.recentLen; i++ {
		idx := (r.recentHead + i) % len(r.recent)
		out = append(out, r.recent[idx])
		// 清掉槽位：取走的行不该因为槽位还被占着而挂在环形数组上。
		r.recent[idx] = RecentRequest{}
	}
	r.recentHead, r.recentLen = 0, 0
	return out
}

// PullLogMessage 每次拉取写进结构化日志的那一行的 msg。
//
// 这一行是 debug 级别（决策 1）：默认配置下不写，排障时把级别调到 debug 才恢复。
const PullLogMessage = "拉取"

// Middleware 构造拉取路径的计数中间件。
func (r *Recorder) Middleware(hooks Hooks) gin.HandlerFunc {
	return func(c *gin.Context) {
		kind, host, object := dispatch.Classify(c.Request.URL.EscapedPath())
		if hooks.Lookup == nil || !dispatch.IsPull(kind) {
			// 界面、静态产物和 katch 自身的端点不是拉取，计进来只会把命中率冲淡。
			c.Next()
			return
		}
		known := hooks.Lookup(c.Request.Context(), host)
		start := r.now()
		c.Next()
		ev := Event{
			// git 的端点寄生在 static 的路径空间里，按路径形态认（决策 3）。
			// 类别说的是「这是哪种请求」，成没成功是结果那一维的事，所以被 403
			// 掉的 push 和 404 掉的主机同样记在 git 这一类下。
			Kind:     kindLabel(kind, dispatch.ClassifyGit(object, c.Request.URL.RawQuery)),
			Duration: r.now().Sub(start),
		}
		if !known {
			// 白名单之外的主机：结果就是拒绝，状态码不必再看一遍。
			ev.Result = ResultDenied
			r.RecordRequest(ev)
			return
		}
		ev.Upstream = host
		ev.Result = classify(c.Writer.Status(), c.Writer.Header().Get(CacheStatusHeader))
		// git 的应答自己说它是谁答的，不必从状态码上猜：本地应答与穿透的状态码
		// 一模一样，差别只在这个头上。
		ev.Result = gitResult(c.Writer.Header().Get(GitSourceHeader), ev.Result)
		if ev.Result == ResultMiss {
			ev.MissReason = missReason(c.Writer.Header().Get(MissHeader))
		}
		if size := int64(c.Writer.Size()); size > 0 {
			ev.BytesServed = size
			revalidated := c.Writer.Header().Get(CacheStatusHeader) == CacheStatusRevalidated
			if (ev.Result == ResultMiss && !revalidated) || ev.Result == ResultPassthrough {
				ev.BytesOrigin = size
			}
		}
		r.RecordRequest(ev)
		r.recordRecent(object, ev)
		r.logPull(c.Request.Context(), object, ev)
		r.feedGate(hooks.Gate, host, ev.Result)
	}
}

// logPull 把这次拉取写成结构化日志的一行。
//
// 它和计数共用这一个缝，而不是另起一处埋点：两者要的是同一批事实，分开记迟早会
// 出现「指标上有、日志里没有」的那一类对不上。这行本身没有消费者——「最近请求」
// 面板改读 recent_request 那张表了（决策 2/11），它只留给排障时按级别取用。
//
// debug 而不是 info（决策 1）：这一行唯一的消费者是「最近请求」面板，面板改读
// 库之后它没有消费者；默认级别下把这次写入整个摘掉（zap 在 level 检查处返回），
// 排障时把级别调到 debug 仍能拿回同一行、同样的六个字段。
//
// 只记上游表里有的主机：拉取路径是公开的，把没见过的主机名也写进去，等于让任何
// 人都能往这台机器的磁盘上写字符串。它们的计数仍然照记，只是折在 unknown 上。
func (r *Recorder) logPull(ctx context.Context, object string, ev Event) {
	if ev.Upstream == "" {
		return
	}
	logger.Ctx(ctx).Debug(PullLogMessage,
		// at 是这次拉取**结束**的时刻：一次拉取的结果要到这时才成立，
		// recent_request 的 at 列用的是同一个时刻，面板也按它从新到旧排。
		zap.Int64("at", r.now().Unix()),
		zap.String("upstream", ev.Upstream),
		zap.String("object", object),
		zap.String("result", string(ev.Result)),
		zap.Int64("bytes", ev.BytesServed),
		zap.Int64("duration_ms", ev.Duration.Milliseconds()),
	)
}

// feedGate 把这次回源的成败喂给退避，并把降级状态标到指标上。
func (r *Recorder) feedGate(gate Gate, host string, result Result) {
	if gate == nil {
		return
	}
	switch result {
	case ResultOriginError:
		gate.Failure(host)
	case ResultMiss, ResultPassthrough:
		// 只有真的回源成功才算「上游还活着」。命中根本没碰上游，拿它当恢复的
		// 证据，会让一个还在挂的上游刚出退避就被全量打回去。git 的穿透是实打实
		// 打到了上游的那一种，和一次回源同等看待。
		gate.Success(host)
	case ResultHit, ResultLocal, ResultDenied:
		return
	}
	r.SetBackoff(host, gate.Degraded(host))
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
	case cacheStatus == CacheStatusHit:
		return ResultHit
	default:
		return ResultMiss
	}
}

// missReason 认缓存层留下的归因，不认得的一律当首次拉取。
//
// 没有归因的未命中确实存在：HEAD、Range 这类请求根本没问过缓存，查不到上游的
// 请求也不会走到缓存层。它们手上没有任何一份可复用的副本，算进首次拉取是这四
// 档里唯一说得通的一档，而「不记」会让占比之和不再是 100%。
func missReason(value string) MissReason {
	switch MissReason(value) {
	case MissTTL:
		return MissTTL
	case MissEvicted:
		return MissEvicted
	case MissChanged:
		return MissChanged
	case MissFirst:
		return MissFirst
	default:
		return MissFirst
	}
}

// gitResult 认 git 应答上的归因头，不是 git 的应答就保持原来那个结果。
func gitResult(source string, fallback Result) Result {
	switch source {
	case GitSourceLocal:
		return ResultLocal
	case GitSourcePassthrough:
		return ResultPassthrough
	default:
		return fallback
	}
}

func kindLabel(kind dispatch.Kind, git dispatch.GitEndpoint) string {
	switch {
	case kind == dispatch.KindRegistry:
		return "registry"
	// 只有 static 认 git，和服务侧同一条判据（web.serveProxy）：git 的端点只寄生
	// 在 static 的路径空间里。放开到别的 Kind，任何人都能拼一条 /sumdb/... 的
	// 路径，让它在服务侧按 sumdb 走、却在这里记进 git 那一维。
	case kind == dispatch.KindStatic && git.IsGit():
		return "git"
	default:
		return "static"
	}
}
