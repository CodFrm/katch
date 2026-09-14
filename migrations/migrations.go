// Package migrations 保存数据库迁移。
//
// 规则：新迁移一律追加到 migrationList() 末尾，**不修改已有迁移**——已经在
// 线上跑过的迁移改了也不会重跑，只会让新旧环境的表结构悄悄分叉。需要修正时
// 追加一条补丁迁移。
package migrations

import (
	"fmt"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"

	"github.com/CodFrm/katch/internal/model/entity/cache_entity"
	"github.com/CodFrm/katch/internal/model/entity/event_entity"
	"github.com/CodFrm/katch/internal/model/entity/request_log_entity"
	"github.com/CodFrm/katch/internal/model/entity/rollup_entity"
	"github.com/CodFrm/katch/internal/model/entity/rule_entity"
	"github.com/CodFrm/katch/internal/model/entity/setting_entity"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
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
	}
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

// tableName 取出 gorm 为该实体生成的表名（含配置里的表前缀）。
//
// 不把表名写死：表前缀来自 configs/config.yaml 的 db.prefix，写死就会让改过前缀的
// 部署建出一张 repository 永远查不到的表。代价是这条迁移的表名跟着实体的结构体名走，
// 因此**重命名这些实体必须配一条补丁迁移**，不能只改结构体。
func tableName(tx *gorm.DB, model any) (string, error) {
	stmt := &gorm.Statement{DB: tx}
	if err := stmt.Parse(model); err != nil {
		return "", err
	}
	return stmt.Table, nil
}

