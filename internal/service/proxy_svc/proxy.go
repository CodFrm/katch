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
	"github.com/CodFrm/katch/internal/service/git_svc"
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
	// Git git 端点的识别结果，由 dispatch.ClassifyGit 给出。零值表示这不是
	// git 请求，一切照旧——判定在拉取路径那一层做完，这里只按结果分流。
	Git dispatch.GitEndpoint
	// Body 请求体，nil 表示没有。目前只有 git 的协商请求带它。
	//
	// 它只能被读一次，所以带请求体的回源不重试，见 fetchWithRetry。
	Body io.Reader
	// ContentLength 请求体长度，-1 表示未知（分块传输）。Body 为 nil 时无意义。
	ContentLength int64
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
	if !protocolMatches(target, upstream) {
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
	// 排队等名额这一步自己要有上限，不能只靠调用方那个 context。
	//
	// 名额一直占到响应体被关闭，所以一个卡在上游那边的对象会把它攥很久；而拉取
	// 路径传进来的是一个**不会结束**的 context——cache_svc 用 context.WithoutCancel
	// 摘掉了客户端的取消（决策 9：断线也要把下载跑完），那种 context 的 Done() 是
	// nil，acquire 里那条 select 因此只剩下「等名额」一个分支。上游一卡，后面每个
	// 请求都会永久挂死在这里，连处理器和客户端连接一起攥着。
	//
	// 下面那个超时救不了这件事：它装在 fetchWithRetry 里，是取到名额**之后**才开始
	// 走的。用同一个时限来盖排队这一段——连名额都排不到的请求，快速失败远好过堆积。
	acquireCtx := ctx
	if timeout := time.Duration(rt.OriginTimeoutSeconds) * time.Second; timeout > 0 {
		var cancel context.CancelFunc
		acquireCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	release, err := p.slots.acquire(acquireCtx, rt.OriginConcurrency)
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
	// 上游的鉴权挑战到此为止。任何一条从 katch 出去的 WWW-Authenticate 都是在请
	// 客户端向 **katch** 鉴权，而 katch 是一个公开的镜像站，没有账号可以给它：
	// 浏览器会为 katch 的域名弹账号密码框，用户敲进去的凭据是交给 katch 的。
	// 一个把别人的登录框挂在自己域名下的代理是在钓鱼，不是在镜像。
	//
	// registry 那一侧本来就摘（registry.strip，决策 5），但那只盖住了两条回源方式
	// 里的一条：挂在 basic auth 后面的 apt 源走的是 static 这条。收在这里而不是
	// origin，是因为 registry 适配器要先靠这个头解析出 realm 才换得到 token——
	// 在 origin 那里摘掉会把 token 交换整个打断。这一点两条路都会经过，且适配器
	// 已经用完了它。401 本身照常透传：那是上游对这个对象的判断，katch 不替它改
	// 口径，摘掉的只是「向谁鉴权」这句话。
	resp.Header.Del("WWW-Authenticate")
	stripUpstreamAccountHeaders(resp.Header)
	// 穿透成功之后才让后台去建镜像（决策 5）：走到这里才同时知道「这是一个
	// git 仓库的只读请求」「这台上游开着 git」「上游确实认这个仓库」三件事。
	p.mirror(ctx, target, resp.StatusCode)
	return body, &Meta{
		StatusCode:    resp.StatusCode,
		Header:        resp.Header,
		ContentLength: resp.ContentLength,
	}, nil
}

// upstreamAccountHeaders 上游用来描述**katch 这个调用方**、而不是这次内容的响应头。
//
// 分两类。一类是上游对 katch 账户的记账：docker.io 每个响应都贴 Docker-Ratelimit-Source
// （这台镜像站的出口 IP）与一组 Ratelimit-*（这台镜像站的配额余量）。它们说的不是客户端
// 拿到的这个对象，转出去等于把出口 IP 和剩余额度播给每一个匿名客户端；而且缓存命中时它们
// 不会出现，留着还会让同一个 URL 的响应头随缓存状态漂移。
//
// 另一类是源站对**自己那个域名**的传输策略：Strict-Transport-Security 会被客户端安到
// katch 的域名上，Alt-Svc 更会把客户端指向源站的备用端点。Set-Cookie 同理——镜像站没有
// 会话，源站的 cookie 落在 katch 的域名下只会跟着此后每一次拉取发回来。
//
// Retry-After 不在表里：它是上游对「什么时候再来问」的答复，删掉会让客户端在 429/503
// 之后立刻重试。Date、Server 这类描述本次响应本身的头同样留着。
var upstreamAccountHeaders = []string{
	"Docker-Ratelimit-Source",
	"Ratelimit-Limit", "Ratelimit-Remaining", "Ratelimit-Reset",
	"X-Ratelimit-Limit", "X-Ratelimit-Remaining", "X-Ratelimit-Reset",
	"Strict-Transport-Security",
	"Alt-Svc",
	"Set-Cookie",
}

// stripUpstreamAccountHeaders 摘掉 upstreamAccountHeaders 里的每一条。
//
// 和 WWW-Authenticate 收在同一处，理由是同一条：这里是两条回源方式汇合、且适配器
// 已经用完这些头之后的那一点，摘在 origin 里会把 registry 的 token 交换打断。
func stripUpstreamAccountHeaders(header http.Header) {
	for _, name := range upstreamAccountHeaders {
		header.Del(name)
	}
}

// mirror 一次成功的 git 穿透之后，让镜像层把这个仓库建起来（决策 7：谁被拉过
// 就镜像谁）。它不阻塞这次拉取——登记之外的活都在镜像层的后台协程上。
//
// 只认 upload-pack：push 在拉取路径那一层就被 403 掉了，真走到这里也不该因为
// 一次写操作去建镜像。只认 200：上游的 404 说的是「这里没有这个仓库」，照着它
// 建镜像只是在一次次白打上游；401/403 同理，katch 不带凭据，建也建不下来。
func (p *proxySvc) mirror(ctx context.Context, target *Target, status int) {
	if !target.Git.IsUploadPack() || status != http.StatusOK {
		return
	}
	git_svc.Mirror().Ensure(ctx, target.Host, target.Git.Repo)
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
	if target.Body != nil {
		// 请求体是一次性的 io.Reader，第一次尝试就把它读空了。再试一次送上去
		// 的是一个空请求体，而上游会拿它当一次合法的空协商正常答复——客户端
		// 于是收到一份和它要的东西无关的 200，比一次干脆的失败难查得多。
		attempts = 1
	}
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
		Method:        target.Method,
		Origin:        upstream.Origin,
		Path:          target.Path,
		RawQuery:      target.RawQuery,
		Header:        target.Header,
		Body:          target.Body,
		ContentLength: target.ContentLength,
	})
}

