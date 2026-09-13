package metrics

import (
	"strconv"
	"time"
)

// 可观测性一节点名的指标里，有几族的事实不在拉取路径最外层那个中间件上：
// 回源的成败只有回源那一层知道，淘汰原因只有缓存层知道，判定落在哪一层只有规则
// 求值知道，token 交换只有 registry 适配器知道。中间件按响应反推不出这些——
// 一个 200 既可能是命中、也可能是回源一次就成、还可能是回源重试三次才成。
//
// 所以这些族由各自那条缝主动喂进来，而指标本身仍然只在这一个包里注册：
// 注册散开之后，「/metrics 上到底有哪些 katch_* 」就没有一个地方答得上来了。

// 淘汰原因，对应 katch_cache_evictions_total 的 reason 标签。
const (
	// EvictionLRU 超配额后按最近最少使用淘汰。
	EvictionLRU = "lru"
	// EvictionTTL 可变对象到期被清走。
	EvictionTTL = "ttl"
	// EvictionManual 人在界面上清掉的。
	EvictionManual = "manual"
)

// token 交换的结果，对应 katch_token_exchanges_total 的 result 标签。
const (
	// TokenSuccess 换到了 token。
	TokenSuccess = "success"
	// TokenFailure 没换到——上游拒绝、超时或响应读不懂。
	TokenFailure = "failure"
)

// CacheUsage 一个上游此刻在缓存里占了多少。
type CacheUsage struct {
	Upstream string
	Objects  int64
	Bytes    int64
}

// OriginStarted 记一次回源开始，返回的闭包在回源结束时调用。
//
// 返回闭包而不是让调用方自己配一次减法：名额与在途数都活到响应体被关闭为止
// （见 proxy_svc 的 guardedBody），配平的责任交给调用方迟早会漏掉一条早退的分支，
// 而一个只增不减的 gauge 比没有这个 gauge 更误导人。
func (r *Recorder) OriginStarted(upstream string) func() {
	if upstream == "" {
		upstream = unknownUpstream
	}
	r.originInflight.WithLabelValues(upstream).Inc()
	var once bool
	return func() {
		if once {
			return
		}
		once = true
		r.originInflight.WithLabelValues(upstream).Dec()
	}
}

// RecordOrigin 记一次拿到了响应的回源。status 是上游给出的状态码。
//
// 连不上、超时这类拿不到响应的情况传 0：它们同样是一次真实发生的回源，
// 不记的话「回源了多少次」会在上游挂掉时凭空变小，正好在最需要这个数的时候。
func (r *Recorder) RecordOrigin(upstream string, status int, d time.Duration) {
	if upstream == "" {
		upstream = unknownUpstream
	}
	r.originRequests.WithLabelValues(upstream, strconv.Itoa(status)).Inc()
	r.originDuration.WithLabelValues(upstream).Observe(d.Seconds())
}

// SetBackoff 标记一个上游此刻是否处于回源退避。
func (r *Recorder) SetBackoff(upstream string, degraded bool) {
	value := float64(0)
	if degraded {
		value = 1
	}
	r.backoff.WithLabelValues(upstream).Set(value)
}

// SetCacheUsage 用一份快照覆盖缓存占用。
//
// 快照而不是增量：对象数和字节数是「此刻有多少」，由统计层每分钟从库里查一次
// 整体算出来，跟着每次写入去加减会在重启、手工清盘之后和真实占用越差越远。
//
// 这一轮没出现的上游归零而不是留着上一轮的数：一个刚被清空缓存的上游若留着
// 旧数字，界面和采集方都会一直看到一份早已不存在的占用。归零而不是删掉序列，
// 是因为「这个上游现在占 0」本身就是要看的事实，删了会在图上变成断点。
func (r *Recorder) SetCacheUsage(usage []CacheUsage) {
	seen := make(map[string]struct{}, len(usage))
	for _, u := range usage {
		if u.Upstream == "" {
			continue
		}
		r.cacheObjects.WithLabelValues(u.Upstream).Set(float64(u.Objects))
		r.cacheBytes.WithLabelValues(u.Upstream).Set(float64(u.Bytes))
		seen[u.Upstream] = struct{}{}
	}
	r.usageMu.Lock()
	defer r.usageMu.Unlock()
	for host := range r.lastUsage {
		if _, ok := seen[host]; ok {
			continue
		}
		r.cacheObjects.WithLabelValues(host).Set(0)
		r.cacheBytes.WithLabelValues(host).Set(0)
	}
	r.lastUsage = seen
}

// RecordEviction 记 n 个对象被按 reason 淘汰。
//
// 按对象数而不是按轮次：一轮回收可能带走几百个对象，记成 1 会让「削掉了多少」
// 这个问题没法从这个指标回答。
func (r *Recorder) RecordEviction(reason string, n int64) {
	if n <= 0 {
		return
	}
	r.evictions.WithLabelValues(reason).Add(float64(n))
}

// RecordIntegrityFailure 记一次缓存副本校验失败。
func (r *Recorder) RecordIntegrityFailure(upstream string) {
	if upstream == "" {
		upstream = unknownUpstream
	}
	r.integrityFails.WithLabelValues(upstream).Inc()
}

// RecordRuleDecision 记一次访问规则判定。scope 是 global / upstream / default，
// decision 是 allow / deny。
//
// 不带上游标签：这一族要答的是「策略整体在放行还是在拦」，按上游铺开会把
// 每个上游的每一次拉取都变成一条时间序列，而那个维度 katch_requests_total
// 的 denied 已经给了。
func (r *Recorder) RecordRuleDecision(scope, decision string) {
	r.ruleDecisions.WithLabelValues(scope, decision).Inc()
}

// RecordTokenExchange 记一次 registry 的 token 交换。
func (r *Recorder) RecordTokenExchange(upstream, result string) {
	if upstream == "" {
		upstream = unknownUpstream
	}
	r.tokenExchanges.WithLabelValues(upstream, result).Inc()
}

// 下面几个是进程级计数器上的同名快捷方式，供不方便拿到 Recorder 的调用方用
// （拉取路径上的缓存层、规则闸与 registry 适配器都是这种情况）。

// OriginStarted 记一次回源开始，走进程级计数器。
func OriginStarted(upstream string) func() { return Default().OriginStarted(upstream) }

// RecordOrigin 记一次回源，走进程级计数器。
func RecordOrigin(upstream string, status int, d time.Duration) {
	Default().RecordOrigin(upstream, status, d)
}

// SetCacheUsage 刷新缓存占用，走进程级计数器。
func SetCacheUsage(usage []CacheUsage) { Default().SetCacheUsage(usage) }

// RecordEviction 记淘汰，走进程级计数器。
func RecordEviction(reason string, n int64) { Default().RecordEviction(reason, n) }

// RecordIntegrityFailure 记一次校验失败，走进程级计数器。
func RecordIntegrityFailure(upstream string) { Default().RecordIntegrityFailure(upstream) }

// RecordRuleDecision 记一次规则判定，走进程级计数器。
func RecordRuleDecision(scope, decision string) { Default().RecordRuleDecision(scope, decision) }

// RecordTokenExchange 记一次 token 交换，走进程级计数器。
func RecordTokenExchange(upstream, result string) { Default().RecordTokenExchange(upstream, result) }
