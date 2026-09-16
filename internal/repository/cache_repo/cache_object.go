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
	// DeleteUnchanged 人手清除用：只在记录仍指着 digest（列出之后没被拉取按新内容写回）
	// 时删除，skipPinned 为真时还要求它仍未被 pin。返回是否真的删掉了。
	DeleteUnchanged(ctx context.Context, id int64, digest string, skipPinned bool) (bool, error)
	// DeleteExpired 只在记录仍满足过期清理条件时删除，避免删掉并发提升的旧记录。
	DeleteExpired(ctx context.Context, id, before int64) (bool, error)
	// Touch 命中时只更新访问时间与命中数，不整行写回。
	Touch(ctx context.Context, id int64, at int64) error
	// SetPinned 只改 pinned 一列，理由同 Touch。
	SetPinned(ctx context.Context, id int64, pinned bool) error
	// PromoteImmutable 修正旧版本把 registry digest 对象写成可变记录的元数据。
	PromoteImmutable(ctx context.Context, id int64) error
	// TotalSize 缓存占用的总字节数，配额判定用。
	TotalSize(ctx context.Context) (int64, error)
	// SizeByUpstream 按上游分组的缓存占用，键是 upstream_id。
	//
	// 在 SQL 里求和而不是把记录捞回去加：首页页脚那张表是匿名就能打的，
	// 每打一次就把整张 cache_object 扫进内存，等于给自己开了一条放大路径。
	SizeByUpstream(ctx context.Context) (map[int64]int64, error)
	// CountByUpstream 按上游分组的缓存对象数，键是 upstream_id。
	//
	// 和 SizeByUpstream 分开而不是一次查两个聚合：/metrics 上的
	// katch_cache_objects 与 katch_cache_bytes 是两族，而这两个数的口径必须
	// 各自说得清——合在一个结构里迟早有人只更新其中一半。
	CountByUpstream(ctx context.Context) (map[int64]int64, error)
	// EvictCandidates 按最久未访问给出淘汰候选，只含不可变且未被 pin 的对象。
	EvictCandidates(ctx context.Context, limit int) ([]*cache_entity.CacheObject, error)
	// ExpiredBefore 给出已经过期的可变对象，供 TTL 清理用。
	//
	// 可变对象不进 EvictCandidates（那条只挑不可变的），所以过期之后没有任何
	// 一条路径会把它们清掉：记录和盘上的字节都留着，还一直算进配额，于是
	// 「缓存总容量有上限」这条会被一批再也没人来取的对象慢慢顶穿。
	ExpiredBefore(ctx context.Context, before int64, limit int) ([]*cache_entity.CacheObject, error)
	// CountByDigest 还有多少条记录引用同一份内容，删文件之前要问一次。
	CountByDigest(ctx context.Context, digest string) (int64, error)
	Search(ctx context.Context, opt *cache_entity.SearchOption) ([]*cache_entity.CacheObject, int64, error)
	ListByUpstream(ctx context.Context, upstreamID int64) ([]*cache_entity.CacheObject, error)

	// 下面几条是管理界面的目录树（实现见 cache_tree.go）。层级不落表，按键前缀现算：
	// 键以 / 开头，查询串之前的 `/` 才是目录分隔，查询串里的 `/` 不算。

	// StatByUpstream 树根：每个有缓存对象的上游一行合计。
	StatByUpstream(ctx context.Context) ([]*cache_entity.UpstreamTreeStat, error)
	// StatByPrefix 一个目录下的合计，以及直接子目录数与直接对象数。
	StatByPrefix(ctx context.Context, upstreamID int64, prefix string) (*cache_entity.PrefixTreeStat, error)
	// ListTreeDirs 一层的子目录，按名称排序。
	ListTreeDirs(ctx context.Context, opt *cache_entity.TreeOption) ([]*cache_entity.TreeDir, error)
	// ListTreeObjects 一层的对象（不在任何子目录里的记录），按键排序。
	ListTreeObjects(ctx context.Context, opt *cache_entity.TreeOption) ([]*cache_entity.CacheObject, error)
	// SearchTree 目录下不区分大小写的子串搜索，返回前 Limit 条与命中总数。
	SearchTree(ctx context.Context, opt *cache_entity.TreeSearchOption) ([]*cache_entity.CacheObject, int64, error)
	// StatTreeMatch 搜索结果里一个目录的合计与命中部分的合计。
	StatTreeMatch(ctx context.Context, opt *cache_entity.TreeMatchOption) (*cache_entity.TreeMatchStat, error)
	// ListByPrefix 前缀下（递归到底，不排除子目录）全部对象，供按目录清除用。
	ListByPrefix(ctx context.Context, upstreamID int64, prefix string) ([]*cache_entity.CacheObject, error)

	// ScanByUpstream 管理界面的容器镜像视图（实现见 cache_image.go）：按 id 升序取
	// 一个上游里 id 大于 afterID 的至多 limit 条记录，只含镜像聚合用得上的列。
	ScanByUpstream(ctx context.Context, upstreamID, afterID int64, limit int) ([]*cache_entity.CacheObject, error)
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

