package migrations

import (
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/go-gormigrate/gormigrate/v2"
	"github.com/smartystreets/goconvey/convey"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"

	"github.com/CodFrm/katch/internal/model/entity/request_log_entity"
)

// 迁移自己的用例是「不连真库」那条约定的例外：DDL 要验的恰恰是方言副作用——
// 一条在 sqlmock 里跑得通的建表语句，在 sqlite 上可能根本解析不了。这里用的是
// 纯 Go 的 modernc 驱动（glebarez/sqlite），不引入 cgo。

// TestRecentRequestMigration 建出来的表要能被 repository 真的读写。
func TestRecentRequestMigration(t *testing.T) {
	convey.Convey("recent_request 迁移在 sqlite 上建得出来", t, func() {
		gormDB, err := gorm.Open(sqlite.Open("file:recent_request_main?mode=memory&cache=shared"), &gorm.Config{})
		convey.So(err, convey.ShouldBeNil)
		sqlDB, err := gormDB.DB()
		convey.So(err, convey.ShouldBeNil)
		defer func() { _ = sqlDB.Close() }()

		convey.So(RunMigrations(gormDB), convey.ShouldBeNil)

		convey.Convey("写进去的一行能原样读回来", func() {
			row := &request_log_entity.RecentRequest{
				UpstreamID: 7, At: 1700000040, Object: "library/redis", Result: "hit",
				Bytes: 1024, DurationMS: 12, Createtime: 1700000040, Updatetime: 1700000040,
			}
			convey.So(gormDB.Create(row).Error, convey.ShouldBeNil)
			convey.So(row.ID, convey.ShouldBeGreaterThan, 0)

			got := &request_log_entity.RecentRequest{}
			convey.So(gormDB.Where("id=?", row.ID).First(got).Error, convey.ShouldBeNil)
			convey.So(got.UpstreamID, convey.ShouldEqual, 7)
			convey.So(got.At, convey.ShouldEqual, 1700000040)
			convey.So(got.Object, convey.ShouldEqual, "library/redis")
			convey.So(got.Result, convey.ShouldEqual, "hit")
			convey.So(got.Bytes, convey.ShouldEqual, 1024)
			convey.So(got.DurationMS, convey.ShouldEqual, 12)
		})

		convey.Convey("两条索引都建了出来", func() {
			// 面板按上游取最近 N 条走 (upstream_id, at)，保留期裁剪走 (at)。
			// 索引名写歪了不会让功能报错，只会让这两条查询变成全表扫。
			table, err := tableName(gormDB, &request_log_entity.RecentRequest{})
			convey.So(err, convey.ShouldBeNil)
			names := make([]string, 0)
			convey.So(gormDB.
				Raw("SELECT name FROM sqlite_master WHERE type='index' AND tbl_name = ? AND name LIKE 'idx_%'", table).
				Scan(&names).Error, convey.ShouldBeNil)
			convey.So(names, convey.ShouldContain, "idx_"+table+"_upstream_at")
			convey.So(names, convey.ShouldContain, "idx_"+table+"_at")
		})

		convey.Convey("迁移可回滚，不会把别的表一起带走", func() {
			// 回滚指名这一条，而不是 RollbackLast：末尾那条是谁取决于此后还追加了
			// 什么（迁移只追加不修改），拿「最后一条」当自己那条的用例会在下一次
			// 追加迁移时失败，而失败的原因和被测的东西毫无关系。
			convey.So(gormigrate.New(gormDB, gormigrate.DefaultOptions, migrationList()).
				RollbackMigration(recentRequest()), convey.ShouldBeNil)
			convey.So(gormDB.Migrator().HasTable(&request_log_entity.RecentRequest{}), convey.ShouldBeFalse)
			// 分钟桶还在：回滚这一条不该把 90 天的流量带走。
			convey.So(gormDB.Migrator().HasTable("traffic_rollups"), convey.ShouldBeTrue)
		})
	})
}

// TestRecentRequestMigrationAppendedAtEnd 新迁移只追加不修改。
//
// 已经跑过的迁移改了不会重跑，只会让新旧环境的表结构悄悄分叉；这条用例钉的是
// 「它排在写它那一刻已有的全部迁移之后」，也就是没有被插到中间去。
//
// 原先钉的是「它在末尾」。git 上游那一轮之后又追加了两条迁移，末尾不再是它——
// 「在末尾」本就只在一条迁移是最后加的那一段时间里成立，换成「在前驱之后」才是
// 只追加这条规则本身。
func TestRecentRequestMigrationAppendedAtEnd(t *testing.T) {
	convey.Convey("recent_request 迁移排在它之前的全部迁移之后", t, func() {
		list := migrationList()
		convey.So(len(list), convey.ShouldBeGreaterThan, 0)
		index := func(id string) int {
			for i, m := range list {
				if m.ID == id {
					return i
				}
			}
			return -1
		}
		at := index(recentRequest().ID)
		convey.So(at, convey.ShouldBeGreaterThanOrEqualTo, 0)
		// trafficRollupMissReasons 是写这条迁移时的末尾。
		convey.So(at, convey.ShouldBeGreaterThan, index(trafficRollupMissReasons().ID))
	})
}

// TestRecentRequestMigrationFollowsTablePrefix 表名由实体名加 db.prefix 得到。
//
// 把表名写死就会让改过前缀的部署建出一张 repository 永远查不到的表。
func TestRecentRequestMigrationFollowsTablePrefix(t *testing.T) {
	convey.Convey("建出的表名跟随 db.prefix", t, func() {
		// 命名策略与 cago 的 db 组件一致（它给 gorm 配的就是 TablePrefix + SingularTable）。
		// 少了 SingularTable: true，这条用例建出的是 katch_recent_requests，看不见进程里
		// 真实建出的 katch_recent_request。
		gormDB, err := gorm.Open(
			sqlite.Open("file:recent_request_prefix?mode=memory&cache=shared"),
			&gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "katch_", SingularTable: true}},
		)
		convey.So(err, convey.ShouldBeNil)
		sqlDB, err := gormDB.DB()
		convey.So(err, convey.ShouldBeNil)
		defer func() { _ = sqlDB.Close() }()

		// 只跑这一条：其余迁移的表名与本次无关，跑全量只会让失败原因变模糊。
		convey.So(runMigrations(gormDB, []*gormigrate.Migration{recentRequest()}), convey.ShouldBeNil)

		table, err := tableName(gormDB, &request_log_entity.RecentRequest{})
		convey.So(err, convey.ShouldBeNil)
		convey.So(table, convey.ShouldEqual, "katch_recent_request")
		convey.So(gormDB.Migrator().HasTable(&request_log_entity.RecentRequest{}), convey.ShouldBeTrue)
		// 写死表名的话建出的会是这张，前缀是白配置的。HasTable 传字符串不套前缀，
		// 所以这一条能真的区分两种写法。
		convey.So(gormDB.Migrator().HasTable("recent_request"), convey.ShouldBeFalse)
	})
}
