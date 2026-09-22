package migrations

import (
	"testing"

	"github.com/go-gormigrate/gormigrate/v2"
	"github.com/smartystreets/goconvey/convey"
	"gorm.io/gorm"
)

func TestCacheResponseMetadataMigration(t *testing.T) {
	columns := []string{
		"cache_control", "origin_date", "origin_age", "origin_expires", "vary",
		"accept_ranges", "content_disposition", "docker_content_digest", "stored_at",
		"requires_revalidation",
	}
	convey.Convey("new databases contain response metadata columns", t, func() {
		db := openSQLite(t, &gorm.Config{})
		convey.So(RunMigrations(db), convey.ShouldBeNil)
		var got []string
		convey.So(db.Raw("SELECT name FROM pragma_table_info('cache_objects')").Scan(&got).Error, convey.ShouldBeNil)
		for _, column := range columns {
			convey.So(got, convey.ShouldContain, column)
		}
	})

	convey.Convey("old cache rows retain content and receive inert metadata defaults", t, func() {
		db := openSQLite(t, &gorm.Config{})
		old := []*gormigrate.Migration{
			upstreamAndSetting(), cacheObject(), trafficRollup(), accessRule(), event(),
			trafficRollupMissReasons(), recentRequest(), upstreamProtocols(), gitMirror(), cacheValidator(),
			packageProfile(),
		}
		convey.So(runMigrations(db, old), convey.ShouldBeNil)
		convey.So(db.Exec("INSERT INTO cache_objects "+
			"(upstream_id,`key`,digest,size,content_type,immutable,pinned,expires_at,last_access_at,hit_count,createtime,updatetime,etag,last_modified) "+
			"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)", 7, "/legacy", "sha256:legacy", 12, "text/plain", true, false, 0, 3, 4, 1, 2, `\"old\"`, "Wed, 21 Oct 2015 07:28:00 GMT").Error, convey.ShouldBeNil)

		convey.So(runMigrations(db, migrationList()), convey.ShouldBeNil)
		var got struct {
			Digest               string `gorm:"column:digest"`
			ETag                 string `gorm:"column:etag"`
			StoredAt             int64  `gorm:"column:stored_at"`
			OriginAge            int64  `gorm:"column:origin_age"`
			CacheControl         string `gorm:"column:cache_control"`
			RequiresRevalidation bool   `gorm:"column:requires_revalidation"`
		}
		convey.So(db.Raw("SELECT digest,etag,stored_at,origin_age,cache_control,requires_revalidation FROM cache_objects WHERE `key`='/legacy'").Scan(&got).Error, convey.ShouldBeNil)
		convey.So(got.Digest, convey.ShouldEqual, "sha256:legacy")
		convey.So(got.ETag, convey.ShouldEqual, `\"old\"`)
		convey.So(got.StoredAt, convey.ShouldEqual, int64(0))
		convey.So(got.OriginAge, convey.ShouldEqual, int64(0))
		convey.So(got.CacheControl, convey.ShouldBeEmpty)
		convey.So(got.RequiresRevalidation, convey.ShouldBeFalse)
	})

	convey.Convey("response metadata migration remains after package profiles", t, func() {
		list := migrationList()
		packageProfileIndex := -1
		responseMetadataIndex := -1
		for i, migration := range list {
			switch migration.ID {
			case packageProfile().ID:
				packageProfileIndex = i
			case cacheResponseMetadata().ID:
				responseMetadataIndex = i
			}
		}
		convey.So(packageProfileIndex, convey.ShouldBeGreaterThanOrEqualTo, 0)
		convey.So(responseMetadataIndex, convey.ShouldBeGreaterThan, packageProfileIndex)
	})
}
