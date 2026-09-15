package migrations

import (
	"fmt"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"

	"github.com/CodFrm/katch/internal/model/entity/setting_entity"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
)

// upstreamAndSetting 建上游表与设置表。
//
// upstream 是代理的白名单：不在表里或 enabled 为假的主机一律 404，否则 katch
// 就是一个任何人都能拿来当跳板的开放代理。setting 是运行时配置的键值表。
func upstreamAndSetting() *gormigrate.Migration {
	return createTable("20260912000001_upstream_and_setting",
		[]any{&upstream_entity.Upstream{}, &setting_entity.Setting{}},
		func(tx *gorm.DB, t []string) []string {
			upstream, setting := t[0], t[1]
			return []string{
				fmt.Sprintf("CREATE TABLE `%s` ("+
					"`id` %s,"+
					"`host` VARCHAR(191) NOT NULL,"+
					"`kind` VARCHAR(32) NOT NULL,"+
					"`origin` VARCHAR(512) NOT NULL,"+
					"`enabled` BOOLEAN NOT NULL DEFAULT 0,"+
					"`immutable_patterns` TEXT,"+
					"`mutable_ttl_seconds` INT NOT NULL DEFAULT 0,"+
					"`default_policy` VARCHAR(32) NOT NULL DEFAULT 'allow_all',"+
					"`library_completion` BOOLEAN NOT NULL DEFAULT 0,"+
					"`note` VARCHAR(512) NOT NULL DEFAULT '',"+
					"`createtime` BIGINT NOT NULL DEFAULT 0,"+
					"`updatetime` BIGINT NOT NULL DEFAULT 0)", upstream, autoPK(tx)),
				// host 唯一由数据库保证：并发的两次创建都能查到「不存在」。
				fmt.Sprintf("CREATE UNIQUE INDEX `idx_%s_host` ON `%s` (`host`)", upstream, upstream),
				// key 在 MySQL 里是保留字，必须带反引号；sqlite 也认。
				fmt.Sprintf("CREATE TABLE `%s` ("+
					"`key` VARCHAR(191) NOT NULL PRIMARY KEY,"+
					"`value` TEXT,"+
					"`createtime` BIGINT NOT NULL DEFAULT 0,"+
					"`updatetime` BIGINT NOT NULL DEFAULT 0)", setting),
			}
		})
}
