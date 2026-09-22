package migrations

import (
	"fmt"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"

	"github.com/CodFrm/katch/internal/model/entity/cache_entity"
)

// cacheResponseMetadata adds the allowlisted response metadata needed for safe cache replay.
func cacheResponseMetadata() *gormigrate.Migration {
	return alterTable("20260916000003_cache_response_metadata", &cache_entity.CacheObject{},
		func(tx *gorm.DB, table string) error {
			return exec(tx,
				fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN `cache_control` VARCHAR(512) NOT NULL DEFAULT ''", table),
				fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN `origin_date` VARCHAR(64) NOT NULL DEFAULT ''", table),
				fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN `origin_age` BIGINT NOT NULL DEFAULT 0", table),
				fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN `origin_expires` VARCHAR(64) NOT NULL DEFAULT ''", table),
				fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN `vary` VARCHAR(512) NOT NULL DEFAULT ''", table),
				fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN `accept_ranges` VARCHAR(64) NOT NULL DEFAULT ''", table),
				fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN `content_disposition` VARCHAR(2048) NOT NULL DEFAULT ''", table),
				fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN `docker_content_digest` VARCHAR(255) NOT NULL DEFAULT ''", table),
				fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN `stored_at` BIGINT NOT NULL DEFAULT 0", table),
				fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN `requires_revalidation` BOOLEAN NOT NULL DEFAULT 0", table),
			)
		},
		func(tx *gorm.DB, table string) error {
			return exec(tx,
				fmt.Sprintf("ALTER TABLE `%s` DROP COLUMN `cache_control`", table),
				fmt.Sprintf("ALTER TABLE `%s` DROP COLUMN `origin_date`", table),
				fmt.Sprintf("ALTER TABLE `%s` DROP COLUMN `origin_age`", table),
				fmt.Sprintf("ALTER TABLE `%s` DROP COLUMN `origin_expires`", table),
				fmt.Sprintf("ALTER TABLE `%s` DROP COLUMN `vary`", table),
				fmt.Sprintf("ALTER TABLE `%s` DROP COLUMN `accept_ranges`", table),
				fmt.Sprintf("ALTER TABLE `%s` DROP COLUMN `content_disposition`", table),
				fmt.Sprintf("ALTER TABLE `%s` DROP COLUMN `docker_content_digest`", table),
				fmt.Sprintf("ALTER TABLE `%s` DROP COLUMN `stored_at`", table),
				fmt.Sprintf("ALTER TABLE `%s` DROP COLUMN `requires_revalidation`", table),
			)
		})
}
