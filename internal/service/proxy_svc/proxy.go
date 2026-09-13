// Package proxy_svc 是拉取路径的业务层：把一个已经分好段的请求变成一次回源。
//
// 它站在白名单（决策 6）这道闸上——不在上游表里、已停用、协议类别对不上的请求
// 在这里就被挡住，一律返回同一个 ErrUpstreamNotAllowed，不透露任何差别。
package proxy_svc

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/cago-frame/cago/pkg/logger"
	"go.uber.org/zap"

	"github.com/CodFrm/katch/internal/metrics"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/dispatch"
	"github.com/CodFrm/katch/internal/proxy/origin"
	"github.com/CodFrm/katch/internal/proxy/registry"
	"github.com/CodFrm/katch/internal/service/setting_svc"
	"github.com/CodFrm/katch/internal/service/upstream_svc"
)

// ErrUpstreamNotAllowed 这个请求没有对应的可用上游。
//
// 「表里没有」「已停用」「协议类别对不上」共用这一个错误，且错误信息里不含主机名：
// 三者之间任何可观察的差别，都会把 katch 变成一个探测内网主机是否存在的工具。
var ErrUpstreamNotAllowed = errors.New("没有可用的上游")

// ErrUpstreamBackoff 上游正在退避窗口里，这次请求不回源，直接失败。
//
// 和 ErrUpstreamNotAllowed 分开：那个说的是「表里没有这个上游」，对外必须是
// 一律 404 且不透露任何差别；这个说的是「有，但它此刻打不通」，对外是 502——
// 客户端据此把它当成一次暂时的失败，而不是「这个对象不存在」这个结论。
var ErrUpstreamBackoff = errors.New("上游正在退避中")

// Target 一次已经分好段的拉取请求。
type Target struct {
	// Kind 由 dispatch.Classify 给出，决定 registry 的 /v2 协议前缀要不要补回去。
	Kind dispatch.Kind
	// Host 上游主机名，查上游表的 key。
	Host string
	// Path 上游侧路径，转义形态、以 / 开头，不含 /v2 协议前缀。
	Path     string
	RawQuery string
	Method   string
	// Header 客户端请求头，由回源侧按白名单过滤后转发。
	Header http.Header
}

// Meta 回源响应的元信息。响应体单独返回，便于流式转发。
type Meta struct {
	StatusCode    int
	Header        http.Header
	ContentLength int64
}

// ProxySvc 拉取路径的业务操作。
type ProxySvc interface {
	// Fetch 回源取一个对象。上游的 4xx/5xx 是正常返回值（原样透传），
	// 只有连不上、超时这类拿不到响应的情况才返回 error。
	Fetch(ctx context.Context, target *Target) (io.ReadCloser, *Meta, error)
}

// Gate 回源之前问一句「这次要不要真的打到上游」。由 internal/proxy/backoff 实现。
//
// 只有回源这一步问它：决策 17 把退避限定在回源失败上，缓存命中不花上游任何
// 成本，闸若装在缓存前面，一次上游抖动会把盘上已有的副本一起变成失败。
type Gate interface {
	Allow(host string) bool
}

// Options 构造参数。
type Options struct {
	// Gate 退避闸。nil 表示不退避，一律放行。
	Gate Gate
	// Runtime 运行时设置的来源，nil 表示进程级的那一个（setting_svc）。
	//
	// 回源并发上限、上游超时与重试次数都从这里现读，不在构造时抄成字段：
	// 它们是 setting 表里的运行时项，改完必须在下一次回源上就生效（决策 3/4）。
	Runtime setting_svc.RuntimeSource
	// Metrics 回源侧指标的去处，nil 表示进程级那一个。
	//
	// 可注入是为了让用例各自拿一个干净的 registry：指标是进程级单例，
	// 用例之间共用会让「这次回源记了几条」取决于前面跑过哪些用例。
	Metrics *metrics.Recorder
}

