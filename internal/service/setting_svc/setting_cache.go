package setting_svc

import (
	"context"
	"sync"

	"github.com/CodFrm/katch/internal/model/entity/setting_entity"
	"github.com/CodFrm/katch/internal/repository/setting_repo"
)

// cachedSettingRepo 给设置表加一层进程内缓存。
//
// 为什么需要它：运行时设置被读在热路径上——每一次回源都要问一遍超时、并发与重试，
// 每一次缓存写入都要问一遍配额。一次 docker pull 是几十上百个请求，逐个查库等于把
// 镜像站的吞吐绑在 sqlite 上。这和上游表那层缓存（proxy_svc.NewCachedUpstreamRepo）
// 是同一个理由，也用同一个形状。
//
// 为什么包在 repository 外面而不是在 service 里各存一份：失效点只有「设置被写过」
// 这一件事，而所有写入都要经过这个接口。装在这里，管理接口保存设置、轮换密钥、
// 首次落初始密钥全都会自动让缓存失效，将来多一个写入口也不会漏——靠每个写入方
// 自己记得调一次 Invalidate 的方案，漏掉的那次表现为「界面上改了但没生效」，
// 而那正是决策 3/4 要保证不会发生的事。
//
// 缓存是逐键的（SettingRepo 没有列出整表的方法），**包括「这一行不存在」**：
// 站长没配过的项在库里没有行，不把「没有」也记下来，那些项就会每次请求都查一次库。
type cachedSettingRepo struct {
	inner setting_repo.SettingRepo
	// mu 护 rows 与 generation。
	mu sync.RWMutex
	// rows 键 → 行，值为 nil 表示「库里确实没有这一行」。
	rows map[string]*setting_entity.Setting
	// generation 每次失效自增。装载期间它变了就说明这份读数已经过期，不能再发布。
	generation uint64
}

// NewCachedSettingRepo 把一个设置仓储包上进程内缓存，由 main 装配。
func NewCachedSettingRepo(inner setting_repo.SettingRepo) setting_repo.SettingRepo {
	return &cachedSettingRepo{inner: inner, rows: map[string]*setting_entity.Setting{}}
}

func (c *cachedSettingRepo) Find(ctx context.Context, key string) (*setting_entity.Setting, error) {
	if row, ok := c.lookup(key); ok {
		return cloneSetting(row), nil
	}
	c.mu.RLock()
	generation := c.generation
	c.mu.RUnlock()

	row, err := c.inner.Find(ctx, key)
	if err != nil {
		return nil, err
	}
	c.publish(key, row, generation)
	return cloneSetting(row), nil
}

// Save 写库成功后丢掉缓存。写失败不动缓存——库里没变，缓存就还是对的。
func (c *cachedSettingRepo) Save(ctx context.Context, setting *setting_entity.Setting) error {
	if err := c.inner.Save(ctx, setting); err != nil {
		return err
	}
	c.invalidate()
	return nil
}

func (c *cachedSettingRepo) lookup(key string) (*setting_entity.Setting, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	row, ok := c.rows[key]
	return row, ok
}

// publish 把一次读数放进缓存，除非它已经被一次写入作废。
//
// 没有这道判定的话，一次「读到旧值」和一次「写入」交错时，旧值会被写在失效之后，
// 于是设置改完之后反而读回了改之前的那个——恰好是这层缓存最不能出的那种错。
func (c *cachedSettingRepo) publish(key string, row *setting_entity.Setting, generation uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation != generation {
		return
	}
	c.rows[key] = cloneSetting(row)
}

func (c *cachedSettingRepo) invalidate() {
	c.mu.Lock()
	c.rows = map[string]*setting_entity.Setting{}
	c.generation++
	c.mu.Unlock()
}

// cloneSetting 复制一份再交出去：调用方改了手上那份，不该改到缓存里的。
func cloneSetting(src *setting_entity.Setting) *setting_entity.Setting {
	if src == nil {
		return nil
	}
	dst := *src
	return &dst
}
