package migrations

import (
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/go-gormigrate/gormigrate/v2"
	"github.com/smartystreets/goconvey/convey"
	"gorm.io/gorm"

	"github.com/CodFrm/katch/internal/model/entity/event_entity"
)

// 迁移自己的用例是「不连真库」那条约定的例外：DDL 要验的恰恰是方言副作用——
// 一条在 sqlmock 里跑得通的建表语句，在 sqlite 上可能根本解析不了。这里用的是
// 纯 Go 的 modernc 驱动（glebarez/sqlite），不引入 cgo。

// TestEventMigration 建出来的表要能被 repository 真的读写。
func TestEventMigration(t *testing.T) {
	convey.Convey("event 迁移在 sqlite 上建得出来", t, func() {
		gormDB, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
		convey.So(err, convey.ShouldBeNil)
		sqlDB, err := gormDB.DB()
		convey.So(err, convey.ShouldBeNil)
		defer func() { _ = sqlDB.Close() }()

		convey.So(RunMigrations(gormDB), convey.ShouldBeNil)

		convey.Convey("写进去的事件能原样读回来", func() {
			event := &event_entity.Event{
				Kind: event_entity.KindUpstreamDegraded, Actor: event_entity.ActorSystem,
				UpstreamID: 7, Detail: `{"host":"deb.debian.org"}`, Createtime: 1700000000,
			}
			convey.So(gormDB.Create(event).Error, convey.ShouldBeNil)
			convey.So(event.ID, convey.ShouldBeGreaterThan, 0)

			got := &event_entity.Event{}
			convey.So(gormDB.Where("id=?", event.ID).First(got).Error, convey.ShouldBeNil)
			convey.So(got.Kind, convey.ShouldEqual, event_entity.KindUpstreamDegraded)
			convey.So(got.Actor, convey.ShouldEqual, event_entity.ActorSystem)
			convey.So(got.UpstreamID, convey.ShouldEqual, 7)
			convey.So(got.Detail, convey.ShouldEqual, `{"host":"deb.debian.org"}`)
		})

		convey.Convey("与具体上游无关的事件不必给 upstream_id", func() {
			// 设置变更、密钥轮换都是这一类：列上有默认值，不写也建得起来。
			event := &event_entity.Event{
				Kind: event_entity.KindAdminKeyRotated, Actor: event_entity.ActorAdmin,
				Detail: "{}", Createtime: 1700000001,
			}
			convey.So(gormDB.Create(event).Error, convey.ShouldBeNil)
			got := &event_entity.Event{}
			convey.So(gormDB.Where("id=?", event.ID).First(got).Error, convey.ShouldBeNil)
			convey.So(got.UpstreamID, convey.ShouldEqual, 0)
		})

		convey.Convey("迁移可回滚，不会把别的表一起带走", func() {
			// 指名这一条而不是 RollbackLast：迁移只追加不修改，下一条追加进来
			// 之后「最后一条」就不再是它了。
			convey.So(gormigrate.New(gormDB, gormigrate.DefaultOptions, migrationList()).
				RollbackMigration(event()), convey.ShouldBeNil)
			convey.So(gormDB.Migrator().HasTable(&event_entity.Event{}), convey.ShouldBeFalse)
		})
	})
}
