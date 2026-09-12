package migrations

import (
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/go-gormigrate/gormigrate/v2"
	"github.com/smartystreets/goconvey/convey"
	"gorm.io/gorm"

	"github.com/CodFrm/katch/internal/model/entity/rule_entity"
)

// 迁移自己的用例是「不连真库」那条约定的例外：DDL 要验的恰恰是方言副作用——
// 一条在 sqlmock 里跑得通的建表语句，在 sqlite 上可能根本解析不了。这里用的是
// 纯 Go 的 modernc 驱动（glebarez/sqlite），不引入 cgo。

// TestAccessRuleMigration 建出来的表要能被 repository 真的读写。
func TestAccessRuleMigration(t *testing.T) {
	convey.Convey("access_rule 迁移在 sqlite 上建得出来", t, func() {
		gormDB, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
		convey.So(err, convey.ShouldBeNil)
		sqlDB, err := gormDB.DB()
		convey.So(err, convey.ShouldBeNil)
		defer func() { _ = sqlDB.Close() }()

		convey.So(RunMigrations(gormDB), convey.ShouldBeNil)

		convey.Convey("写进去的规则能原样读回来，upstream_id 为 0 即全局", func() {
			rule := &rule_entity.AccessRule{
				Action: rule_entity.ActionDeny, Pattern: "*:latest", Note: "禁 latest",
			}
			convey.So(gormDB.Save(rule).Error, convey.ShouldBeNil)
			convey.So(rule.ID, convey.ShouldBeGreaterThan, 0)

			got := &rule_entity.AccessRule{}
			convey.So(gormDB.Where("id=?", rule.ID).First(got).Error, convey.ShouldBeNil)
			convey.So(got.Global(), convey.ShouldBeTrue)
			convey.So(got.Pattern, convey.ShouldEqual, "*:latest")
			convey.So(got.Note, convey.ShouldEqual, "禁 latest")
		})

		convey.Convey("迁移可回滚，不会把别的表一起带走", func() {
			convey.So(gormigrate.New(gormDB, gormigrate.DefaultOptions, migrationList()).
				RollbackLast(), convey.ShouldBeNil)
			convey.So(gormDB.Migrator().HasTable(&rule_entity.AccessRule{}), convey.ShouldBeFalse)
		})
	})
}
