package migrations

import (
	"testing"

	"github.com/go-gormigrate/gormigrate/v2"
	"github.com/smartystreets/goconvey/convey"
	"gorm.io/gorm"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
)

func TestPackageProfileMigration(t *testing.T) {
	convey.Convey("新库包含 package profile 与 rewrite generation", t, func() {
		db := openSQLite(t, &gorm.Config{})
		convey.So(RunMigrations(db), convey.ShouldBeNil)

		var column struct {
			Name       string
			NotNull    int    `gorm:"column:notnull"`
			DefaultVal string `gorm:"column:dflt_value"`
		}
		convey.So(db.Raw("SELECT name, `notnull`, dflt_value FROM pragma_table_info('upstreams') "+
			"WHERE name='package_profile'").Scan(&column).Error, convey.ShouldBeNil)
		convey.So(column.Name, convey.ShouldEqual, "package_profile")
		convey.So(column.NotNull, convey.ShouldEqual, 1)
		convey.So(column.DefaultVal, convey.ShouldEqual, "'none'")

		var generation int64
		convey.So(db.Raw("SELECT generation FROM rewrite_states WHERE id=1").Scan(&generation).Error,
			convey.ShouldBeNil)
		convey.So(generation, convey.ShouldEqual, 0)
	})

	convey.Convey("旧库升级把已有上游默认成 none", t, func() {
		db := openSQLite(t, &gorm.Config{})
		old := []*gormigrate.Migration{
			upstreamAndSetting(), cacheObject(), trafficRollup(), accessRule(), event(),
			trafficRollupMissReasons(), recentRequest(), upstreamProtocols(), gitMirror(), cacheValidator(),
		}
		convey.So(runMigrations(db, old), convey.ShouldBeNil)
		convey.So(db.Exec("INSERT INTO upstreams "+
			"(host,protocols,origin,enabled,createtime,updatetime) VALUES (?,?,?,?,?,?)",
			"legacy.example.com", `["static"]`, "https://legacy.example.com", true, 11, 12).Error,
			convey.ShouldBeNil)

		convey.So(runMigrations(db, migrationList()), convey.ShouldBeNil)
		var got upstream_entity.Upstream
		convey.So(db.Where("host=?", "legacy.example.com").First(&got).Error, convey.ShouldBeNil)
		convey.So(got.PackageProfile, convey.ShouldEqual, upstream_entity.PackageProfileNone)
		convey.So(got.Createtime, convey.ShouldEqual, 11)
		convey.So(got.Updatetime, convey.ShouldEqual, 12)
	})

	convey.Convey("package profile 紧邻 cache response metadata 之前", t, func() {
		list := migrationList()
		packageProfileIndex := -1
		cacheResponseMetadataIndex := -1
		for i, migration := range list {
			switch migration.ID {
			case packageProfile().ID:
				packageProfileIndex = i
			case cacheResponseMetadata().ID:
				cacheResponseMetadataIndex = i
			}
		}
		convey.So(packageProfileIndex, convey.ShouldNotEqual, -1)
		convey.So(cacheResponseMetadataIndex, convey.ShouldEqual, packageProfileIndex+1)
	})
}
