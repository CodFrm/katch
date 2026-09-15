package migrations

import (
	"fmt"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"

	"github.com/CodFrm/katch/internal/model/entity/request_log_entity"
)

// recentRequest 建「最近请求」明细表。
//
// 它和 traffic_rollups 问的不是同一个问题：那张表可加（每分钟一行，留 90 天），
// 这张不可加（每请求一行，保留时长由设置决定）。合成一张会让两者互相迁就。
func recentRequest() *gormigrate.Migration {
	return createTable("20260914000001_recent_request",
		[]any{&request_log_entity.RecentRequest{}},
		func(tx *gorm.DB, t []string) []string {
			table := t[0]
			return []string{
				// at 是这次拉取**结束**的时刻（秒），裁剪与排序都按它。
				// object 取 512：它不进索引，不受 InnoDB 键上限约束。
				fmt.Sprintf("CREATE TABLE `%s` ("+
					"`id` %s,"+
					"`upstream_id` BIGINT NOT NULL DEFAULT 0,"+
					"`at` BIGINT NOT NULL DEFAULT 0,"+
					"`object` VARCHAR(512) NOT NULL DEFAULT '',"+
					"`result` VARCHAR(32) NOT NULL DEFAULT '',"+
					"`bytes` BIGINT NOT NULL DEFAULT 0,"+
					"`duration_ms` BIGINT NOT NULL DEFAULT 0,"+
					"`createtime` BIGINT NOT NULL DEFAULT 0,"+
					"`updatetime` BIGINT NOT NULL DEFAULT 0)", table, autoPK(tx)),
				// 面板：WHERE upstream_id = ? ORDER BY at DESC, id DESC LIMIT ?。
				fmt.Sprintf("CREATE INDEX `idx_%s_upstream_at` ON `%s` (`upstream_id`, `at`)", table, table),
				// 保留期裁剪：WHERE at < ?，跨全部上游。
				fmt.Sprintf("CREATE INDEX `idx_%s_at` ON `%s` (`at`)", table, table),
			}
		})
}
