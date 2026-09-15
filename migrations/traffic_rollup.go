package migrations

import (
	"fmt"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"

	"github.com/CodFrm/katch/internal/model/entity/rollup_entity"
)

// trafficRollup 建流量分钟桶表。
//
// 界面上的请求量、命中率、省下的流量都从这张表聚合（决策 16）：每请求写一次库
// 会把拉取热路径拖进事务，进程内计数器又一重启就没了。折中是每分钟落一行。
func trafficRollup() *gormigrate.Migration {
	return createTable("20260912000003_traffic_rollup",
		[]any{&rollup_entity.TrafficRollup{}},
		func(tx *gorm.DB, t []string) []string {
			table := t[0]
			return []string{
				// 没有 misses 列：未命中数是 requests 减去其余三项。多存一列，
				// 就多一处会和总数对不上的地方。
				fmt.Sprintf("CREATE TABLE `%s` ("+
					"`id` %s,"+
					"`upstream_id` BIGINT NOT NULL DEFAULT 0,"+
					"`bucket` BIGINT NOT NULL DEFAULT 0,"+
					"`requests` BIGINT NOT NULL DEFAULT 0,"+
					"`hits` BIGINT NOT NULL DEFAULT 0,"+
					"`denied` BIGINT NOT NULL DEFAULT 0,"+
					"`origin_errors` BIGINT NOT NULL DEFAULT 0,"+
					"`bytes_served` BIGINT NOT NULL DEFAULT 0,"+
					"`bytes_origin` BIGINT NOT NULL DEFAULT 0,"+
					"`createtime` BIGINT NOT NULL DEFAULT 0,"+
					"`updatetime` BIGINT NOT NULL DEFAULT 0)", table, autoPK(tx)),
				// 落库是「读出来加上去再写回」，同一分钟的两次写入要落到同一行。
				fmt.Sprintf("CREATE UNIQUE INDEX `idx_%s_upstream_bucket` ON `%s` (`upstream_id`, `bucket`)",
					table, table),
				// 区间聚合与保留期裁剪都只按 bucket 过滤，跨全部上游。
				fmt.Sprintf("CREATE INDEX `idx_%s_bucket` ON `%s` (`bucket`)", table, table),
			}
		})
}
