// Package backoff 记录每个上游的连续回源失败，失败到一定次数就让它快速失败一段时间。
//
// 状态只在内存里，**不落库**（决策 17）：上游限流时继续全量打过去只会延长封禁，
// 而退避是运行时事实，进程重启后重新探测即可；落库反而会把一段早已过期的退避
// 带到下一个进程里。
package backoff

import (
	"sync"
	"time"
)

const (
	// defaultThreshold 连续失败多少次算「这个上游此刻不可用」。
	// 取 3 而不是 1：单次超时更可能是这一个对象或这一条连接的问题。
	defaultThreshold = 3
	// defaultBase 第一次退避的时长。
	defaultBase = time.Second
	// defaultMax 退避时长上限。再长只会让一个已经恢复的上游迟迟不被重试。
	defaultMax = 60 * time.Second
	// maxShift 退避时长的位移上限，纯粹防止 1<<n 溢出。
	maxShift = 32
)

// Options 退避参数。零值按上面的默认值处理。
type Options struct {
	// Threshold 连续失败多少次进入退避。
	Threshold int
	// Base 进入退避后的首次等待时长，之后每失败一次翻一倍。
	Base time.Duration
	// Max 等待时长上限。
	Max time.Duration
	// Now 取当前时间，用例注入假时钟用——真等一个退避窗口过去会让测试套凭空变慢。
	Now func() time.Time
}

// Status 一个降级中的上游，供界面标注「限流中/降级」。
type Status struct {
	Host string `json:"host"`
	// Failures 连续失败次数。
	Failures int `json:"failures"`
	// RetryAt 下一次放行探测的时刻（秒）。
	RetryAt int64 `json:"retry_at"`
}

type hostState struct {
	failures int
	retryAt  time.Time
}

// Tracker 按上游主机名记录失败。零值不可用，用 New 构造。
type Tracker struct {
	opt Options
	mu  sync.Mutex
	// state 只保留还在失败的上游：成功一次就把这一项删掉，
	// 否则一个跑了几个月的进程会把每个来过的主机名都留在表里。
	state map[string]*hostState
}

// New 构造 Tracker。
func New(opt Options) *Tracker {
	if opt.Threshold <= 0 {
		opt.Threshold = defaultThreshold
	}
	if opt.Base <= 0 {
		opt.Base = defaultBase
	}
	if opt.Max <= 0 {
		opt.Max = defaultMax
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &Tracker{opt: opt, state: map[string]*hostState{}}
}

// Allow 这次请求要不要真的打到上游。false 表示还在退避窗口里，应当快速失败。
//
// 窗口一过就放行，不限流只放一个探测：katch 的调用方是 docker / apt 这类会自己
// 重试的客户端，为了「只探一次」再加一把锁，换来的只是把恢复推迟到下一轮。
func (t *Tracker) Allow(host string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.state[host]
	if !ok || s.failures < t.opt.Threshold {
		return true
	}
	return !t.opt.Now().Before(s.retryAt)
}

// Failure 记一次回源失败。
//
// 只记真的打到上游的那些。窗口里的请求被闸挡在回源之前，调用方看到的同样是一次
// 失败并原样喂回来，但上游根本没被碰过：把它算成新证据，会让 docker / apt 的自动
// 重试把退避一路顶到上限并不断顺延，上游恢复了也等不到那次探测。
func (t *Tracker) Failure(host string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.state[host]
	if !ok {
		s = &hostState{}
		t.state[host] = s
	}
	if s.failures >= t.opt.Threshold && t.opt.Now().Before(s.retryAt) {
		return
	}
	s.failures++
	if s.failures >= t.opt.Threshold {
		s.retryAt = t.opt.Now().Add(t.delay(s.failures))
	}
}

// Success 记一次成功：状态整个清掉。
//
// 不是「失败数减一」：连续失败的计数一旦被一次成功打断就不再连续，
// 留着它会让上游刚恢复就因为再抖一次而重新进退避。
func (t *Tracker) Success(host string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.state, host)
}

// Degraded 这个上游此刻是不是降级中。
//
// 退避窗口过去之后它仍然是降级：那时只是「可以试一下了」，探测成功之前
// 界面上摘掉标记就是在报告一个还没发生的恢复。
func (t *Tracker) Degraded(host string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.state[host]
	return ok && s.failures >= t.opt.Threshold
}

// Snapshot 列出全部降级中的上游。
func (t *Tracker) Snapshot() []Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	list := make([]Status, 0, len(t.state))
	for host, s := range t.state {
		if s.failures < t.opt.Threshold {
			continue
		}
		list = append(list, Status{Host: host, Failures: s.failures, RetryAt: s.retryAt.Unix()})
	}
	return list
}

// delay 第 n 次连续失败后要等多久：从 Base 起每多失败一次翻一倍，封顶 Max。
func (t *Tracker) delay(failures int) time.Duration {
	shift := failures - t.opt.Threshold
	if shift > maxShift {
		return t.opt.Max
	}
	d := t.opt.Base << uint(shift)
	if d > t.opt.Max || d <= 0 {
		return t.opt.Max
	}
	return d
}
