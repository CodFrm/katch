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

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/dispatch"
	"github.com/CodFrm/katch/internal/proxy/origin"
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
}

type proxySvc struct {
	origin *origin.Client
	gate   Gate
}

// New 构造拉取路径的业务层。
func New(opt Options) ProxySvc {
	return &proxySvc{origin: origin.New(), gate: opt.Gate}
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
	path, ok := upstreamPath(target, upstream)
	if !ok {
		return nil, nil, ErrUpstreamNotAllowed
	}
	// 闸问在这里，而不是在白名单判定之前：表里没有的主机名必须先拿到那一个
	// 统一的 ErrUpstreamNotAllowed，否则「退避中」和「不存在」两种回应之间的
	// 差别，就成了探测内网主机是否存在的信号（决策 6）。
	if p.gate != nil && !p.gate.Allow(target.Host) {
		return nil, nil, ErrUpstreamBackoff
	}
	resp, err := p.origin.Do(ctx, &origin.Request{
		Method:   target.Method,
		Origin:   upstream.Origin,
		Path:     path,
		RawQuery: target.RawQuery,
		Header:   target.Header,
	})
	if err != nil {
		return nil, nil, err
	}
	return resp.Body, &Meta{
		StatusCode:    resp.StatusCode,
		Header:        resp.Header,
		ContentLength: resp.ContentLength,
	}, nil
}

// upstreamPath 求出上游侧路径，并顺带校验请求形态与上游类别是否相符。
//
// 类别对不上就当作没有这个上游：registry 客户端固定走 /v2 前缀，一个 static 上游
// 出现在 /v2/ 之下（或反过来）只可能是拼错或在试探，按白名单之外处理最省事。
func upstreamPath(target *Target, upstream *upstream_entity.Upstream) (string, bool) {
	switch target.Kind {
	case dispatch.KindRegistry:
		if upstream.Kind != upstream_entity.KindRegistry {
			return "", false
		}
		// dispatch 摘掉的 /v2 是 registry 的协议前缀，回源时补回去。
		return "/v2" + target.Path, true
	case dispatch.KindStatic:
		if upstream.Kind != upstream_entity.KindStatic {
			return "", false
		}
		return target.Path, true
	default:
		// 其余归属根本不该走到回源，走到了就是调用方漏判了一种分支。
		return "", false
	}
}
