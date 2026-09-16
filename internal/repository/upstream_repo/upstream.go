// Package upstream_repo 是上游的数据访问层。
package upstream_repo

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cago-frame/cago/database/db"
	"gorm.io/gorm"

	"github.com/CodFrm/katch/internal/model/entity/setting_entity"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
)

//go:generate mockgen -source upstream.go -destination mock/upstream.go -package mock_upstream_repo

// UpstreamRepo 上游的存取。
//
// 「不存在」不是错误：Find/FindByHost 在没查到时返回 (nil, nil)，由上层决定
// 这算 404 还是算可以创建。
type UpstreamRepo interface {
	Find(ctx context.Context, id int64) (*upstream_entity.Upstream, error)
	FindByHost(ctx context.Context, host string) (*upstream_entity.Upstream, error)
	List(ctx context.Context) ([]*upstream_entity.Upstream, error)
	Save(ctx context.Context, upstream *upstream_entity.Upstream) error
	Delete(ctx context.Context, id int64) error
}

var defaultUpstream UpstreamRepo

// Upstream 返回已注册的实现。
func Upstream() UpstreamRepo {
	return defaultUpstream
}

// RegisterUpstream 注册实现，由 main 装配、由测试注入 mock。
func RegisterUpstream(i UpstreamRepo) {
	defaultUpstream = i
}

type upstreamRepo struct{}

// NewUpstream 构造基于 gorm 的实现。
func NewUpstream() UpstreamRepo {
	return &upstreamRepo{}
}

func (u *upstreamRepo) Find(ctx context.Context, id int64) (*upstream_entity.Upstream, error) {
	ret := &upstream_entity.Upstream{}
	if err := db.Ctx(ctx).Where("id=?", id).First(ret).Error; err != nil {
		if db.RecordNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return ret, nil
}

func (u *upstreamRepo) FindByHost(ctx context.Context, host string) (*upstream_entity.Upstream, error) {
	ret := &upstream_entity.Upstream{}
	if err := db.Ctx(ctx).Where("host=?", host).First(ret).Error; err != nil {
		if db.RecordNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return ret, nil
}

func (u *upstreamRepo) List(ctx context.Context) ([]*upstream_entity.Upstream, error) {
	list := make([]*upstream_entity.Upstream, 0)
	// 按 host 排序而不是按 id：界面上这张表是给人看的，插入顺序对读者没有意义。
	if err := db.Ctx(ctx).Order("host asc").Find(&list).Error; err != nil {
		return nil, err
	}
	return list, nil
}

// Save 主键为空时插入，否则整行更新。
//
// 用 Save 而不是 Updates(struct)：后者会跳过零值字段，于是「把 enabled 从 true
// 改成 false」这种停用上游的操作会被静默丢掉。
func (u *upstreamRepo) Save(ctx context.Context, upstream *upstream_entity.Upstream) error {
	return db.Ctx(ctx).Save(upstream).Error
}

func (u *upstreamRepo) Delete(ctx context.Context, id int64) error {
	return db.Ctx(ctx).Where("id=?", id).Delete(&upstream_entity.Upstream{}).Error
}

const (
	rewriteStateID       int64 = 1
	siteDomainSettingKey       = "site_domain"
)

// RewriteConfigSnapshot 是 generation 与其对应配置的一次数据库快照。
type RewriteConfigSnapshot struct {
	SiteDomain string
	Generation int64
	Upstreams  []*upstream_entity.Upstream
}

// RewriteConfigRepo 串行化会改变 rewrite 身份的写入，并提供一致读快照。
type RewriteConfigRepo interface {
	Transaction(ctx context.Context, fn func(context.Context) error) error
	AdvanceGeneration(ctx context.Context) error
	Snapshot(ctx context.Context) (*RewriteConfigSnapshot, error)
}

var defaultRewriteConfig RewriteConfigRepo = NewRewriteConfig()

// RewriteConfig 返回 rewrite 配置仓储。
func RewriteConfig() RewriteConfigRepo {
	return defaultRewriteConfig
}

// RegisterRewriteConfig 注入 rewrite 配置仓储。
func RegisterRewriteConfig(repo RewriteConfigRepo) {
	defaultRewriteConfig = repo
}

type rewriteConfigRepo struct{}

// NewRewriteConfig 构造数据库实现。
func NewRewriteConfig() RewriteConfigRepo {
	return &rewriteConfigRepo{}
}

// Transaction 在固定状态行上先取得写锁，使并发配置写按提交顺序比较与递增。
func (r *rewriteConfigRepo) Transaction(ctx context.Context, fn func(context.Context) error) error {
	return db.Ctx(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&upstream_entity.RewriteState{}).
			Where("id=?", rewriteStateID).
			UpdateColumn("generation", gorm.Expr("generation")).Error; err != nil {
			return err
		}
		return fn(db.WithContextDB(ctx, tx))
	})
}

// AdvanceGeneration 在当前配置事务里原子递增持久化代数。
func (r *rewriteConfigRepo) AdvanceGeneration(ctx context.Context) error {
	result := db.Ctx(ctx).Model(&upstream_entity.RewriteState{}).
		Where("id=?", rewriteStateID).
		UpdateColumn("generation", gorm.Expr("generation + 1"))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("upstream: rewrite state row is missing")
	}
	return nil
}

// Snapshot 在一个读事务里装载 generation、站点域名与全部启用上游。
func (r *rewriteConfigRepo) Snapshot(ctx context.Context) (*RewriteConfigSnapshot, error) {
	out := &RewriteConfigSnapshot{Upstreams: make([]*upstream_entity.Upstream, 0)}
	err := db.Ctx(ctx).Transaction(func(tx *gorm.DB) error {
		state := &upstream_entity.RewriteState{}
		if err := tx.Where("id=?", rewriteStateID).First(state).Error; err != nil {
			return err
		}
		out.Generation = state.Generation

		setting := &setting_entity.Setting{}
		err := tx.Where("`key`=?", siteDomainSettingKey).First(setting).Error
		if err != nil && !db.RecordNotFound(err) {
			return err
		}
		if err == nil && setting.Value != "" {
			if err := json.Unmarshal([]byte(setting.Value), &out.SiteDomain); err != nil {
				return fmt.Errorf("upstream: invalid site_domain: %w", err)
			}
		}
		return tx.Where("enabled=?", true).Order("host asc").Find(&out.Upstreams).Error
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
