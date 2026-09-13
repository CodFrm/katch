package proxy_svc

import (
	"context"
	"sync"

	"github.com/CodFrm/katch/internal/model/entity/event_entity"
	"github.com/CodFrm/katch/internal/service/event_svc"
)

// BackoffTracker 退避状态的读写面，由 internal/proxy/backoff.Tracker 实现。
//
// 声明成接口而不是直接引用那个结构体：EventGate 只需要这四个方法，而用例要能
// 塞一个假的进来。方法集刻意和 metrics.Gate（Failure/Success/Degraded）加
// proxy_svc.Gate（Allow）合起来一致，所以包装之后的东西在两处都插得上。
type BackoffTracker interface {
	Allow(host string) bool
	Failure(host string)
	Success(host string)
	Degraded(host string) bool
}

// EventGate 在退避状态**发生转换**时往事件流里记一条。
//
// 它是个装饰器，不是第二套退避：状态仍然只有 internal/proxy/backoff 那一份
// （决策 17：内存态，不落库）。这里落库的只是「什么时候开始降级、什么时候恢复」
// 这两个瞬间——界面上那条时间线要的是转换，而不是每一个撞上退避的请求。
//
// 为什么不在 Fetch 里记：Fetch 只问「这次要不要打上游」，回源的成败是由拉取
// 路径最外层的计数中间件在响应之后喂进来的（metrics.feedGate）。转换只有在
// 喂进来的那一刻才看得见。
type EventGate struct {
	inner BackoffTracker
	// mu 只护 degraded 这张表，并且让「读退避状态 + 比对上一次」成为一步。
	//
	// 分两步会让两个同时失败的请求都看到「刚从正常变成降级」，于是同一次转换
	// 落两条事件。锁里**不做**落库：Allow 走的是拉取热路径，把一次数据库写入
	// 锁在它前面，等于让事件表的抖动拖住整台镜像站。
	mu sync.Mutex
	// degraded 只记降级中的上游：恢复之后这一项被删掉，
	// 否则一个跑了几个月的进程会把每个来过的主机名都留在表里。
	degraded map[string]bool
}

// NewEventGate 包一个退避器，让它的状态转换落进事件流。
func NewEventGate(inner BackoffTracker) *EventGate {
	return &EventGate{inner: inner, degraded: map[string]bool{}}
}

// Allow 原样转交。这是拉取热路径上的那一问，不碰事件流。
func (g *EventGate) Allow(host string) bool {
	return g.inner.Allow(host)
}

// Failure 记一次回源失败，失败到进退避的那一次落一条降级事件。
func (g *EventGate) Failure(host string) {
	g.inner.Failure(host)
	g.recordTransition(host)
}

// Success 记一次回源成功，从退避里出来的那一次落一条恢复事件。
func (g *EventGate) Success(host string) {
	g.inner.Success(host)
	g.recordTransition(host)
}

// Degraded 原样转交。
func (g *EventGate) Degraded(host string) bool {
	return g.inner.Degraded(host)
}

// recordTransition 只在降级状态真的翻面时记一条。
//
// 用的是 context.Background()：喂退避的那一层（计数中间件）在响应写完之后才跑，
// 手里那个请求 context 随时会被取消，拿它去写库等于让客户端断线决定这条事件
// 记不记得上。
func (g *EventGate) recordTransition(host string) {
	g.mu.Lock()
	now := g.inner.Degraded(host)
	was := g.degraded[host]
	if now == was {
		g.mu.Unlock()
		return
	}
	if now {
		g.degraded[host] = true
	} else {
		delete(g.degraded, host)
	}
	g.mu.Unlock()

	kind := event_entity.KindUpstreamRecovered
	if now {
		kind = event_entity.KindUpstreamDegraded
	}
	event_svc.Event().Record(context.Background(), &event_svc.RecordInput{
		Kind: kind, Actor: event_entity.ActorSystem,
		// 这一缝上只有主机名——退避按主机名记状态，为一条事件再查一次上游表，
		// 就是在上游正出问题的时候给库多加一次查询。界面按主机名对得上。
		Detail: map[string]any{"host": host},
	})
}
