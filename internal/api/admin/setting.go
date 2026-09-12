package admin

import (
	"encoding/json"

	"github.com/cago-frame/cago/server/mux"
)

// SettingValueType 设置项的值类型，界面按它选控件。
type SettingValueType string

const (
	// SettingTypeBool 值是 JSON 的 true / false。
	SettingTypeBool SettingValueType = "bool"
	// SettingTypeInt 值是 JSON 整数。
	SettingTypeInt SettingValueType = "int"
	// SettingTypeString 值是 JSON 字符串。
	SettingTypeString SettingValueType = "string"
)

// SettingItem 一项运行时设置。
//
// 这是键值形态而不是一个字段固定的结构体，因为设置项本来就是一张键值表
// （「数据模型」一节）；更要紧的是，键值形态才谈得上「不认识的键当场拒绝」——
// 固定结构体遇到没见过的字段只会安静地忽略掉。
type SettingItem struct {
	Key string `json:"key"`
	// Value 归一化之后的 JSON 值：库里存着的如果是一份读不懂的内容，
	// 这里给的是默认值，而不是把坏数据原样吐出来（那会让整个响应不是合法 JSON）。
	Value json.RawMessage  `json:"value"`
	Type  SettingValueType `json:"type"`
}

// ListSettingsRequest 读出全部运行时设置。
type ListSettingsRequest struct {
	mux.Meta `path:"/admin/settings" method:"GET"`
}

// ListSettingsResponse 全部运行时设置，库里没写过的项给默认值。
//
// 只列「进程跑起来之后才生效」的那些（决策 4）：监听地址、数据库 DSN、缓存目录
// 这些进程起不来就改不了的东西留在 config.yaml 里，不在这里。管理密钥的哈希也
// 不在这里——它虽然存在同一张表上，但读出来对界面毫无用处，只是多一处泄漏面。
type ListSettingsResponse struct {
	List []*SettingItem `json:"list"`
}

// SaveSettingsRequest 写入若干运行时设置，只写给出的那些键。
type SaveSettingsRequest struct {
	mux.Meta `path:"/admin/settings" method:"POST"`
	// Settings 键 → JSON 值。不认识的键、类型不对或超出取值范围的值都会被
	// 整体拒绝，而不是挑能写的写进去——写了一半的设置组合是谁也没要求过的状态。
	Settings map[string]json.RawMessage `json:"settings" binding:"required,min=1" label:"设置项"`
}

// SaveSettingsResponse 写入成功后回一份写完的全量设置，省得界面再读一次。
type SaveSettingsResponse struct {
	List []*SettingItem `json:"list"`
}

// RotateAdminKeyRequest 轮换管理密钥。
//
// 只要新密钥：当前密钥已经在 Authorization 头上验过一次，请求体里再要一遍
// 并不多挡住谁，只是让轮换这件事多一个能填错的字段。
type RotateAdminKeyRequest struct {
	mux.Meta `path:"/admin/settings/admin-key" method:"POST"`
	// NewKey 有长度下限：密钥是这台镜像站后台的唯一凭据，允许轮换成 "1"
	// 等于把决策 18 的哈希存储做成摆设。
	NewKey string `json:"new_key" binding:"required,min=16,max=128" label:"新的管理密钥"`
}

// RotateAdminKeyResponse 轮换成功没有额外信息可返回。
//
// 尤其不回显新密钥：它已经在请求里了，回一遍只是多一处会被日志和代理留下的副本。
type RotateAdminKeyResponse struct{}
