package proxy_svc

import (
	"context"
	"io"
	"sync"
)

// originSlots 回源并发闸：同时压在上游那一侧的回源数不超过设置里的上限。
//
// 上限是**每次取名额时现读**的，而不是构造时定死的一个信号量容量：并发上限是
// setting 表里的运行时项，站长改小它之后下一次回源就得排队，不必重启进程
// （决策 3/4）。容量固定的信号量（含 x/sync/semaphore）改不了大小，要么重建——
// 重建的那一刻手上还攥着名额的请求会把新旧两个信号量的账算乱。
//
// 名额从开始回源一直占到响应体被关闭，而不是拿到响应头就放：这道闸要护的是上游
// 那边同时被打开的连接数，拿到头就放等于完全不限速一次大对象拉取。
type originSlots struct {
	// mu 护下面两个字段。
	mu       sync.Mutex
	inflight int
	// waiters 排队等名额的人。唤醒只是「再去看一眼」的信号，名额本身不随之转交——
	// 转交要求唤醒方知道被唤醒方用的是哪个上限，而上限是各自现读的。
	waiters []chan struct{}
}

// acquire 取一个回源名额，取不到就等。limit <= 0 表示不限并发。
//
// 返回的是「还名额」那个动作本身，而不是让调用方记着当初用的是哪个上限再还一次：
// 上限每次现读，一取一还之间它可能已经被改过，靠调用方传回同一个数来配平是个
// 迟早要错的约定。它可以被调用多次，只有第一次算数。
func (s *originSlots) acquire(ctx context.Context, limit int) (func(), error) {
	if limit <= 0 {
		return func() {}, nil
	}
	release := sync.OnceFunc(s.release)
	for {
		s.mu.Lock()
		if s.inflight < limit {
			s.inflight++
			s.mu.Unlock()
			return release, nil
		}
		ch := make(chan struct{}, 1)
		s.waiters = append(s.waiters, ch)
		s.mu.Unlock()

		select {
		case <-ch:
			// 被唤醒之后回到循环顶上重新判定：名额可能已经被别人抢走，
			// 上限也可能刚刚被改过。判定与重新排队都在同一次持锁里完成，
			// 中间不会漏掉一次 release。
		case <-ctx.Done():
			s.abandon(ch)
			return nil, ctx.Err()
		}
	}
}

// release 还一个名额，只由 acquire 交出去的那个闭包调用。
func (s *originSlots) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inflight--
	s.wake()
}

// wake 叫醒一个还在排队的人，持锁调用。
func (s *originSlots) wake() {
	for len(s.waiters) > 0 {
		ch := s.waiters[0]
		s.waiters = s.waiters[1:]
		select {
		case ch <- struct{}{}:
			return
		default:
			// 这个等待者已经走了（ctx 取消），换下一个。
		}
	}
}

// abandon 不等了：把自己从队伍里摘掉。
func (s *originSlots) abandon(ch chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, waiter := range s.waiters {
		if waiter == ch {
			s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
			return
		}
	}
	// 已经不在队伍里，说明唤醒正发给它而它不要了。这一次唤醒不能就这么丢掉，
	// 否则队伍后面的人会在名额空着的情况下一直等下去。
	select {
	case <-ch:
	default:
	}
	s.wake()
}

// guardedBody 把这次回源占着的东西挂在响应体的生命周期上。
//
// 并发名额与超时用的那个 context 都活到响应体被关闭为止：名额早放会让并发上限
// 形同虚设，context 早取消会把正在读的响应体掐断。调用方本来就负责关闭响应体
// （origin.Response 的约定），这里搭的是同一趟车。
type guardedBody struct {
	io.ReadCloser
	once sync.Once
	done func()
}

func (b *guardedBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.done)
	return err
}
