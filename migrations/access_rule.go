package migrations

import (
	"fmt"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"

	"github.com/CodFrm/katch/internal/model/entity/rule_entity"
)

// accessRule 建访问规则表。
//
// 规则分全局与上游内两层，全局先于上游内求值（决策 14）：upstream_id 为 0 即全局。
// 用 0 而不是 NULL——「全局」是一个正常取值，不是「没有值」。
//
// 没有排序列：同层内按具体度定序，不按人工顺序（决策 15）。上游的默认策略也不在
// 这里，它是 upstream 上的字段，没有 pattern，混进来具体度排序就无从谈起。
func accessRule() *gormigrate.Migration {
	return createTable("20260912000004_access_rule",
		[]any{&rule_entity.AccessRule{}},
		func(tx *gorm.DB, t []string) []string {
			table := t[0]
			return []string{
				// pattern 取 500，同 cache_object 的 key：InnoDB 索引键上限。
				fmt.Sprintf("CREATE TABLE `%s` ("+
					"`id` %s,"+
					"`upstream_id` BIGINT NOT NULL DEFAULT 0,"+
					"`action` VARCHAR(16) NOT NULL,"+
					"`pattern` VARCHAR(500) NOT NULL,"+
					"`note` VARCHAR(512) NOT NULL DEFAULT '',"+
					"`createtime` BIGINT NOT NULL DEFAULT 0,"+
					"`updatetime` BIGINT NOT NULL DEFAULT 0)", table, autoPK(tx)),
				// 求值时读整张表（规则是人工维护的策略，规模几十条），
				// 这条索引是给管理界面按上游筛选用的。
				fmt.Sprintf("CREATE INDEX `idx_%s_upstream` ON `%s` (`upstream_id`)", table, table),
			}
		})
}
