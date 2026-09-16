package cache_repo

import (
	"context"

	"github.com/cago-frame/cago/database/db"

	"github.com/CodFrm/katch/internal/model/entity/cache_entity"
)

// imageColumns 镜像视图聚合用得上的列：内容类型与创建/更新时间不在其中。
const imageColumns = "id,upstream_id,`key`,digest,size,immutable,pinned,expires_at,last_access_at,hit_count"

// ScanByUpstream 镜像按仓库归并要找「最后一个动词段」，sqlite 与 MySQL 没有一个
// 共同的反向查找函数能在 SQL 里切出来，所以聚合放在 service 里做；这里按主键分批
// 给出记录（id>? ORDER BY id 走主键），调用方一次只在内存里放一批。
func (c *cacheObjectRepo) ScanByUpstream(ctx context.Context, upstreamID, afterID int64, limit int) ([]*cache_entity.CacheObject, error) {
	list := make([]*cache_entity.CacheObject, 0, limit)
	if err := db.Ctx(ctx).Select(imageColumns).
		Where("upstream_id=? AND id>?", upstreamID, afterID).
		Order("id").Limit(limit).Find(&list).Error; err != nil {
		return nil, err
	}
	return list, nil
}
