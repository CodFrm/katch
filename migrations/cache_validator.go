package migrations

import (
	"fmt"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"

	"github.com/CodFrm/katch/internal/model/entity/cache_entity"
)

// cacheValidator 给缓存记录加上上游 validator 所在的两列。
//
// 已有行保持原样，新列留空：规格明确不为历史静态对象推导或伪造上游 validator
// （决策 5）。列做成 NOT NULL DEFAULT ”，与同表其余字符串列一致，读出来就是
// 空串，不必区分 NULL 与「上游没给」这两种其实同义的状态。
//
// 两条独立的 ALTER，理由同 upstreamProtocols：MySQL 认多子句，sqlite 不认。
// 两列都不带索引，回滚时的 DROP COLUMN 不会被 sqlite 拒绝。
func cacheValidator() *gormigrate.Migration {
	return alterTable("20260916000001_cache_validator",
		&cache_entity.CacheObject{},
		func(tx *gorm.DB, table string) error {
			return exec(tx,
				fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN `etag` VARCHAR(255) NOT NULL DEFAULT ''", table),
				fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN `last_modified` VARCHAR(64) NOT NULL DEFAULT ''", table),
			)
		},
		func(tx *gorm.DB, table string) error {
			return exec(tx,
				fmt.Sprintf("ALTER TABLE `%s` DROP COLUMN `etag`", table),
				fmt.Sprintf("ALTER TABLE `%s` DROP COLUMN `last_modified`", table),
			)
		})
}
