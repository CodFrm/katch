package migrations

import (
	"fmt"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
)

// protocolBackfill 把旧的 kind 取值映射成新的协议集合。集合里就它一个：升级前的
// 记录只可能服务一种协议，多开哪一种是升级之后由人决定的事。
var protocolBackfill = map[string]string{
	upstream_entity.ProtocolRegistry: `["` + upstream_entity.ProtocolRegistry + `"]`,
	upstream_entity.ProtocolStatic:   `["` + upstream_entity.ProtocolStatic + `"]`,
}

// upstreamProtocols 把上游的单值 kind 换成协议集合 protocols。
//
// 建列、回填、删列三步在同一条迁移里走完，不留 kind：留一个没人读的旧真相，迟早
// 有人照着它写判断，两份真相就此分叉（决策 2）。
//
// 先核对、后动结构：gormigrate 默认不开事务，中途失败会把半截 DDL 留在库里。
// 遇到第三种 kind 就在什么都还没改时停下——库被手改过时猜一个默认值，只会让一条
// registry 上游在升级之后静默地服务不了任何东西。
//
// 逐条 ALTER：MySQL 认多子句，sqlite 不认。DROP COLUMN 要 sqlite 3.35+，
// modernc.org/sqlite 满足；kind 上没有索引，否则 sqlite 会直接拒绝删列。
func upstreamProtocols() *gormigrate.Migration {
	return alterTable("20260914000001_upstream_protocols",
		&upstream_entity.Upstream{},
		func(tx *gorm.DB, table string) error {
			var kinds []string
			if err := tx.Table(table).Distinct().Pluck("kind", &kinds).Error; err != nil {
				return err
			}
			for _, kind := range kinds {
				if _, ok := protocolBackfill[kind]; !ok {
					return fmt.Errorf("migrations: upstream.kind 出现未知取值 %q，"+
						"无法回填 protocols；请先修正这一行再升级", kind)
				}
			}
			// 可空的 TEXT，与同表的 immutable_patterns 一致：MySQL 8.0.13 之前
			// 不允许 TEXT 列带 DEFAULT。
			if err := exec(tx, fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN `protocols` TEXT", table)); err != nil {
				return err
			}
			for kind, protocols := range protocolBackfill {
				if err := tx.Exec(fmt.Sprintf(
					"UPDATE `%s` SET `protocols` = ? WHERE `kind` = ?", table),
					protocols, kind).Error; err != nil {
					return err
				}
			}
			return exec(tx, fmt.Sprintf("ALTER TABLE `%s` DROP COLUMN `kind`", table))
		},
		func(tx *gorm.DB, table string) error {
			if err := exec(tx, fmt.Sprintf(
				"ALTER TABLE `%s` ADD COLUMN `kind` VARCHAR(32) NOT NULL DEFAULT ''", table)); err != nil {
				return err
			}
			// 回滚回单值只能挑一个：registry 优先，它是路径形态上唯一不可替代的
			// 那种，其余（含只开 git 的）落到 static。这是有损的。
			if err := tx.Exec(fmt.Sprintf("UPDATE `%s` SET `kind` = ?", table),
				upstream_entity.ProtocolStatic).Error; err != nil {
				return err
			}
			if err := tx.Exec(fmt.Sprintf(
				"UPDATE `%s` SET `kind` = ? WHERE `protocols` LIKE ?", table),
				upstream_entity.ProtocolRegistry,
				`%"`+upstream_entity.ProtocolRegistry+`"%`).Error; err != nil {
				return err
			}
			return exec(tx, fmt.Sprintf("ALTER TABLE `%s` DROP COLUMN `protocols`", table))
		})
}
