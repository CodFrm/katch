package migrations

import (
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/go-gormigrate/gormigrate/v2"
	"github.com/smartystreets/goconvey/convey"
	"gorm.io/gorm"

	"github.com/CodFrm/katch/internal/model/entity/rollup_entity"
)

// TestTrafficRollupMissReasonsMigration 分钟桶上的四个回源原因列。
//
// 追加一条补丁迁移而不是改 20260912000003：那一条已经跑过的环境不会再跑一遍，
// 改它只会让新旧部署的表结构悄悄分叉。
func TestTrafficRollupMissReasonsMigration(t *testing.T) {
	convey.Convey("回源原因四列在 sqlite 上加得出来", t, func() {
		gormDB, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
		convey.So(err, convey.ShouldBeNil)
		sqlDB, err := gormDB.DB()
		convey.So(err, convey.ShouldBeNil)
		defer func() { _ = sqlDB.Close() }()

		convey.So(RunMigrations(gormDB), convey.ShouldBeNil)

		convey.Convey("四个原因写得进去也读得回来", func() {
			row := &rollup_entity.TrafficRollup{
				UpstreamID: 7, Bucket: 1700000040, Requests: 10, Hits: 4,
				MissFirst: 2, MissTTL: 2, MissEvicted: 1, MissChanged: 1,
				Createtime: 1700000040, Updatetime: 1700000040,
			}
			convey.So(gormDB.Create(row).Error, convey.ShouldBeNil)

			got := &rollup_entity.TrafficRollup{}
			convey.So(gormDB.Where("id=?", row.ID).First(got).Error, convey.ShouldBeNil)
			convey.So(got.MissFirst, convey.ShouldEqual, 2)
			convey.So(got.MissTTL, convey.ShouldEqual, 2)
			convey.So(got.MissEvicted, convey.ShouldEqual, 1)
			convey.So(got.MissChanged, convey.ShouldEqual, 1)
		})

		convey.Convey("升级上来的老行读出来是零而不是 NULL", func() {
			// 这条 INSERT 模拟的就是补丁迁移之前落下的行：不给这四列赋值。
			// 列上没有 NOT NULL DEFAULT 0 的话，扫进 int64 会直接报错，
			// 而那是一台跑了 90 天的机器升级之后第一次打开界面才会发现的。
			convey.So(gormDB.Exec("INSERT INTO `traffic_rollups` "+
				"(`upstream_id`,`bucket`,`requests`,`hits`,`denied`,`origin_errors`,"+
				"`bytes_served`,`bytes_origin`,`createtime`,`updatetime`) "+
				"VALUES (8,1700000100,5,5,0,0,10,0,1700000100,1700000100)").Error,
				convey.ShouldBeNil)

			got := &rollup_entity.TrafficRollup{}
			convey.So(gormDB.Where("upstream_id=? AND bucket=?", 8, 1700000100).
				First(got).Error, convey.ShouldBeNil)
			convey.So(got.MissFirst, convey.ShouldEqual, 0)
			convey.So(got.MissTTL, convey.ShouldEqual, 0)
			convey.So(got.MissEvicted, convey.ShouldEqual, 0)
			convey.So(got.MissChanged, convey.ShouldEqual, 0)
		})

		convey.Convey("迁移可回滚，只拿掉这四列", func() {
			convey.So(gormigrate.New(gormDB, gormigrate.DefaultOptions, migrationList()).
				RollbackMigration(trafficRollupMissReasons()), convey.ShouldBeNil)
			migrator := gormDB.Migrator()
			convey.So(migrator.HasColumn(&rollup_entity.TrafficRollup{}, "miss_first"), convey.ShouldBeFalse)
			convey.So(migrator.HasColumn(&rollup_entity.TrafficRollup{}, "miss_changed"), convey.ShouldBeFalse)
			// 表和原有的列都还在：回滚一条补丁迁移不该把整张表带走。
			convey.So(migrator.HasTable(&rollup_entity.TrafficRollup{}), convey.ShouldBeTrue)
			convey.So(migrator.HasColumn(&rollup_entity.TrafficRollup{}, "requests"), convey.ShouldBeTrue)
		})
	})
}