// upstreamAndSetting 建上游表与设置表。
//
// upstream 是代理的白名单：不在表里或 enabled 为假的主机一律 404，
// 否则 katch 就是一个任何人都能拿来当跳板的开放代理。
// setting 是运行时配置的键值表，眼下先承载管理密钥的哈希。
func upstreamAndSetting() *gormigrate.Migration {
	return &gormigrate.Migration{
		ID: "20260912000001_upstream_and_setting",
		Migrate: func(tx *gorm.DB) error {
			upstream, setting, err := tables(tx)
			if err != nil {
				return err
			}
			// 自增主键是 sqlite 和 MySQL 唯一写不到一起的地方：MySQL 要
			// AUTO_INCREMENT，sqlite 的 AUTOINCREMENT 只认 INTEGER PRIMARY KEY。
			// 其余列两种方言同形，所以只有这一处分方言。
			autoPK := "BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY"
			if tx.Name() == "sqlite" {
				autoPK = "INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT"
			}
			stmts := []string{
				fmt.Sprintf("CREATE TABLE `%s` ("+
					"`id` %s,"+
					"`host` VARCHAR(191) NOT NULL,"+
					"`kind` VARCHAR(32) NOT NULL,"+
					"`origin` VARCHAR(512) NOT NULL,"+
					"`enabled` BOOLEAN NOT NULL DEFAULT 0,"+
					"`immutable_patterns` TEXT,"+
					"`mutable_ttl_seconds` INT NOT NULL DEFAULT 0,"+
					"`default_policy` VARCHAR(32) NOT NULL DEFAULT 'allow_all',"+
					"`library_completion` BOOLEAN NOT NULL DEFAULT 0,"+
					"`note` VARCHAR(512) NOT NULL DEFAULT '',"+
					"`createtime` BIGINT NOT NULL DEFAULT 0,"+
					"`updatetime` BIGINT NOT NULL DEFAULT 0)", upstream, autoPK),
				// host 唯一由数据库保证，而不是只靠 service 里的那次查重：
				// 并发的两次创建都能查到「不存在」，唯一索引是最后一道闸。
				fmt.Sprintf("CREATE UNIQUE INDEX `idx_%s_host` ON `%s` (`host`)", upstream, upstream),
				// key 在 MySQL 里是保留字，必须带反引号；sqlite 也认反引号。
				fmt.Sprintf("CREATE TABLE `%s` ("+
					"`key` VARCHAR(191) NOT NULL PRIMARY KEY,"+
					"`value` TEXT,"+
					"`createtime` BIGINT NOT NULL DEFAULT 0,"+
					"`updatetime` BIGINT NOT NULL DEFAULT 0)", setting),
			}
			for _, stmt := range stmts {
				if err := tx.Exec(stmt).Error; err != nil {
					return err
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			upstream, setting, err := tables(tx)
			if err != nil {
				return err
			}
			for _, name := range []string{upstream, setting} {
				if err := tx.Exec(fmt.Sprintf("DROP TABLE IF EXISTS `%s`", name)).Error; err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// tables 取这条迁移涉及的两张表名。
func tables(tx *gorm.DB) (string, string, error) {
	upstream, err := tableName(tx, &upstream_entity.Upstream{})
	if err != nil {
		return "", "", err
	}
	setting, err := tableName(tx, &setting_entity.Setting{})
	if err != nil {
		return "", "", err
	}
	return upstream, setting, nil
}

// cacheObject 建缓存对象表。
//
// 表里只有「哪个上游的哪条路径对应盘上的哪一份内容」这份元数据，对象本体按内容
// 摘要存在缓存目录里。分开的理由是同一份字节可能被多条路径引用（内容寻址，
// 同一份内容只存一份），删记录时要先数一数还有没有别人引用它。
func cacheObject() *gormigrate.Migration {
	return &gormigrate.Migration{
		ID: "20260912000002_cache_object",
		Migrate: func(tx *gorm.DB) error {
			table, err := tableName(tx, &cache_entity.CacheObject{})
			if err != nil {
				return err
			}
			autoPK := "BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY"
			if tx.Name() == "sqlite" {
				autoPK = "INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT"
			}
			stmts := []string{
				// key 在 MySQL 里是保留字，建表和查询都必须带反引号；sqlite 也认。
				// 它取 500 而不是更长：下面那条唯一索引把它和 upstream_id 一起做键，
				// utf8mb4 下 500 字符是 2000 字节，加上 8 字节的 bigint 仍在 InnoDB
				// 3072 字节的索引键上限之内，再长建索引就会失败。
				fmt.Sprintf("CREATE TABLE `%s` ("+
					"`id` %s,"+
					"`upstream_id` BIGINT NOT NULL DEFAULT 0,"+
					"`key` VARCHAR(500) NOT NULL,"+
					"`digest` VARCHAR(80) NOT NULL DEFAULT '',"+
					"`size` BIGINT NOT NULL DEFAULT 0,"+
					"`content_type` VARCHAR(191) NOT NULL DEFAULT '',"+
					"`immutable` BOOLEAN NOT NULL DEFAULT 0,"+
					"`pinned` BOOLEAN NOT NULL DEFAULT 0,"+
					"`expires_at` BIGINT NOT NULL DEFAULT 0,"+
					"`last_access_at` BIGINT NOT NULL DEFAULT 0,"+
					"`hit_count` BIGINT NOT NULL DEFAULT 0,"+
					"`createtime` BIGINT NOT NULL DEFAULT 0,"+
					"`updatetime` BIGINT NOT NULL DEFAULT 0)", table, autoPK),
				// 一个上游内一条路径只能有一条记录：并发回源合并之后仍可能有两次写入
				// 落到同一个 key 上（先后两轮下载），唯一索引是最后一道闸。
				fmt.Sprintf("CREATE UNIQUE INDEX `idx_%s_upstream_key` ON `%s` (`upstream_id`, `key`)", table, table),
				// LRU 的查询条件就是这三列：不可变、未 pin、按最久未访问排序。
				fmt.Sprintf("CREATE INDEX `idx_%s_lru` ON `%s` (`immutable`, `pinned`, `last_access_at`)", table, table),
				// 删记录前要数「还有几条记录引用这份内容」，那是一次按摘要的点查。
				fmt.Sprintf("CREATE INDEX `idx_%s_digest` ON `%s` (`digest`)", table, table),
			}
			for _, stmt := range stmts {
				if err := tx.Exec(stmt).Error; err != nil {
					return err
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			table, err := tableName(tx, &cache_entity.CacheObject{})
			if err != nil {
				return err
			}
			return tx.Exec(fmt.Sprintf("DROP TABLE IF EXISTS `%s`", table)).Error
		},
	}
}

// trafficRollup 建流量分钟桶表。
//
// 界面上的请求量、命中率、省下的流量都从这张表聚合（决策 16）：每请求写一次库
// 会把拉取热路径拖进事务，而进程内计数器一重启就没了。折中是每分钟落一行，
// 保留 90 天。
func trafficRollup() *gormigrate.Migration {
	return &gormigrate.Migration{
		ID: "20260912000003_traffic_rollup",
		Migrate: func(tx *gorm.DB) error {
			table, err := tableName(tx, &rollup_entity.TrafficRollup{})
			if err != nil {
				return err
			}
			autoPK := "BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY"
			if tx.Name() == "sqlite" {
				autoPK = "INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT"
			}
			stmts := []string{
				// 没有 misses 列：未命中数是 requests 减去其余三项。多存一列，
				// 就多一处会和总数对不上的地方（可观测性一节：比值与冗余量都由
				// 读取方推导）。
				fmt.Sprintf("CREATE TABLE `%s` ("+
					"`id` %s,"+
					"`upstream_id` BIGINT NOT NULL DEFAULT 0,"+
					"`bucket` BIGINT NOT NULL DEFAULT 0,"+
					"`requests` BIGINT NOT NULL DEFAULT 0,"+
					"`hits` BIGINT NOT NULL DEFAULT 0,"+
					"`denied` BIGINT NOT NULL DEFAULT 0,"+
					"`origin_errors` BIGINT NOT NULL DEFAULT 0,"+
					"`bytes_served` BIGINT NOT NULL DEFAULT 0,"+
					"`bytes_origin` BIGINT NOT NULL DEFAULT 0,"+
					"`createtime` BIGINT NOT NULL DEFAULT 0,"+
					"`updatetime` BIGINT NOT NULL DEFAULT 0)", table, autoPK),
				// 一个上游的一整分钟只能有一行：落库是「读出来加上去再写回」，
				// 同一分钟落两次要落到同一行上，唯一索引是最后一道闸。
				fmt.Sprintf("CREATE UNIQUE INDEX `idx_%s_upstream_bucket` ON `%s` (`upstream_id`, `bucket`)",
					table, table),
				// 区间聚合与保留期裁剪都只按 bucket 过滤，跨全部上游。
				fmt.Sprintf("CREATE INDEX `idx_%s_bucket` ON `%s` (`bucket`)", table, table),
			}
			for _, stmt := range stmts {
				if err := tx.Exec(stmt).Error; err != nil {
					return err
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			table, err := tableName(tx, &rollup_entity.TrafficRollup{})
			if err != nil {
				return err
			}
			return tx.Exec(fmt.Sprintf("DROP TABLE IF EXISTS `%s`", table)).Error
		},
	}
}

// accessRule 建访问规则表。
//
// 规则分全局与上游内两层，全局先于上游内求值（决策 14）：upstream_id 为 0 就是
// 一条全局规则。用 0 而不是 NULL 表示全局——NULL 在索引、相等比较和 gorm 的
// 零值语义里各有一套分支，而「全局」是一个正常取值，不是「没有值」。
//
// 表里**没有**排序列：同层内按具体度定序，不按人工顺序（决策 15）。存一列看起来
// 像优先级的序号，只会让人以为拖动它能改变结果。上游的默认策略同样不在这里——
// 它是 upstream 上的一个字段，没有 pattern，混进规则表会让具体度排序无从谈起。
func accessRule() *gormigrate.Migration {
	return &gormigrate.Migration{
		ID: "20260912000004_access_rule",
		Migrate: func(tx *gorm.DB) error {
			table, err := tableName(tx, &rule_entity.AccessRule{})
			if err != nil {
				return err
			}
			autoPK := "BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY"
			if tx.Name() == "sqlite" {
				autoPK = "INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT"
			}
			stmts := []string{
				// pattern 取 500，和 cache_object 的 key 同一个理由：utf8mb4 下
				// 500 字符是 2000 字节，落在 InnoDB 3072 字节的索引键上限之内。
				fmt.Sprintf("CREATE TABLE `%s` ("+
					"`id` %s,"+
					"`upstream_id` BIGINT NOT NULL DEFAULT 0,"+
					"`action` VARCHAR(16) NOT NULL,"+
					"`pattern` VARCHAR(500) NOT NULL,"+
					"`note` VARCHAR(512) NOT NULL DEFAULT '',"+
					"`createtime` BIGINT NOT NULL DEFAULT 0,"+
					"`updatetime` BIGINT NOT NULL DEFAULT 0)", table, autoPK),
				// 求值时读的是整张表（规则是人工维护的策略，规模是几十条），
				// 这条索引是给管理界面按上游筛选用的。
				fmt.Sprintf("CREATE INDEX `idx_%s_upstream` ON `%s` (`upstream_id`)", table, table),
			}
			for _, stmt := range stmts {
				if err := tx.Exec(stmt).Error; err != nil {
					return err
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			table, err := tableName(tx, &rule_entity.AccessRule{})
			if err != nil {
				return err
			}
			return tx.Exec(fmt.Sprintf("DROP TABLE IF EXISTS `%s`", table)).Error
		},
	}
}

// event 建事件表。
//
// 后台概览上那条时间线：自动告警与人为变更放在一起，带操作人（「管理接口与界面」
// 一节）。kind 与 actor 存的是稳定枚举而不是给人读的句子——界面按它们查翻译表，
// 一旦落进库的是英文散文，中英双语就只剩把后端字符串原样贴给用户这一条路。
//
// 表里没有 updatetime：事件只追加，一条改得动的历史记录不再是历史。也没有
// 「草稿」「版本」这类列——规则的审批流与版本回滚在这一轮的 Out of scope 里，
// 这张表只记变更发生过。
func event() *gormigrate.Migration {
	return &gormigrate.Migration{
		ID: "20260913000001_event",
		Migrate: func(tx *gorm.DB) error {
			table, err := tableName(tx, &event_entity.Event{})
			if err != nil {
				return err
			}
			autoPK := "BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY"
			if tx.Name() == "sqlite" {
				autoPK = "INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT"
			}
			// 没有额外索引：唯一的读法是「按 id 倒序取最近 N 条」，主键自己就是
			// 那个顺序。给 createtime 建一条谁也不会走的索引，只是让每一次写入
			// 多维护一棵树——而写入发生在缓存回收、退避转换这些已经出了状况的时刻。
			stmt := fmt.Sprintf("CREATE TABLE `%s` ("+
				"`id` %s,"+
				"`kind` VARCHAR(64) NOT NULL,"+
				"`actor` VARCHAR(32) NOT NULL,"+
				"`upstream_id` BIGINT NOT NULL DEFAULT 0,"+
				"`detail` TEXT,"+
				"`createtime` BIGINT NOT NULL DEFAULT 0)", table, autoPK)
			return tx.Exec(stmt).Error
		},
		Rollback: func(tx *gorm.DB) error {
			table, err := tableName(tx, &event_entity.Event{})
			if err != nil {
				return err
			}
			return tx.Exec(fmt.Sprintf("DROP TABLE IF EXISTS `%s`", table)).Error
		},
	}
}

// trafficRollupMissReasons 给分钟桶补上四个回源原因列。
//
// 补丁迁移而不是改 20260912000003：那一条已经跑过的环境不会再跑一遍，改它只会
// 让新旧部署的表结构悄悄分叉。
//
// 四列而不是一张每请求的明细表：界面上的回源原因分解只要占比，占比可加，而
// 决策 16 否掉了每请求写库。NOT NULL DEFAULT 0 是给升级上来的老行用的——
// 没有默认值，那些行的这四列会是 NULL，扫进 int64 时直接报错，而那要等到一台
// 跑了 90 天的机器升级之后第一次打开上游详情才会发现。
//
// 逐列 ADD COLUMN 而不是一条多子句的 ALTER：MySQL 认后者，sqlite 不认。
func trafficRollupMissReasons() *gormigrate.Migration {
	columns := []string{"miss_first", "miss_ttl", "miss_evicted", "miss_changed"}
	return &gormigrate.Migration{
		ID: "20260913000002_traffic_rollup_miss_reasons",
		Migrate: func(tx *gorm.DB) error {
			table, err := tableName(tx, &rollup_entity.TrafficRollup{})
			if err != nil {
				return err
			}
			for _, column := range columns {
				if err := tx.Exec(fmt.Sprintf(
					"ALTER TABLE `%s` ADD COLUMN `%s` BIGINT NOT NULL DEFAULT 0",
					table, column)).Error; err != nil {
					return err
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			table, err := tableName(tx, &rollup_entity.TrafficRollup{})
			if err != nil {
				return err
			}
			// 只拿掉这四列：回滚一条补丁迁移不该把整张表和 90 天的流量一起带走。
			for _, column := range columns {
				if err := tx.Exec(fmt.Sprintf("ALTER TABLE `%s` DROP COLUMN `%s`",
					table, column)).Error; err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// recentRequest 建「最近请求」明细表。
//
// 它和 traffic_rollups 问的不是同一个问题：那张表可加（每分钟一行，留 90 天），
// 这张表不可加（每请求一行，保留时长由设置决定）。合成一张会让两者互相迁就。
//
// 两条索引各有唯一的走法：面板按上游取最近 N 条走 (upstream_id, at)，保留期裁剪
// 只看 at 走 (at)。没有别的索引——写入发生在每一次拉取之后，多一棵树就是每个
// 请求多付一次维护成本。
func recentRequest() *gormigrate.Migration {
	return &gormigrate.Migration{
		ID: "20260914000001_recent_request",
		Migrate: func(tx *gorm.DB) error {
			table, err := tableName(tx, &request_log_entity.RecentRequest{})
			if err != nil {
				return err
			}
			autoPK := "BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY"
			if tx.Name() == "sqlite" {
				autoPK = "INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT"
			}
			stmts := []string{
				// at 是这次拉取**结束**的时刻（秒），裁剪与排序都按它。
				// object 取 512，和 upstream.origin 同一个量级：它是上游内的路径，
				// 不进索引，所以不受 InnoDB 索引键上限的约束。
				fmt.Sprintf("CREATE TABLE `%s` ("+
					"`id` %s,"+
					"`upstream_id` BIGINT NOT NULL DEFAULT 0,"+
					"`at` BIGINT NOT NULL DEFAULT 0,"+
					"`object` VARCHAR(512) NOT NULL DEFAULT '',"+
					"`result` VARCHAR(32) NOT NULL DEFAULT '',"+
					"`bytes` BIGINT NOT NULL DEFAULT 0,"+
					"`duration_ms` BIGINT NOT NULL DEFAULT 0,"+
					"`createtime` BIGINT NOT NULL DEFAULT 0,"+
					"`updatetime` BIGINT NOT NULL DEFAULT 0)", table, autoPK),
				// 面板：WHERE upstream_id = ? ORDER BY at DESC, id DESC LIMIT ?。
				fmt.Sprintf("CREATE INDEX `idx_%s_upstream_at` ON `%s` (`upstream_id`, `at`)", table, table),
				// 保留期裁剪：WHERE at < ?。跨全部上游，单独一条。
				fmt.Sprintf("CREATE INDEX `idx_%s_at` ON `%s` (`at`)", table, table),
			}
			for _, stmt := range stmts {
				if err := tx.Exec(stmt).Error; err != nil {
					return err
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			table, err := tableName(tx, &request_log_entity.RecentRequest{})
			if err != nil {
				return err
			}
			return tx.Exec(fmt.Sprintf("DROP TABLE IF EXISTS `%s`", table)).Error
		},
	}
}
