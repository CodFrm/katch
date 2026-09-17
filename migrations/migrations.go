// Package migrations 保存数据库迁移。
//
// 一条迁移一个文件，文件名跟着迁移函数走（accessRule → access_rule.go）。
// migrationList() 是执行顺序的唯一真相。
//
// 新迁移只追加到 migrationList() 末尾，不改已有的：跑过的迁移改了不会重跑，
// 只会让新旧环境的表结构悄悄分叉。要修正就追加一条补丁迁移。
package migrations

import (
	"fmt"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

// migrationList 按执行顺序返回全部迁移。
func migrationList() []*gormigrate.Migration {
	return []*gormigrate.Migration{
		upstreamAndSetting(),
		cacheObject(),
		trafficRollup(),
		accessRule(),
		event(),
		trafficRollupMissReasons(),
		recentRequest(),
		upstreamProtocols(),
		gitMirror(),
		cacheValidator(),
		packageProfile(),
		cacheResponseMetadata(),
		cacheChecksumHeaders(),
	}
}

// RunMigrations 执行全部未执行的迁移。
func RunMigrations(db *gorm.DB) error {
	return runMigrations(db, migrationList())
}

// runMigrations 是 RunMigrations 的可注入形态，便于用例直接给定迁移列表。
func runMigrations(db *gorm.DB, list []*gormigrate.Migration) error {
	// gormigrate 在列表为空时返回 "No migration defined"，会让服务启动即 panic。
	// 而「还没有任何迁移」是新项目的正常状态。
	if len(list) == 0 {
		return nil
	}
	return gormigrate.New(db, gormigrate.DefaultOptions, list).Migrate()
}

// tableName 取 gorm 为该实体生成的表名（含 configs/config.yaml 里的 db.prefix）。
//
// 不写死表名：写死会让改过前缀的部署建出一张 repository 永远查不到的表。
// 代价是表名跟着结构体名走，重命名实体必须配一条补丁迁移。
func tableName(tx *gorm.DB, model any) (string, error) {
	stmt := &gorm.Statement{DB: tx}
	if err := stmt.Parse(model); err != nil {
		return "", err
	}
	return stmt.Table, nil
}

// autoPK 返回自增主键列的类型声明。
//
// 自增主键是 sqlite 和 MySQL 唯一写不到一起的地方，其余列两种方言同形。
func autoPK(tx *gorm.DB) string {
	if tx.Name() == "sqlite" {
		return "INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT"
	}
	return "BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY"
}

// exec 依次执行 stmts，遇错即停。
func exec(tx *gorm.DB, stmts ...string) error {
	for _, stmt := range stmts {
		if err := tx.Exec(stmt).Error; err != nil {
			return err
		}
	}
	return nil
}

// createTable 建表类迁移的骨架：Migrate 跑 ddl 给出的语句，Rollback 删掉这些表。
// ddl 拿到的 tables 与 models 一一对应。
func createTable(id string, models []any,
	ddl func(tx *gorm.DB, tables []string) []string) *gormigrate.Migration {
	resolve := func(tx *gorm.DB) ([]string, error) {
		names := make([]string, len(models))
		for i, model := range models {
			name, err := tableName(tx, model)
			if err != nil {
				return nil, err
			}
			names[i] = name
		}
		return names, nil
	}
	return &gormigrate.Migration{
		ID: id,
		Migrate: func(tx *gorm.DB) error {
			names, err := resolve(tx)
			if err != nil {
				return err
			}
			return exec(tx, ddl(tx, names)...)
		},
		Rollback: func(tx *gorm.DB) error {
			names, err := resolve(tx)
			if err != nil {
				return err
			}
			stmts := make([]string, len(names))
			for i, name := range names {
				stmts[i] = fmt.Sprintf("DROP TABLE IF EXISTS `%s`", name)
			}
			return exec(tx, stmts...)
		},
	}
}

// alterTable 改表类迁移的骨架：Migrate 与 Rollback 都先解析出表名再交给回调。
func alterTable(id string, model any,
	migrate, rollback func(tx *gorm.DB, table string) error) *gormigrate.Migration {
	with := func(fn func(tx *gorm.DB, table string) error) func(*gorm.DB) error {
		return func(tx *gorm.DB) error {
			table, err := tableName(tx, model)
			if err != nil {
				return err
			}
			return fn(tx, table)
		}
	}
	return &gormigrate.Migration{ID: id, Migrate: with(migrate), Rollback: with(rollback)}
}
