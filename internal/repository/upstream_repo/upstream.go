// Package upstream_repo 是上游的数据访问层。
package upstream_repo

import (
	"context"

	"github.com/cago-frame/cago/database/db"

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
