package migrations

import (
	"testing"

	"github.com/go-gormigrate/gormigrate/v2"
	"github.com/smartystreets/goconvey/convey"
	"gorm.io/gorm"
)

func TestCacheDistributionAPIVersionMigration(t *testing.T) {
	convey.Convey("新库的 cache_objects 带 docker_distribution_api_version 列", t, func() {
		db := openSQLite(t, &gorm.Config{})
		convey.So(RunMigrations(db), convey.ShouldBeNil)
		var got []string
		convey.So(db.Raw("SELECT name FROM pragma_table_info('cache_objects')").Scan(&got).Error,
			convey.ShouldBeNil)
		convey.So(got, convey.ShouldContain, "docker_distribution_api_version")
	})

	convey.Convey("升级上来的旧记录这一列为空", t, func() {
		db := openSQLite(t, &gorm.Config{})
		old := []*gormigrate.Migration{
			upstreamAndSetting(), cacheObject(), trafficRollup(), accessRule(), event(),
			trafficRollupMissReasons(), recentRequest(), upstreamProtocols(), gitMirror(), cacheValidator(),
			packageProfile(), cacheResponseMetadata(), cacheChecksumHeaders(),
		}
		convey.So(runMigrations(db, old), convey.ShouldBeNil)
		convey.So(db.Exec("INSERT INTO cache_objects "+
			"(upstream_id,`key`,digest,size,content_type,immutable,pinned,expires_at,last_access_at,hit_count,createtime,updatetime) "+
			"VALUES (?,?,?,?,?,?,?,?,?,?,?,?)", 7, "/v2/legacy/manifests/1", "sha256:legacy", 12,
			"application/json", false, false, 0, 3, 4, 1, 2).Error, convey.ShouldBeNil)

		convey.So(runMigrations(db, migrationList()), convey.ShouldBeNil)
		var got struct {
			Digest  string `gorm:"column:digest"`
			Version string `gorm:"column:docker_distribution_api_version"`
		}
		convey.So(db.Raw("SELECT digest,docker_distribution_api_version FROM cache_objects "+
			"WHERE `key`='/v2/legacy/manifests/1'").Scan(&got).Error, convey.ShouldBeNil)
		convey.So(got.Digest, convey.ShouldEqual, "sha256:legacy")
		convey.So(got.Version, convey.ShouldBeEmpty)
	})

	convey.Convey("这条迁移排在 checksum 头之后", t, func() {
		convey.So(migrationIndex(cacheDistributionAPIVersion().ID), convey.ShouldBeGreaterThan,
			migrationIndex(cacheChecksumHeaders().ID))
	})
}

// migrationIndex 迁移在 migrationList 里的位置，找不到时为 -1。
func migrationIndex(id string) int {
	for i, migration := range migrationList() {
		if migration.ID == id {
			return i
		}
	}
	return -1
}