func (c *cacheObjectRepo) DeleteUnchanged(ctx context.Context, id int64, digest string, skipPinned bool) (bool, error) {
	where, args := "id=? AND digest=?", []any{id, digest}
	if skipPinned {
		where, args = where+" AND pinned=?", append(args, false)
	}
	result := db.Ctx(ctx).Where(where, args...).Delete(&cache_entity.CacheObject{})
	return result.RowsAffected > 0, result.Error
}

func (c *cacheObjectRepo) DeleteExpired(ctx context.Context, id, before int64) (bool, error) {
	result := db.Ctx(ctx).
		Where("id=? AND immutable=? AND expires_at>0 AND expires_at<=? AND pinned=?",
			id, false, before, false).
		Delete(&cache_entity.CacheObject{})
	return result.RowsAffected > 0, result.Error
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

func (c *cacheObjectRepo) PromoteImmutable(ctx context.Context, id int64) error {
	return db.Ctx(ctx).Model(&cache_entity.CacheObject{}).Where("id=?", id).
		Updates(map[string]any{
			"expires_at": int64(0),
			"immutable":  true,
			"updatetime": time.Now().Unix(),
		}).Error
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

func (c *cacheObjectRepo) SizeByUpstream(ctx context.Context) (map[int64]int64, error) {
	rows := make([]struct {
		UpstreamID int64 `gorm:"column:upstream_id"`
		Size       int64 `gorm:"column:size"`
	}, 0)
	// COALESCE 的理由同 TotalSize：一组里的 size 全是 NULL 时 SUM 也是 NULL，
	// 扫进 int64 会报错。没有缓存的上游干脆不在结果里，由调用方按 0 处理。
	if err := db.Ctx(ctx).Model(&cache_entity.CacheObject{}).
		Select("upstream_id,COALESCE(SUM(size), 0) AS size").
		Group("upstream_id").
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	ret := make(map[int64]int64, len(rows))
	for _, row := range rows {
		ret[row.UpstreamID] = row.Size
	}
	return ret, nil
}

func (c *cacheObjectRepo) CountByUpstream(ctx context.Context) (map[int64]int64, error) {
	rows := make([]struct {
		UpstreamID int64 `gorm:"column:upstream_id"`
		Count      int64 `gorm:"column:count"`
	}, 0)
	if err := db.Ctx(ctx).Model(&cache_entity.CacheObject{}).
		Select("upstream_id,COUNT(*) AS count").
		Group("upstream_id").
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	ret := make(map[int64]int64, len(rows))
	for _, row := range rows {
		ret[row.UpstreamID] = row.Count
	}
	return ret, nil
}

func (c *cacheObjectRepo) ExpiredBefore(ctx context.Context, before int64, limit int) ([]*cache_entity.CacheObject, error) {
	list := make([]*cache_entity.CacheObject, 0, limit)
	// expires_at=0 是「不过期」而不是「1970 年就过期了」；同时显式限定可变对象，
	// 避免升级遗留或人工修复留下 immutable=true + 旧 expires_at 的不一致行反复入选。
	// pin 的对象留下：人明确要求常驻的东西不该被一次例行清理带走。
	if err := db.Ctx(ctx).
		Where("expires_at>0 AND expires_at<=? AND immutable=? AND pinned=?", before, false, false).
		Order("expires_at asc").Limit(limit).Find(&list).Error; err != nil {
		return nil, err
	}
	return list, nil
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
