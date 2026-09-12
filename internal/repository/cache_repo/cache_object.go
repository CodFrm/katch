// Package cache_repo 是缓存对象的数据访问层。
package cache_repo

import (
	"context"
	"time"

	"github.com/cago-frame/cago/database/db"
	"gorm.io/gorm"

	"github.com/CodFrm/katch/internal/model/entity/cache_entity"
)

//go:generate mockgen -source cache_object.go -destination mock/cache_object.go -package mock_cache_repo

// CacheObjectRepo 缓存对象的存取。
//
// 「没缓存过」不是错误：FindByKey 未命中时返回 (nil, nil)，未命中是拉取路径上的
// 常态，当成错误只会让每一次冷拉取都在日志里留下一条 error。
type CacheObjectRepo interface {
	Find(ctx context.Context, id int64) (*cache_entity.CacheObject, error)
	FindByKey(ctx context.Context, upstreamID int64, key string) (*cache_entity.CacheObject, error)
	Save(ctx context.Context, object *cache_entity.CacheObject) error
	Delete(ctx context.Context, id int64) error
	// Touch 命中时只更新访问时间与命中数，不整行写回。
	Touch(ctx context.Context, id int64, at int64) error
	// SetPinned 只改 pinned 一列，理由同 Touch。
	SetPinned(ctx context.Context, id int64, pinned bool) error
	// TotalSize 缓存占用的总字节数，配额判定用。
	TotalSize(ctx context.Context) (int64, error)
	// EvictCandidates 按最久未访问给出淘汰候选，只含不可变且未被 pin 的对象。
	EvictCandidates(ctx context.Context, limit int) ([]*cache_entity.CacheObject, error)
	// CountByDigest 还有多少条记录引用同一份内容，删文件之前要问一次。
	CountByDigest(ctx context.Context, digest string) (int64, error)
	Search(ctx context.Context, opt *cache_entity.SearchOption) ([]*cache_entity.CacheObject, int64, error)
	ListByUpstream(ctx context.Context, upstreamID int64) ([]*cache_entity.CacheObject, error)
}

var defaultCacheObject CacheObjectRepo

// CacheObject 返回已注册的实现。
func CacheObject() CacheObjectRepo {
	return defaultCacheObject
}

// RegisterCacheObject 注册实现，由 main 装配、由测试注入 mock。
func RegisterCacheObject(i CacheObjectRepo) {
	defaultCacheObject = i
}

type cacheObjectRepo struct{}

// NewCacheObject 构造基于 gorm 的实现。
func NewCacheObject() CacheObjectRepo {
	return &cacheObjectRepo{}
}

func (c *cacheObjectRepo) Find(ctx context.Context, id int64) (*cache_entity.CacheObject, error) {
	ret := &cache_entity.CacheObject{}
	if err := db.Ctx(ctx).Where("id=?", id).First(ret).Error; err != nil {
		if db.RecordNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return ret, nil
}

func (c *cacheObjectRepo) FindByKey(ctx context.Context, upstreamID int64, key string) (*cache_entity.CacheObject, error) {
	ret := &cache_entity.CacheObject{}
	// key 在 MySQL 里是保留字，不加反引号这条 SQL 在 MySQL 上直接语法错误。
	if err := db.Ctx(ctx).Where("upstream_id=? AND `key`=?", upstreamID, key).First(ret).Error; err != nil {
		if db.RecordNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return ret, nil
}

func (c *cacheObjectRepo) Save(ctx context.Context, object *cache_entity.CacheObject) error {
	return db.Ctx(ctx).Save(object).Error
}

func (c *cacheObjectRepo) Delete(ctx context.Context, id int64) error {
	return db.Ctx(ctx).Where("id=?", id).Delete(&cache_entity.CacheObject{}).Error
}

func (c *cacheObjectRepo) Touch(ctx context.Context, id int64, at int64) error {
	// hit_count 用表达式自增而不是读出来加一写回去：命中是并发最高的写，
	// 读改写会把同时发生的另一次命中直接吞掉。
	return db.Ctx(ctx).Model(&cache_entity.CacheObject{}).Where("id=?", id).
		UpdateColumns(map[string]any{
			"last_access_at": at,
			"hit_count":      gorm.Expr("hit_count + 1"),
		}).Error
}

func (c *cacheObjectRepo) SetPinned(ctx context.Context, id int64, pinned bool) error {
	return db.Ctx(ctx).Model(&cache_entity.CacheObject{}).Where("id=?", id).
		Updates(map[string]any{"pinned": pinned, "updatetime": time.Now().Unix()}).Error
}

func (c *cacheObjectRepo) TotalSize(ctx context.Context) (int64, error) {
	var total int64
	// COALESCE 不能省：空表上的 SUM 是 NULL，扫进 int64 会报错，而空缓存是正常状态。
	if err := db.Ctx(ctx).Model(&cache_entity.CacheObject{}).
		Select("COALESCE(SUM(size), 0)").Scan(&total).Error; err != nil {
		return 0, err
	}
	return total, nil
}

func (c *cacheObjectRepo) EvictCandidates(ctx context.Context, limit int) ([]*cache_entity.CacheObject, error) {
	list := make([]*cache_entity.CacheObject, 0, limit)
	// 只淘汰不可变且未被 pin 的对象：可变对象由 TTL 自己过期，pin 的是人明确
	// 要求留下的，把它们卷进 LRU 就成了「刚 pin 的东西过两天又没了」。
	if err := db.Ctx(ctx).Where("immutable=? AND pinned=?", true, false).
		Order("last_access_at asc").Limit(limit).Find(&list).Error; err != nil {
		return nil, err
	}
	return list, nil
}

func (c *cacheObjectRepo) CountByDigest(ctx context.Context, digest string) (int64, error) {
	var count int64
	if err := db.Ctx(ctx).Model(&cache_entity.CacheObject{}).
		Where("digest=?", digest).Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

func (c *cacheObjectRepo) Search(ctx context.Context, opt *cache_entity.SearchOption) ([]*cache_entity.CacheObject, int64, error) {
	list := make([]*cache_entity.CacheObject, 0)
	var total int64
	query := db.Ctx(ctx).Model(&cache_entity.CacheObject{})
	if opt.UpstreamID > 0 {
		query = query.Where("upstream_id=?", opt.UpstreamID)
	}
	if opt.Keyword != "" {
		query = query.Where("`key` LIKE ?", "%"+opt.Keyword+"%")
	}
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	// 按最近访问倒序：界面上先看到的应该是正在被拉的那些对象。
	if err := query.Order("last_access_at desc").
		Offset(opt.Offset).Limit(opt.Limit).Find(&list).Error; err != nil {
		return nil, 0, err
	}
	return list, total, nil
}

func (c *cacheObjectRepo) ListByUpstream(ctx context.Context, upstreamID int64) ([]*cache_entity.CacheObject, error) {
	list := make([]*cache_entity.CacheObject, 0)
	if err := db.Ctx(ctx).Where("upstream_id=?", upstreamID).Find(&list).Error; err != nil {
		return nil, err
	}
	return list, nil
}
