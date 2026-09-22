package migrations

import (
	"fmt"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"

	"github.com/CodFrm/katch/internal/model/entity/cache_entity"
)

// cacheChecksumHeaders adds the allowlisted Maven checksum response metadata.
func cacheChecksumHeaders() *gormigrate.Migration {
	return alterTable("20260916000004_cache_checksum_headers", &cache_entity.CacheObject{},
		func(tx *gorm.DB, table string) error {
			return exec(tx,
				fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN `x_checksum_md5` VARCHAR(255) NOT NULL DEFAULT ''", table),
				fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN `x_checksum_sha1` VARCHAR(255) NOT NULL DEFAULT ''", table),
				fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN `x_checksum_sha256` VARCHAR(255) NOT NULL DEFAULT ''", table),
				fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN `x_checksum_sha512` VARCHAR(255) NOT NULL DEFAULT ''", table),
			)
		},
		func(tx *gorm.DB, table string) error {
			return exec(tx,
				fmt.Sprintf("ALTER TABLE `%s` DROP COLUMN `x_checksum_md5`", table),
				fmt.Sprintf("ALTER TABLE `%s` DROP COLUMN `x_checksum_sha1`", table),
				fmt.Sprintf("ALTER TABLE `%s` DROP COLUMN `x_checksum_sha256`", table),
				fmt.Sprintf("ALTER TABLE `%s` DROP COLUMN `x_checksum_sha512`", table),
			)
		})
}
