package migrations

import (
	"fmt"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"

	"github.com/CodFrm/katch/internal/model/entity/rollup_entity"
)

// missReasonColumns 是本条迁移补上的四列。
var missReasonColumns = []string{"miss_first", "miss_ttl", "miss_evicted", "miss_changed"}

// trafficRollupMissReasons 给分钟桶补上四个回源原因列。
//
// 补丁迁移而不是改 20260912000003：改已跑过的迁移只会让新旧部署的表结构分叉。
//
// 四列而不是一张每请求的明细表：界面只要占比，占比可加，而决策 16 否掉了每请求
// 写库。NOT NULL DEFAULT 0 是给升级上来的老行用的——没有默认值那四列会是 NULL，
// 扫进 int64 直接报错，而那要等一台跑了 90 天的机器升级后打开详情才会发现。
//
// 逐列 ADD COLUMN 而不是一条多子句的 ALTER：MySQL 认后者，sqlite 不认。
func trafficRollupMissReasons() *gormigrate.Migration {
	return alterTable("20260913000002_traffic_rollup_miss_reasons",
		&rollup_entity.TrafficRollup{},
		func(tx *gorm.DB, table string) error {
			stmts := make([]string, len(missReasonColumns))
			for i, column := range missReasonColumns {
				stmts[i] = fmt.Sprintf(
					"ALTER TABLE `%s` ADD COLUMN `%s` BIGINT NOT NULL DEFAULT 0", table, column)
			}
			return exec(tx, stmts...)
		},
		func(tx *gorm.DB, table string) error {
			// 只拿掉这四列：回滚一条补丁迁移不该把整张表和 90 天的流量带走。
			stmts := make([]string, len(missReasonColumns))
			for i, column := range missReasonColumns {
				stmts[i] = fmt.Sprintf("ALTER TABLE `%s` DROP COLUMN `%s`", table, column)
			}
			return exec(tx, stmts...)
		})
}
