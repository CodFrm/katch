// Package migrations 保存数据库迁移。
//
// 规则：新迁移一律追加到 migrationList() 末尾，**不修改已有迁移**——已经在
// 线上跑过的迁移改了也不会重跑，只会让新旧环境的表结构悄悄分叉。需要修正时
// 追加一条补丁迁移。
package migrations

import (
	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

// migrationList 按执行顺序返回全部迁移。
func migrationList() []*gormigrate.Migration {
	return []*gormigrate.Migration{}
}

// RunMigrations 执行全部未执行的迁移。
func RunMigrations(db *gorm.DB) error {
	return runMigrations(db, migrationList())
}

// runMigrations 是 RunMigrations 的可注入形态，便于用例直接给定迁移列表。
func runMigrations(db *gorm.DB, list []*gormigrate.Migration) error {
	// gormigrate 在列表为空时返回 "No migration defined"，会让服务启动即 panic。
	// 而「还没有任何迁移」是新项目的正常状态，这里直接跳过。
	if len(list) == 0 {
		return nil
	}
	return gormigrate.New(db, gormigrate.DefaultOptions, list).Migrate()
}