// protocolMatches 校验请求形态所需的协议，这条上游开没开。
//
// 没开就当作没有这个上游：registry 客户端固定走 /v2 前缀，一个只开 static 的上游
// 出现在 /v2/ 之下（或反过来）只可能是拼错或在试探，按白名单之外处理最省事。
//
// 查集合而不是比单值，于是一条同时开了 static 与 git 的记录照常服务 static 路径，
// 而回填成 [registry] 的 docker.io 在 static 路径上仍然什么都不是。空集合对任何
// 形态都答 false，见 ProtocolSet.Has。
func protocolMatches(target *Target, upstream *upstream_entity.Upstream) bool {
	switch target.Kind {
	case dispatch.KindRegistry:
		return upstream.Protocols.Has(upstream_entity.ProtocolRegistry)
	case dispatch.KindStatic:
		// git 的端点寄生在 static 的路径空间里，但要的是 git 那一种协议：
		// 一条只开了 static 的 github.com 服务 release 资产，不服务 clone。
		if target.Git.IsGit() {
			return upstream.Protocols.Has(upstream_entity.ProtocolGit)
		}
		return upstream.Protocols.Has(upstream_entity.ProtocolStatic)
	default:
		// 其余归属根本不该走到回源，走到了就是调用方漏判了一种分支。
		return false
	}
}
