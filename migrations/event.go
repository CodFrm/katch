package migrations

import (
	"fmt"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"

	"github.com/CodFrm/katch/internal/model/entity/event_entity"
)

// event 建事件表。
//
// 后台概览上那条时间线，自动告警与人为变更放在一起。kind 与 actor 存稳定枚举而不是
// 给人读的句子：界面按它们查翻译表，落进库的要是英文散文，双语就只剩原样贴给用户。
//
// 没有 updatetime：事件只追加，改得动的历史不再是历史。也没有额外索引——唯一的读法
// 是「按 id 倒序取最近 N 条」，主键自己就是那个顺序。
func event() *gormigrate.Migration {
	return createTable("20260913000001_event",
		[]any{&event_entity.Event{}},
		func(tx *gorm.DB, t []string) []string {
			return []string{
				fmt.Sprintf("CREATE TABLE `%s` ("+
					"`id` %s,"+
					"`kind` VARCHAR(64) NOT NULL,"+
					"`actor` VARCHAR(32) NOT NULL,"+
					"`upstream_id` BIGINT NOT NULL DEFAULT 0,"+
					"`detail` TEXT,"+
					"`createtime` BIGINT NOT NULL DEFAULT 0)", t[0], autoPK(tx)),
			}
		})
}