type proxySvc struct {
	origin *origin.Client
	// registry registry 上游的适配器：token 交换与 /v2、library/ 的路径补全。
	// 它坐在这个缝的后面，而不是另开一条数据通路——白名单、退避、缓存都在
	// 上面那几行里，绕过去就等于绕过它们。
	registry *registry.Adapter
	gate     Gate
	// runtime 并发上限、超时与重试次数的来源，每次回源现读。
	runtime setting_svc.RuntimeSource
	// slots 回源并发闸，进程内唯一一份：上限是「这台 katch 同时压给上游多少个
	// 请求」，按 service 实例各算各的就限不住。
	slots originSlots
	// recorder 回源侧指标的去处。
	recorder *metrics.Recorder
}

// metrics 取指标去处。延迟到调用时才取进程级那一个：包初始化时就去碰全局
// registry 会让「导入这个包」变成一次注册指标的副作用。
func (p *proxySvc) metrics() *metrics.Recorder {
	if p.recorder != nil {
		return p.recorder
	}
	return metrics.Default()
}

// New 构造拉取路径的业务层。
func New(opt Options) ProxySvc {
	client := origin.New()
	if opt.Runtime == nil {
		opt.Runtime = setting_svc.Setting()
	}
	return &proxySvc{
		origin:   client,
		registry: registry.New(registry.Options{Origin: client, Metrics: opt.Metrics}),
		gate:     opt.Gate,
		runtime:  opt.Runtime,
		recorder: opt.Metrics,
	}
}

var defaultProxy = New(Options{})

// Proxy 返回拉取路径的业务层。
func Proxy() ProxySvc {
	return defaultProxy
}

// Register 注册实现，由 main 装配（把退避闸接上）、由测试注入。
func Register(svc ProxySvc) {
	defaultProxy = svc
}

func (p *proxySvc) Fetch(ctx context.Context, target *Target) (io.ReadCloser, *Meta, error) {
	upstream, err := upstream_svc.Upstream().FindByHost(ctx, target.Host)
	if err != nil {
		return nil, nil, err
	}
	// FindByHost 已经把「已停用」折叠成「不存在」，这里不必也不该再看一遍 Enabled。
	if upstream == nil {
		return nil, nil, ErrUpstreamNotAllowed
	}
	if !kindMatches(target, upstream) {
		return nil, nil, ErrUpstreamNotAllowed
	}
	// 闸问在这里，而不是在白名单判定之前：表里没有的主机名必须先拿到那一个
	// 统一的 ErrUpstreamNotAllowed，否则「退避中」和「不存在」两种回应之间的
	// 差别，就成了探测内网主机是否存在的信号（决策 6）。
	if p.gate != nil && !p.gate.Allow(target.Host) {
		return nil, nil, ErrUpstreamBackoff
	}
	// 并发上限、超时与重试都在这里现读：站长在设置页改完，下一次回源就按新值走。
	rt := p.settings(ctx)
	release, err := p.slots.acquire(ctx, rt.OriginConcurrency)
	if err != nil {
		return nil, nil, err
	}
	// 在途数从这里一直记到响应体被关闭：它量的是「此刻压在上游那一侧多少个
	// 请求」，拿到响应头就减掉等于把一次大对象的下载当成已经结束。
	inflightDone := p.metrics().OriginStarted(target.Host)
	resp, stop, err := p.fetchWithRetry(ctx, target, upstream, rt)
	if err != nil {
		inflightDone()
		release()
		return nil, nil, err
	}
	// 名额、在途数与超时用的 context 一直留到响应体被关闭，见 guardedBody。
	body := &guardedBody{ReadCloser: resp.Body, done: func() {
		stop()
		inflightDone()
		release()
	}}
	return body, &Meta{
		StatusCode:    resp.StatusCode,
		Header:        resp.Header,
		ContentLength: resp.ContentLength,
	}, nil
}

// settings 读一次运行时设置。
//
// 读不出来不让拉取停摆：返回的快照在出错时是出厂值（失败与降级一节——库不可用时
// 读路径要能继续服务）。
func (p *proxySvc) settings(ctx context.Context) *setting_svc.RuntimeSettings {
	rt, err := p.runtime.Runtime(ctx)
	if err != nil {
		logger.Ctx(ctx).Error("读取运行时设置失败，本次回源按默认值处理", zap.Error(err))
	}
	return rt
}

