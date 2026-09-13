package admin

import (
	"encoding/json"

	"github.com/cago-frame/cago/server/mux"
)

// EventItem 时间线上的一条事件。
//
// Kind 与 Actor 是稳定枚举而不是给人读的句子：界面按它们查翻译表，中英两套文案
// 由界面自己组织（「界面遵循」那一段：后端返回的英文串不直接贴给用户）。
type EventItem struct {
	ID int64 `json:"id"`
	// Kind 事件类别，取值见 event_entity 的 Kind* 常量。
	Kind string `json:"kind"`
	// Actor 操作人，admin 是带着管理密钥的人，system 是 katch 自己。
	Actor string `json:"actor"`
	// UpstreamID 这条事件属于哪个上游，0 表示与具体上游无关。
	UpstreamID int64 `json:"upstream_id"`
	// Detail 「改了什么」的结构化记录，一个 JSON 对象。字段随 Kind 而定。
	Detail json.RawMessage `json:"detail"`
	// Createtime 事件发生的时刻（秒）。
	Createtime int64 `json:"createtime"`
}

// ListEventsRequest 读出最近的事件，新的在前。
//
// 只有 limit，没有分页与筛选：概览上的那条时间线要的是「最近发生了什么」，
// 翻到第 20 页的事件属于事后追查，这一轮不做（Out of scope：事件流只记变更）。
type ListEventsRequest struct {
	mux.Meta `path:"/admin/events" method:"GET"`
	// Limit 最多给几条，留空按默认值，上限挡着一次把整张表捞回来。
	Limit int `form:"limit" binding:"omitempty,gte=1,lte=200" label:"条数"`
}

// ListEventsResponse 事件列表，按时间倒序。
type ListEventsResponse struct {
	List []*EventItem `json:"list"`
}
