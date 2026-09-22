package migrations

import (
	"fmt"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"

	"github.com/CodFrm/katch/internal/model/entity/cache_entity"
)

// cacheDistributionAPIVersion 给缓存记录加上 Docker-Distribution-Api-Version：未命中时
// 上游的这个头原样转发，命中要回放同一个值。
func cacheDistributionAPIVersion() *gormigrate.Migration {
	return alterTable("20260921000001_cache_distribution_api_version", &cache_entity.CacheObject{},
		func(tx *gorm.DB, table string) error {
			return exec(tx, fmt.Sprintf(
				"ALTER TABLE `%s` ADD COLUMN `docker_distribution_api_version` VARCHAR(255) NOT NULL DEFAULT ''", table))
		},
		func(tx *gorm.DB, table string) error {
			return exec(tx, fmt.Sprintf(
				"ALTER TABLE `%s` DROP COLUMN `docker_distribution_api_version`", table))
		})
}
