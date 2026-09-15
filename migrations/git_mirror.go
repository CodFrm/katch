package migrations

import (
	"fmt"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"

	"github.com/CodFrm/katch/internal/model/entity/git_entity"
)

// gitMirror 建 git 本地镜像表。
//
// host+repo 上一条唯一索引：并发的两次穿透会同时想登记同一个仓库，没有它，盘上
// 一份镜像会对应库里两条真相。两列都给足长度，截断会让两个仓库撞成同一行。
func gitMirror() *gormigrate.Migration {
	return createTable("20260914000002_git_mirror",
		[]any{&git_entity.GitMirror{}},
		func(tx *gorm.DB, t []string) []string {
			table := t[0]
			return []string{
				fmt.Sprintf("CREATE TABLE `%s` ("+
					"`id` %s,"+
					"`host` VARCHAR(255) NOT NULL,"+
					"`repo` VARCHAR(512) NOT NULL,"+
					"`state` VARCHAR(32) NOT NULL,"+
					"`last_sync_at` BIGINT NOT NULL DEFAULT 0,"+
					"`last_access_at` BIGINT NOT NULL DEFAULT 0,"+
					"`size_bytes` BIGINT NOT NULL DEFAULT 0,"+
					"`last_error` TEXT,"+
					"`createtime` BIGINT NOT NULL DEFAULT 0,"+
					"`updatetime` BIGINT NOT NULL DEFAULT 0)", table, autoPK(tx)),
				fmt.Sprintf("CREATE UNIQUE INDEX `uk_%s_host_repo` ON `%s` (`host`,`repo`)", table, table),
			}
		})
}
