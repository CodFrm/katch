// Package upstream_entity 定义上游的实体。
package upstream_entity

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// Kind 上游的协议类别。只有两种，其余差异由记录上的字段表达。
const (
	// KindRegistry container registry 协议（鉴权与路径形态特殊）。
	KindRegistry = "registry"
	// KindStatic 普通静态资源（APT、Go module proxy、GitHub 原始文件等）。
	KindStatic = "static"
)

// 上游的默认策略。它是上游上的一个字段而不是一条访问规则——它没有 pattern，
// 混进规则表会让规则的具体度排序无从谈起。
const (
	// PolicyAllowAll 默认放行。
	PolicyAllowAll = "allow_all"
	// PolicyDenyUnlessMatched 默认拒绝，只有命中 allow 规则才放行。
	PolicyDenyUnlessMatched = "deny_unless_matched"
)

// PatternList 路径模式列表，在库里存成 JSON 文本。
//
// 存 JSON 而不是逗号分隔：模式里出现逗号是合法的，分隔符方案会在那一天悄悄拆错。
type PatternList []string

// Value 实现 driver.Valuer。空列表也写成 "[]"，避免库里出现 NULL 与 "" 两种空。
func (p PatternList) Value() (driver.Value, error) {
	b, err := json.Marshal([]string(p))
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// Scan 实现 sql.Scanner。sqlite 给的是 string，MySQL 给的是 []byte，两种都要认。
func (p *PatternList) Scan(src any) error {
	var b []byte
	switch v := src.(type) {
	case nil:
		*p = nil
		return nil
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		return fmt.Errorf("upstream: 无法把 %T 读成路径模式列表", src)
	}
	if len(b) == 0 {
		*p = nil
		return nil
	}
	return json.Unmarshal(b, (*[]string)(p))
}

// Upstream 一个被允许代理的上游主机。
//
// 上游表就是白名单：不在表里或已停用的主机一律 404，否则 katch 会变成开放代理。
type Upstream struct {
	ID     int64  `gorm:"column:id;primary_key" json:"id"`
	Host   string `gorm:"column:host" json:"host"`
	Kind   string `gorm:"column:kind" json:"kind"`
	Origin string `gorm:"column:origin" json:"origin"`
	// Enabled 为 false 时等同于不存在：既不回源，也不在拉取路径上回显。
	Enabled bool `gorm:"column:enabled" json:"enabled"`
	// ImmutablePatterns 命中即视为内容寻址、可长期缓存的路径模式
	// （APT 是 pool/，Go proxy 是 @v/，raw 是 40 位 commit SHA）。
	ImmutablePatterns PatternList `gorm:"column:immutable_patterns;type:text" json:"immutable_patterns"`
	// MutableTTLSeconds 可变对象的缓存时长，0 表示用全局默认值。
	MutableTTLSeconds int    `gorm:"column:mutable_ttl_seconds" json:"mutable_ttl_seconds"`
	DefaultPolicy     string `gorm:"column:default_policy" json:"default_policy"`
	// LibraryCompletion 仅对 registry 有意义：把 redis 补全成 library/redis。
	LibraryCompletion bool   `gorm:"column:library_completion" json:"library_completion"`
	Note              string `gorm:"column:note" json:"note"`
	Createtime        int64  `gorm:"column:createtime" json:"createtime"`
	Updatetime        int64  `gorm:"column:updatetime" json:"updatetime"`
}
