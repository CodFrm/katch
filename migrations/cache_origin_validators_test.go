package migrations

import (
	"testing"

	"github.com/go-gormigrate/gormigrate/v2"
	"github.com/smartystreets/goconvey/convey"
	"gorm.io/gorm"
)

func TestCacheOriginValidatorsMigration(t *testing.T) {
	convey.Convey("新库的 cache_objects 带 origin_etag 与 origin_last_modified 列", t, func() {
		db := openSQLite(t, &gorm.Config{})
		convey.So(RunMigrations(db), convey.ShouldBeNil)
		var got []string
		convey.So(db.Raw("SELECT name FROM pragma_table_info('cache_objects')").Scan(&got).Error,
			convey.ShouldBeNil)
		convey.So(got, convey.ShouldContain, "origin_etag")
		convey.So(got, convey.ShouldContain, "origin_last_modified")
	})

	convey.Convey("升级上来的旧记录两列为空，不从 etag / last_modified 回填", t, func() {
		db := openSQLite(t, &gorm.Config{})
		old := []*gormigrate.Migration{
			upstreamAndSetting(), cacheObject(), trafficRollup(), accessRule(), event(),
			trafficRollupMissReasons(), recentRequest(), upstreamProtocols(), gitMirror(), cacheValidator(),
			packageProfile(), cacheResponseMetadata(), cacheChecksumHeaders(), cacheDistributionAPIVersion(),
		}
		convey.So(runMigrations(db, old), convey.ShouldBeNil)
		convey.So(db.Exec("INSERT INTO cache_objects "+
			"(upstream_id,`key`,digest,size,content_type,immutable,pinned,expires_at,last_access_at,hit_count,createtime,updatetime,etag,last_modified) "+
			"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)", 7, "/metadata", "sha256:legacy", 12,
			"application/json", false, false, 1, 3, 4, 1, 2, `"sha256:katch"`,
			"Wed, 21 Oct 2015 07:28:00 GMT").Error, convey.ShouldBeNil)

		convey.So(runMigrations(db, migrationList()), convey.ShouldBeNil)
		var got struct {
			ETag               string `gorm:"column:etag"`
			OriginETag         string `gorm:"column:origin_etag"`
			OriginLastModified string `gorm:"column:origin_last_modified"`
		}
		convey.So(db.Raw("SELECT etag,origin_etag,origin_last_modified FROM cache_objects "+
			"WHERE `key`='/metadata'").Scan(&got).Error, convey.ShouldBeNil)
		convey.So(got.ETag, convey.ShouldEqual, `"sha256:katch"`)
		convey.So(got.OriginETag, convey.ShouldBeEmpty)
		convey.So(got.OriginLastModified, convey.ShouldBeEmpty)
	})

	convey.Convey("这条迁移排在 Docker-Distribution-Api-Version 之后", t, func() {
		convey.So(migrationIndex(cacheOriginValidators().ID), convey.ShouldBeGreaterThan,
			migrationIndex(cacheDistributionAPIVersion().ID))
	})
}
