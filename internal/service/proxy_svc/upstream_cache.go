package proxy_svc

import (
	"context"
	"sync"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
)

// cachedUpstreamRepo 给上游表加一层进程内缓存。
//
// 为什么是包在 repository 外面的一层，而不是在 service 里各存一份：非事务写直接
// 经过这个接口，写成功立即失效；事务写使用 Uncached 取得底层仓储，并在外层提交成功
// 后调用 Invalidate。这样新增、改、停用、删除都会失效，同时不会在提交前开放旧数据
// 重新发布进缓存的窗口。
//
// 缓存的是**整张表的快照**而不是逐条问答：上游是人工维护的白名单，规模是几十条；
// 一次装载之后，未知主机也能在内存里直接答「没有」，不给探测流量留一条打到库上
// 的通路。代价是任何一次写入都丢掉整张快照，而写入是运维动作，不在热路径上。
type cachedUpstreamRepo struct {
	inner upstream_repo.UpstreamRepo
	// mu 只护 byHost 与 generation；byHost 一旦发布就不再改，读侧因此不必持锁读表。
	mu     sync.RWMutex
	byHost map[string]*upstream_entity.Upstream
	// generation 每次失效自增。装载期间它变了就说明这份读数已经过期，不能再发布。
	//
	// 冷装载是「查库 → 发布」两步，中间不持锁。少了这道判定，一次正好落在两步
	// 之间的写入会先失效、后被过期读数盖回去，而这层缓存没有 TTL——盖回去的那份
	// 会一直用到下一次有人写上游表为止，表现成「界面上停用了但还在回源」。
	generation uint64
	// loadMu 把并发的冷启动装载串起来，避免一堆请求同时撞上空缓存时齐刷刷查库。
	loadMu sync.Mutex
}

// NewCachedUpstreamRepo 把一个上游仓储包上进程内缓存，由 main 装配。
func NewCachedUpstreamRepo(inner upstream_repo.UpstreamRepo) upstream_repo.UpstreamRepo {
	return &cachedUpstreamRepo{inner: inner}
}

// FindByHost 走缓存：这是拉取路径上每个请求都要问一次的那个方法。
func (c *cachedUpstreamRepo) FindByHost(ctx context.Context, host string) (*upstream_entity.Upstream, error) {
	table, err := c.table(ctx)
	if err != nil {
		return nil, err
	}
	upstream, ok := table[host]
	if !ok {
		return nil, nil
	}
	return clone(upstream), nil
}

// Find 直穿到库。它只在管理路径上被调用，那里要的是「库里此刻是什么」。
func (c *cachedUpstreamRepo) Find(ctx context.Context, id int64) (*upstream_entity.Upstream, error) {
	return c.inner.Find(ctx, id)
}

// List 直穿到库，理由同 Find：管理界面必须看到刚写进去的那条。
func (c *cachedUpstreamRepo) List(ctx context.Context) ([]*upstream_entity.Upstream, error) {
	return c.inner.List(ctx)
}

// Save 写库成功后丢掉快照。写失败不动缓存——库里没变，缓存就还是对的。
func (c *cachedUpstreamRepo) Save(ctx context.Context, upstream *upstream_entity.Upstream) error {
	if err := c.inner.Save(ctx, upstream); err != nil {
		return err
	}
	c.invalidate()
	return nil
}

// Delete 同 Save。
func (c *cachedUpstreamRepo) Delete(ctx context.Context, id int64) error {
	if err := c.inner.Delete(ctx, id); err != nil {
		return err
	}
	c.invalidate()
	return nil
}

func (c *cachedUpstreamRepo) invalidate() {
	c.mu.Lock()
	c.byHost = nil
	c.generation++
	c.mu.Unlock()
}

// Uncached 返回事务回调使用的底层仓储。
func (c *cachedUpstreamRepo) Uncached() upstream_repo.UpstreamRepo {
	return c.inner
}

// Invalidate 在外层事务成功提交后失效快照。
func (c *cachedUpstreamRepo) Invalidate() {
	c.invalidate()
}

// table 取当前快照，没有就装载一份。
func (c *cachedUpstreamRepo) table(ctx context.Context) (map[string]*upstream_entity.Upstream, error) {
	if table := c.snapshot(); table != nil {
		return table, nil
	}
	c.loadMu.Lock()
	defer c.loadMu.Unlock()
	// 等锁的时候别人可能已经装载好了，再看一眼。
	if table := c.snapshot(); table != nil {
		return table, nil
	}
	c.mu.RLock()
	generation := c.generation
	c.mu.RUnlock()

	list, err := c.inner.List(ctx)
	if err != nil {
		return nil, err
	}
	table := make(map[string]*upstream_entity.Upstream, len(list))
	for _, v := range list {
		table[v.Host] = v
	}
	c.publish(table, generation)
	// 交出去的是这次读到的表，哪怕它已经过期：这次调用问的就是「刚才库里是什么」，
	// 而过期的那份不会被留下来给下一个人。
	return table, nil
}

// publish 把一次读数发布成快照，除非它已经被一次写入作废。
//
// 和 setting_svc.cachedSettingRepo.publish 是同一道判定，理由见 generation 那里。
func (c *cachedUpstreamRepo) publish(table map[string]*upstream_entity.Upstream, generation uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation != generation {
		return
	}
	c.byHost = table
}

func (c *cachedUpstreamRepo) snapshot() map[string]*upstream_entity.Upstream {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.byHost
}

// clone 复制一份再交出去，含路径模式列表这个引用字段。
func clone(src *upstream_entity.Upstream) *upstream_entity.Upstream {
	dst := *src
	dst.ImmutablePatterns = append(upstream_entity.PatternList(nil), src.ImmutablePatterns...)
	return &dst
}
