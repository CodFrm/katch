package git_svc

import (
	"context"
	"sync"
)

// mirrorKey 一个仓库在进程内的标识。
//
// 用 \x00 而不是斜杠当分隔符：仓库路径里本来就全是斜杠，
// host="a" repo="/b" 和 host="a/" repo="b" 会拼成同一个键。
func mirrorKey(host, repo string) string {
	return host + "\x00" + repo
}

// inflightSet 正在被本进程处理的仓库。
//
// 它就是「同一个仓库的并发建镜像合并成一次」那条：第一个请求把这个仓库领走，
// 其余的直接返回（它们这一次本来就走穿透）。附带把同一个仓库的记录读写串了
// 起来——没有它，两个同时到达的请求会各自读到「还没有这条记录」，然后双双插入，
// 撞在 host+repo 那条唯一索引上。
type inflightSet struct {
	mu   sync.Mutex
	keys map[string]struct{}
}

func newInflightSet() inflightSet {
	return inflightSet{keys: map[string]struct{}{}}
}

// claim 领走一个仓库，已经有人领着时返回 false。
func (s *inflightSet) claim(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keys == nil {
		s.keys = map[string]struct{}{}
	}
	if _, ok := s.keys[key]; ok {
		return false
	}
	s.keys[key] = struct{}{}
	return true
}

// release 放掉一个仓库。
func (s *inflightSet) release(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.keys, key)
}

// held 这个仓库此刻有没有被人领着。
//
// 配额淘汰与手动删除据此跳过它：建镜像与增量同步都在往这个目录里写，而读占用
// （readerSet）盖不住这两者——同步发生在拿读占用**之前**。
func (s *inflightSet) held(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.keys[key]
	return ok
}

// syncSlots 建镜像的并发闸：同时在跑的建镜像数不超过设置里的上限。
//
// 形状与 proxy_svc 的 originSlots 相同，理由也相同：上限是**每次取名额时现读**
// 的，不是构造时定死的信号量容量——它是 setting 表里的运行时项，站长改完下一趟
// 活就得按新值排队，不必重启（决策 3/4）。两处没有共用一份实现，是因为 proxy_svc
// 反过来要调用这一层（穿透成功之后触发建镜像），共用就成了循环依赖。
type syncSlots struct {
	// mu 护下面两个字段。
	mu      sync.Mutex
	running int
	// waiters 排队等名额的人。唤醒只是「再去看一眼」的信号，名额本身不随之
	// 转交——转交要求唤醒方知道被唤醒方用的是哪个上限，而上限是各自现读的。
	waiters []chan struct{}
}

// acquire 取一个名额，取不到就等。limit <= 0 表示不限并发。
//
// 返回的是「还名额」那个动作本身，它可以被调用多次，只有第一次算数。
func (s *syncSlots) acquire(ctx context.Context, limit int) (func(), error) {
	if limit <= 0 {
		return func() {}, nil
	}
	release := sync.OnceFunc(s.release)
	for {
		s.mu.Lock()
		if s.running < limit {
			s.running++
			s.mu.Unlock()
			return release, nil
		}
		ch := make(chan struct{}, 1)
		s.waiters = append(s.waiters, ch)
		s.mu.Unlock()

		select {
		case <-ch:
			// 回到循环顶上重新判定：名额可能已经被别人抢走，上限也可能刚被改过。
		case <-ctx.Done():
			s.abandon(ch)
			return nil, ctx.Err()
		}
	}
}

func (s *syncSlots) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running--
	s.wake()
}

// wake 叫醒一个还在排队的人，持锁调用。
func (s *syncSlots) wake() {
	for len(s.waiters) > 0 {
		ch := s.waiters[0]
		s.waiters = s.waiters[1:]
		select {
		case ch <- struct{}{}:
			return
		default:
			// 这个等待者已经走了（ctx 结束），换下一个。
		}
	}
}

// abandon 不等了：把自己从队伍里摘掉。
func (s *syncSlots) abandon(ch chan struct{}) {
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
