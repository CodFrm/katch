package proxy_svc

import (
	"context"
	"sync"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
)

// cachedUpstreamRepo 给上游表加一层进程内缓存。
//
// 为什么是包在 repository 外面的一层，而不是在 service 里各存一份：失效点只有
// 「上游被写过」这一件事，而所有写入都要经过这个接口。装在这里，管理接口新增、
// 改、停用、删除任何一条上游都会自动让缓存失效，将来多一个写入口也不会漏——
// 靠每个写入方自己记得调一次 Invalidate 的方案，漏掉的那次表现为「界面上停用了
// 但还在回源」，而且只在生产上才看得见。
//
// 缓存的是**整张表的快照**而不是逐条问答：上游是人工维护的白名单，规模是几十条；
// 一次装载之后，未知主机也能在内存里直接答「没有」，不给探测流量留一条打到库上
// 的通路。代价是任何一次写入都丢掉整张快照，而写入是运维动作，不在热路径上。
type cachedUpstreamRepo struct {
	inner upstream_repo.UpstreamRepo
	// mu 只护 byHost 这个指针本身；byHost 一旦发布就不再改，读侧因此不必持锁读表。
	mu     sync.RWMutex
	byHost map[string]*upstream_entity.Upstream
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
	c.mu.Unlock()
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
	list, err := c.inner.List(ctx)
	if err != nil {
		return nil, err
	}
	table := make(map[string]*upstream_entity.Upstream, len(list))
	for _, v := range list {
		table[v.Host] = v
	}
	c.mu.Lock()
	c.byHost = table
	c.mu.Unlock()
	return table, nil
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
