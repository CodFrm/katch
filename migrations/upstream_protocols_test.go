package migrations

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/go-gormigrate/gormigrate/v2"
	"github.com/smartystreets/goconvey/convey"
	"gorm.io/gorm"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
)

// 这条迁移的 ID。用例指名它而不是「最后一条」：迁移只追加，拿末条当自己那条的
// 用例会在下一次追加时失败，而失败的原因和被测的东西毫无关系。
const upstreamProtocolsID = "20260914000001_upstream_protocols"

// migrationsBefore 取 id 这条之前的那一段迁移，用来造出「升级之前的那个库」。
func migrationsBefore(id string) []*gormigrate.Migration {
	list := migrationList()
	for i, m := range list {
		if m.ID == id {
			return list[:i]
		}
	}
	return list
}

// oldSchemaSeq 给每个库一个独有的名字。convey 每个叶子都会把外层重跑一遍，
// 共用一个 DSN 时第二个叶子拿到的是上一个叶子已经迁过的库。
var oldSchemaSeq atomic.Int64

// openOldSchema 开一个只跑到 protocols 之前的库，于是 upstream 表上还有 kind 列。
func openOldSchema(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:upstream_protocols_%d?mode=memory&cache=shared",
		oldSchemaSeq.Add(1))
	gormDB, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	convey.So(err, convey.ShouldBeNil)
	sqlDB, err := gormDB.DB()
	convey.So(err, convey.ShouldBeNil)
	t.Cleanup(func() { _ = sqlDB.Close() })
	convey.So(runMigrations(gormDB, migrationsBefore(upstreamProtocolsID)), convey.ShouldBeNil)
	convey.So(gormDB.Migrator().HasColumn(&upstream_entity.Upstream{}, "kind"), convey.ShouldBeTrue)
	return gormDB
}

// insertOldUpstream 按升级之前的表结构插一行，列名写死才能绕开已经换掉的实体。
func insertOldUpstream(db *gorm.DB, id int64, host, kind string) error {
	return db.Exec("INSERT INTO `upstreams` "+
		"(`id`,`host`,`kind`,`origin`,`enabled`,`immutable_patterns`,`mutable_ttl_seconds`,"+
		"`default_policy`,`library_completion`,`note`,`createtime`,`updatetime`) "+
		"VALUES (?,?,?,?,1,'[]',300,'allow_all',0,'',0,0)",
		id, host, kind, "https://"+host).Error
}

// TestUpstreamProtocolsMigration 单值的 kind 换成协议集合 protocols。
//
// 建列、回填、删列三步在同一条迁移里完成：留一个没人读的旧真相，迟早有人照着它
// 写判断，两份真相就此分叉（spec 决策 2）。
func TestUpstreamProtocolsMigration(t *testing.T) {
	convey.Convey("旧库升上来之后 kind 没了，protocols 是回填出来的那一个", t, func() {
		gormDB := openOldSchema(t)
		convey.So(insertOldUpstream(gormDB, 1, "docker.io", "registry"), convey.ShouldBeNil)
		convey.So(insertOldUpstream(gormDB, 2, "deb.debian.org", "static"), convey.ShouldBeNil)

		convey.So(RunMigrations(gormDB), convey.ShouldBeNil)

		convey.Convey("registry 回填成 [registry]，static 回填成 [static]", func() {
			registry := &upstream_entity.Upstream{}
			convey.So(gormDB.Where("id=?", 1).First(registry).Error, convey.ShouldBeNil)
			convey.So([]string(registry.Protocols), convey.ShouldResemble,
				[]string{upstream_entity.ProtocolRegistry})

			static := &upstream_entity.Upstream{}
			convey.So(gormDB.Where("id=?", 2).First(static).Error, convey.ShouldBeNil)
			convey.So([]string(static.Protocols), convey.ShouldResemble,
				[]string{upstream_entity.ProtocolStatic})
		})

		convey.Convey("kind 列已经不在表上了", func() {
			convey.So(gormDB.Migrator().HasColumn(&upstream_entity.Upstream{}, "kind"),
				convey.ShouldBeFalse)
			convey.So(gormDB.Migrator().HasColumn(&upstream_entity.Upstream{}, "protocols"),
				convey.ShouldBeTrue)
		})
	})

	convey.Convey("kind 是第三种取值时迁移报错停下，不猜一个默认值", t, func() {
		gormDB := openOldSchema(t)
		convey.So(insertOldUpstream(gormDB, 1, "docker.io", "registry"), convey.ShouldBeNil)
		// 库被手改过才会出现的取值。猜成 static 会让一条 registry 上游静默失效。
		convey.So(insertOldUpstream(gormDB, 2, "weird.example.com", "ftp"), convey.ShouldBeNil)

		err := RunMigrations(gormDB)
		convey.So(err, convey.ShouldNotBeNil)
		convey.So(err.Error(), convey.ShouldContainSubstring, "ftp")

		convey.Convey("停下时表结构原样不动，没有半途改过的痕迹", func() {
			migrator := gormDB.Migrator()
			convey.So(migrator.HasColumn(&upstream_entity.Upstream{}, "kind"), convey.ShouldBeTrue)
			convey.So(migrator.HasColumn(&upstream_entity.Upstream{}, "protocols"), convey.ShouldBeFalse)
		})
	})

	convey.Convey("迁移可回滚：kind 回来、protocols 拿掉，别的列不受影响", t, func() {
		gormDB := openOldSchema(t)
		convey.So(insertOldUpstream(gormDB, 1, "docker.io", "registry"), convey.ShouldBeNil)
		convey.So(insertOldUpstream(gormDB, 2, "deb.debian.org", "static"), convey.ShouldBeNil)
		convey.So(RunMigrations(gormDB), convey.ShouldBeNil)

		convey.So(gormigrate.New(gormDB, gormigrate.DefaultOptions, migrationList()).
			RollbackMigration(upstreamProtocols()), convey.ShouldBeNil)

		migrator := gormDB.Migrator()
		convey.So(migrator.HasColumn(&upstream_entity.Upstream{}, "kind"), convey.ShouldBeTrue)
		convey.So(migrator.HasColumn(&upstream_entity.Upstream{}, "protocols"), convey.ShouldBeFalse)

		var kinds []string
		convey.So(gormDB.Table("upstreams").Order("id").Pluck("kind", &kinds).Error,
			convey.ShouldBeNil)
		convey.So(kinds, convey.ShouldResemble, []string{"registry", "static"})
		// 回滚一条补丁迁移不该把整张表和上面的白名单一起带走。
		var hosts []string
		convey.So(gormDB.Table("upstreams").Order("id").Pluck("host", &hosts).Error,
			convey.ShouldBeNil)
		convey.So(hosts, convey.ShouldResemble, []string{"docker.io", "deb.debian.org"})
	})
}
