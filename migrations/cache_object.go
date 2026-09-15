package migrations

import (
	"fmt"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"

	"github.com/CodFrm/katch/internal/model/entity/cache_entity"
)

// cacheObject 建缓存对象表。
//
// 表里只有「哪个上游的哪条路径对应盘上的哪一份内容」这份元数据，对象本体按内容
// 摘要存在缓存目录里：同一份字节可能被多条路径引用，删记录前要数一数引用数。
func cacheObject() *gormigrate.Migration {
	return createTable("20260912000002_cache_object",
		[]any{&cache_entity.CacheObject{}},
		func(tx *gorm.DB, t []string) []string {
			table := t[0]
			return []string{
				// key 取 500：它和 upstream_id 一起进唯一索引，utf8mb4 下 500 字符
				// 是 2000 字节，加 8 字节 bigint 仍在 InnoDB 3072 字节的键上限内。
				fmt.Sprintf("CREATE TABLE `%s` ("+
					"`id` %s,"+
					"`upstream_id` BIGINT NOT NULL DEFAULT 0,"+
					"`key` VARCHAR(500) NOT NULL,"+
					"`digest` VARCHAR(80) NOT NULL DEFAULT '',"+
					"`size` BIGINT NOT NULL DEFAULT 0,"+
					"`content_type` VARCHAR(191) NOT NULL DEFAULT '',"+
					"`immutable` BOOLEAN NOT NULL DEFAULT 0,"+
					"`pinned` BOOLEAN NOT NULL DEFAULT 0,"+
					"`expires_at` BIGINT NOT NULL DEFAULT 0,"+
					"`last_access_at` BIGINT NOT NULL DEFAULT 0,"+
					"`hit_count` BIGINT NOT NULL DEFAULT 0,"+
					"`createtime` BIGINT NOT NULL DEFAULT 0,"+
					"`updatetime` BIGINT NOT NULL DEFAULT 0)", table, autoPK(tx)),
				// 一个上游内一条路径只能有一条记录。
				fmt.Sprintf("CREATE UNIQUE INDEX `idx_%s_upstream_key` ON `%s` (`upstream_id`, `key`)", table, table),
				// LRU 的查询条件就是这三列。
				fmt.Sprintf("CREATE INDEX `idx_%s_lru` ON `%s` (`immutable`, `pinned`, `last_access_at`)", table, table),
				// 删记录前按摘要点查引用数。
				fmt.Sprintf("CREATE INDEX `idx_%s_digest` ON `%s` (`digest`)", table, table),
			}
		})
}
