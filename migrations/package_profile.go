package migrations

import (
	"fmt"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
)

// packageProfile 增加包管理器 profile，并建立持久化的 rewrite generation 单行时钟。
func packageProfile() *gormigrate.Migration {
	resolve := func(tx *gorm.DB) (string, string, error) {
		upstream, err := tableName(tx, &upstream_entity.Upstream{})
		if err != nil {
			return "", "", err
		}
		state, err := tableName(tx, &upstream_entity.RewriteState{})
		return upstream, state, err
	}
	return &gormigrate.Migration{
		ID: "20260916000002_package_profile",
		Migrate: func(tx *gorm.DB) error {
			upstream, state, err := resolve(tx)
			if err != nil {
				return err
			}
			return exec(tx,
				fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN `package_profile` VARCHAR(32) NOT NULL DEFAULT 'none'", upstream),
				fmt.Sprintf("CREATE TABLE `%s` (`id` BIGINT NOT NULL PRIMARY KEY,`generation` BIGINT NOT NULL DEFAULT 0)", state),
				fmt.Sprintf("INSERT INTO `%s` (`id`,`generation`) VALUES (1,0)", state),
			)
		},
		Rollback: func(tx *gorm.DB) error {
			upstream, state, err := resolve(tx)
			if err != nil {
				return err
			}
			return exec(tx,
				fmt.Sprintf("DROP TABLE IF EXISTS `%s`", state),
				fmt.Sprintf("ALTER TABLE `%s` DROP COLUMN `package_profile`", upstream),
			)
		},
	}
}
