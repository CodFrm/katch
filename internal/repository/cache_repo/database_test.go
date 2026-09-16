package cache_repo

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/cago-frame/cago/database/db"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// database 基于 sqlmock 的 gorm，命名策略与运行时 cago 的 db 组件一致：
// TablePrefix（取 configs/config.yaml.example 的 db.prefix）加 SingularTable。
//
// 不用 testutils.Database：它的 gorm 没配命名策略，表名是默认的复数 cache_objects。
// 手写表名的 SQL 在那里和 gorm 生成的表名碰巧一致，断言照样通过；真库里那张表
// 叫 katch_cache_object，同一条 SQL 一跑就是 no such table。
func database(t *testing.T) (context.Context, *gorm.DB, sqlmock.Sqlmock) {
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	gormDB, err := gorm.Open(mysql.New(mysql.Config{
		SkipInitializeWithVersion: true,
		Conn:                      sqlDB,
	}), &gorm.Config{
		NamingStrategy: schema.NamingStrategy{TablePrefix: "katch_", SingularTable: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return db.WithContextDB(context.Background(), gormDB), gormDB, mock
}
