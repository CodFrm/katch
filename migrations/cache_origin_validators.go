package migrations

import (
	"fmt"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"

	"github.com/CodFrm/katch/internal/model/entity/cache_entity"
)

// cacheOriginValidators 给缓存记录加上上游自己的 validator，过期后按它们发条件回源。
//
// 旧记录留空，不从 etag / last_modified 两列回填：转换过的元数据在那两列里存的是 katch
// 的表示 ETag，拿它问上游只会换回一次整份重传，留空则照旧走无条件 GET。
func cacheOriginValidators() *gormigrate.Migration {
	return alterTable("20260921000002_cache_origin_validators", &cache_entity.CacheObject{},
		func(tx *gorm.DB, table string) error {
			return exec(tx,
				fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN `origin_etag` VARCHAR(255) NOT NULL DEFAULT ''", table),
				fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN `origin_last_modified` VARCHAR(255) NOT NULL DEFAULT ''", table),
			)
		},
		func(tx *gorm.DB, table string) error {
			return exec(tx,
				fmt.Sprintf("ALTER TABLE `%s` DROP COLUMN `origin_etag`", table),
				fmt.Sprintf("ALTER TABLE `%s` DROP COLUMN `origin_last_modified`", table),
			)
		})
}
