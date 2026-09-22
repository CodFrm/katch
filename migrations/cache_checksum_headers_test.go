package migrations

import (
	"testing"

	"github.com/go-gormigrate/gormigrate/v2"
	"github.com/smartystreets/goconvey/convey"
	"gorm.io/gorm"
)

func TestCacheChecksumHeadersMigration(t *testing.T) {
	columns := []string{
		"x_checksum_md5", "x_checksum_sha1", "x_checksum_sha256", "x_checksum_sha512",
	}
	convey.Convey("new databases contain Maven checksum response columns", t, func() {
		db := openSQLite(t, &gorm.Config{})
		convey.So(RunMigrations(db), convey.ShouldBeNil)
		var got []string
		convey.So(db.Raw("SELECT name FROM pragma_table_info('cache_objects')").Scan(&got).Error,
			convey.ShouldBeNil)
		for _, column := range columns {
			convey.So(got, convey.ShouldContain, column)
		}
	})

	convey.Convey("upgraded cache rows receive empty checksum metadata", t, func() {
		db := openSQLite(t, &gorm.Config{})
		old := []*gormigrate.Migration{
			upstreamAndSetting(), cacheObject(), trafficRollup(), accessRule(), event(),
			trafficRollupMissReasons(), recentRequest(), upstreamProtocols(), gitMirror(), cacheValidator(),
			packageProfile(), cacheResponseMetadata(),
		}
		convey.So(runMigrations(db, old), convey.ShouldBeNil)
		convey.So(db.Exec("INSERT INTO cache_objects "+
			"(upstream_id,`key`,digest,size,content_type,immutable,pinned,expires_at,last_access_at,hit_count,createtime,updatetime,etag,last_modified) "+
			"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)", 7, "/legacy.jar", "sha256:legacy", 12,
			"application/java-archive", true, false, 0, 3, 4, 1, 2, `\"old\"`,
			"Wed, 21 Oct 2015 07:28:00 GMT").Error, convey.ShouldBeNil)

		convey.So(runMigrations(db, migrationList()), convey.ShouldBeNil)
		var got struct {
			Digest         string `gorm:"column:digest"`
			ChecksumMD5    string `gorm:"column:x_checksum_md5"`
			ChecksumSHA1   string `gorm:"column:x_checksum_sha1"`
			ChecksumSHA256 string `gorm:"column:x_checksum_sha256"`
			ChecksumSHA512 string `gorm:"column:x_checksum_sha512"`
		}
		convey.So(db.Raw("SELECT digest,x_checksum_md5,x_checksum_sha1,x_checksum_sha256,"+
			"x_checksum_sha512 FROM cache_objects WHERE `key`='/legacy.jar'").Scan(&got).Error,
			convey.ShouldBeNil)
		convey.So(got.Digest, convey.ShouldEqual, "sha256:legacy")
		convey.So(got.ChecksumMD5, convey.ShouldBeEmpty)
		convey.So(got.ChecksumSHA1, convey.ShouldBeEmpty)
		convey.So(got.ChecksumSHA256, convey.ShouldBeEmpty)
		convey.So(got.ChecksumSHA512, convey.ShouldBeEmpty)
	})

	convey.Convey("checksum metadata migration remains after response metadata", t, func() {
		list := migrationList()
		responseMetadataIndex := -1
		checksumHeadersIndex := -1
		for i, migration := range list {
			switch migration.ID {
			case cacheResponseMetadata().ID:
				responseMetadataIndex = i
			case cacheChecksumHeaders().ID:
				checksumHeadersIndex = i
			}
		}
		convey.So(responseMetadataIndex, convey.ShouldBeGreaterThanOrEqualTo, 0)
		convey.So(checksumHeadersIndex, convey.ShouldBeGreaterThan, responseMetadataIndex)
	})
}
