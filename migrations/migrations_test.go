package migrations

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/glebarez/sqlite"
	"github.com/smartystreets/goconvey/convey"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
)

// 只有这个包里的工具方法有用例。建表语句本身不单测：那只会把 DDL 抄第二遍，
// 而它真的建不出来在 make smoke 里就会当场暴露（真二进制启动即跑全部迁移）。

// openSQLite 开一张内存库。用纯 Go 的 modernc 驱动（glebarez/sqlite），不引入 cgo。
func openSQLite(t *testing.T, cfg *gorm.Config) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), cfg)
	convey.So(err, convey.ShouldBeNil)
	sqlDB, err := db.DB()
	convey.So(err, convey.ShouldBeNil)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// openMySQL 开一个 MySQL 方言的连接。走 sqlmock，不连真库：这里只问方言是什么，
// 不发任何语句。
func openMySQL(t *testing.T) *gorm.DB {
	t.Helper()
	conn, _, err := sqlmock.New()
	convey.So(err, convey.ShouldBeNil)
	t.Cleanup(func() { _ = conn.Close() })
	db, err := gorm.Open(mysql.New(mysql.Config{Conn: conn, SkipInitializeWithVersion: true}),
		&gorm.Config{})
	convey.So(err, convey.ShouldBeNil)
	return db
}

// TestRunMigrationsEmptyList
//
// gormigrate 在迁移列表为空时返回 "No migration defined"。而「还没有任何迁移」
// 是新项目的正常状态，不是错误——放任它冒出去会让服务在启动时直接 panic。
func TestRunMigrationsEmptyList(t *testing.T) {
	convey.Convey("迁移列表为空时不报错", t, func() {
		convey.So(runMigrations(openSQLite(t, &gorm.Config{}), nil), convey.ShouldBeNil)
	})
}

// TestTableName 表名由实体名加 db.prefix 得到。
//
// 把表名写死就会让改过前缀的部署建出一张 repository 永远查不到的表。
func TestTableName(t *testing.T) {
	convey.Convey("表名跟随 db.prefix", t, func() {
		// 命名策略与 cago 的 db 组件一致（它给 gorm 配的就是 TablePrefix + SingularTable）。
		// 少了 SingularTable: true，这里得到的是 katch_upstreams，看不见进程里真实的
		// katch_upstream。
		db := openSQLite(t, &gorm.Config{
			NamingStrategy: schema.NamingStrategy{TablePrefix: "katch_", SingularTable: true},
		})
		name, err := tableName(db, &upstream_entity.Upstream{})
		convey.So(err, convey.ShouldBeNil)
		convey.So(name, convey.ShouldEqual, "katch_upstream")
	})

	convey.Convey("没有前缀时就是实体名的复数", t, func() {
		name, err := tableName(openSQLite(t, &gorm.Config{}), &upstream_entity.Upstream{})
		convey.So(err, convey.ShouldBeNil)
		convey.So(name, convey.ShouldEqual, "upstreams")
	})

	convey.Convey("解析不了的东西要报错，不能返回空表名", t, func() {
		// 空表名会拼出 CREATE TABLE ``，那是一条能跑到数据库才失败的语句。
		name, err := tableName(openSQLite(t, &gorm.Config{}), "not a struct")
		convey.So(err, convey.ShouldNotBeNil)
		convey.So(name, convey.ShouldBeEmpty)
	})
}

// TestAutoPK 自增主键是 sqlite 和 MySQL 唯一写不到一起的地方。
//
// 两种方言各钉一次：写反了不会有编译错误，只会让其中一种数据库上的建表语句
// 解析不了，而两种数据库都是这个项目支持的部署形态。
func TestAutoPK(t *testing.T) {
	convey.Convey("sqlite 上是 INTEGER PRIMARY KEY AUTOINCREMENT", t, func() {
		// sqlite 的 AUTOINCREMENT 只认 INTEGER PRIMARY KEY，写成 BIGINT 就不是自增列。
		convey.So(autoPK(openSQLite(t, &gorm.Config{})), convey.ShouldEqual,
			"INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT")
	})

	convey.Convey("MySQL 上是 BIGINT AUTO_INCREMENT", t, func() {
		convey.So(autoPK(openMySQL(t)), convey.ShouldEqual,
			"BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY")
	})
}