// fetchWithRetry 回源，失败按设置里的次数重试；返回的第二个值要在响应体关闭时调用。
//
// 只有「拿不到响应」才重试：上游的 4xx/5xx 是一个要原样透传的答复，重试它等于把
// 上游明确给出的结论当成噪声，还会把一次 404 放大成 N 次回源。
//
// 超时不下沉到 http.Client：那是一个覆盖整次请求（含响应体读取）的总时限，几百 MB
// 的镜像层会稳定地在读到一半时被它掐断。这里用一个只跑到「拿到响应头」为止的计时器，
// 拿到响应就把它停掉，随后的响应体读取不再受它管。计时器装在这一层而不是 origin 里，
// 是因为 registry 上游要经适配器绕一趟 token 交换，装在这里两条回源方式才都被盖住。
func (p *proxySvc) fetchWithRetry(ctx context.Context, target *Target,
	upstream *upstream_entity.Upstream, rt *setting_svc.RuntimeSettings,
) (*origin.Response, context.CancelFunc, error) {
	timeout := time.Duration(rt.OriginTimeoutSeconds) * time.Second
	// 重试次数是个非负数（写入时校验挡着），真读到一个负数也要至少回源一次：
	// 一次都不试就返回，交出去的会是一个既没响应也没错误的结果，调用方当场崩。
	attempts := max(rt.OriginRetries+1, 1)
	var lastErr error
	for range attempts {
		attemptCtx, cancel := context.WithCancel(ctx)
		var timer *time.Timer
		if timeout > 0 {
			timer = time.AfterFunc(timeout, cancel)
		}
		started := time.Now()
		resp, err := p.fetchUpstream(attemptCtx, target, upstream)
		if timer != nil {
			timer.Stop()
		}
		// 每一次尝试都记一条：重试是真的又打了上游一次，合并成一条会让
		// 「katch 给上游添了多少负载」这个问题答错，而那正是限流时要看的数。
		// 拿不到响应记成 status=0——它同样是一次真实发生的回源。
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		p.metrics().RecordOrigin(target.Host, status, time.Since(started))
		if err == nil {
			return resp, cancel, nil
		}
		cancel()
		lastErr = err
		if ctx.Err() != nil {
			// 调用方自己走了，再试也是白试。
			break
		}
	}
	return nil, nil, lastErr
}

// fetchUpstream 按上游类别选一条回源方式。
//
// registry 走适配器：它要补回 /v2 协议前缀、按记录补全 library/，还要在上游
// 要求鉴权时自己换 token（决策 5）。static 上游原样回源，一个字节都不改——
// APT 的响应体在 InRelease 的 GPG 签名覆盖范围内。
func (p *proxySvc) fetchUpstream(
	ctx context.Context, target *Target, upstream *upstream_entity.Upstream,
) (*origin.Response, error) {
	if target.Kind == dispatch.KindRegistry {
		return p.registry.Do(ctx, &registry.Request{
			Host:              target.Host,
			Origin:            upstream.Origin,
			Path:              target.Path,
			RawQuery:          target.RawQuery,
			Method:            target.Method,
			Header:            target.Header,
			LibraryCompletion: upstream.LibraryCompletion,
		})
	}
	return p.origin.Do(ctx, &origin.Request{
		Method:   target.Method,
		Origin:   upstream.Origin,
		Path:     target.Path,
		RawQuery: target.RawQuery,
		Header:   target.Header,
	})
}

// kindMatches 校验请求形态与上游类别是否相符。
//
// 类别对不上就当作没有这个上游：registry 客户端固定走 /v2 前缀，一个 static 上游
// 出现在 /v2/ 之下（或反过来）只可能是拼错或在试探，按白名单之外处理最省事。
func kindMatches(target *Target, upstream *upstream_entity.Upstream) bool {
	switch target.Kind {
	case dispatch.KindRegistry:
		return upstream.Kind == upstream_entity.KindRegistry
	case dispatch.KindStatic:
		return upstream.Kind == upstream_entity.KindStatic
	default:
		// 其余归属根本不该走到回源，走到了就是调用方漏判了一种分支。
		return false
	}
}
