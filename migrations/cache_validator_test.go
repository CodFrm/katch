package migrations

import (
	"testing"

	"github.com/go-gormigrate/gormigrate/v2"
	"github.com/smartystreets/goconvey/convey"
	"gorm.io/gorm"
)

// TestCacheValidatorMigration 缓存记录新增上游 validator 两列的迁移。
//
// 新旧库都要拿到这两列：新库跑完整列表就有，旧库升级之后补出来。已有缓存行的
// 内容、键、TTL、immutable 与访问统计一个都不许变，validator 留空——历史静态
// 缓存不补造上游校验符（决策 5），上游的 ETag 与缓存摘要未必是同一个标识。
func TestCacheValidatorMigration(t *testing.T) {
	convey.Convey("新库跑完整迁移列表就有 etag 与 last_modified", t, func() {
		db := openSQLite(t, &gorm.Config{})
		convey.So(RunMigrations(db), convey.ShouldBeNil)

		var cols []string
		convey.So(db.Raw("SELECT name FROM pragma_table_info('cache_objects')").
			Scan(&cols).Error, convey.ShouldBeNil)
		convey.So(cols, convey.ShouldContain, "etag")
		convey.So(cols, convey.ShouldContain, "last_modified")
	})

	convey.Convey("旧库升级：已有缓存行原样保留，validator 为空", t, func() {
		db := openSQLite(t, &gorm.Config{})
		// 升级前的库：只有建表那两条迁移。
		convey.So(runMigrations(db, []*gormigrate.Migration{
			upstreamAndSetting(), cacheObject(),
		}), convey.ShouldBeNil)
		res := db.Exec("INSERT INTO `cache_objects` "+
			"(`upstream_id`,`key`,`digest`,`size`,`content_type`,`immutable`,`pinned`,"+
			"`expires_at`,`last_access_at`,`hit_count`,`createtime`,`updatetime`) "+
			"VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
			7, "/pool/history.deb", "sha256:history", 42, "application/octet-stream",
			true, true, 0, 1234, 5, 111, 222)
		convey.So(res.Error, convey.ShouldBeNil)

		// 把剩下未跑的迁移全部跑完，其中就包含这条新增的。
		convey.So(runMigrations(db, migrationList()), convey.ShouldBeNil)

		var got struct {
			Key          string
			Digest       string
			Size         int64
			ContentType  string
			Immutable    bool
			Pinned       bool
			ExpiresAt    int64
			LastAccessAt int64
			HitCount     int64
			Createtime   int64
			Updatetime   int64
			ETag         string
			LastModified string
		}
		convey.So(db.Raw("SELECT `key`, `digest`, `size`, `content_type`, `immutable`,"+
			"`pinned`, `expires_at`, `last_access_at`, `hit_count`, `createtime`,"+
			"`updatetime`, `etag`, `last_modified` FROM `cache_objects` "+
			"WHERE `key`=?", "/pool/history.deb").Scan(&got).Error, convey.ShouldBeNil)

		convey.So(got.Key, convey.ShouldEqual, "/pool/history.deb")
		convey.So(got.Digest, convey.ShouldEqual, "sha256:history")
		convey.So(got.Size, convey.ShouldEqual, 42)
		convey.So(got.ContentType, convey.ShouldEqual, "application/octet-stream")
		convey.So(got.Immutable, convey.ShouldBeTrue)
		convey.So(got.Pinned, convey.ShouldBeTrue)
		convey.So(got.ExpiresAt, convey.ShouldEqual, 0)
		convey.So(got.LastAccessAt, convey.ShouldEqual, 1234)
		convey.So(got.HitCount, convey.ShouldEqual, 5)
		convey.So(got.Createtime, convey.ShouldEqual, 111)
		convey.So(got.Updatetime, convey.ShouldEqual, 222)
		convey.So(got.ETag, convey.ShouldBeEmpty)
		convey.So(got.LastModified, convey.ShouldBeEmpty)
	})

	convey.Convey("新迁移只追加在列表末尾", t, func() {
		list := migrationList()
		convey.So(list[len(list)-1].ID, convey.ShouldEqual, cacheValidator().ID)
	})
}
