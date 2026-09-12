// Package rule_entity 定义访问规则的实体。
package rule_entity

// 规则的动作。只有放行和拒绝两种，「默认怎么办」不是规则，而是上游上的一个字段。
const (
	// ActionAllow 放行。
	ActionAllow = "allow"
	// ActionDeny 拒绝。
	ActionDeny = "deny"
)

// AccessRule 一条访问规则：回答「这个资源允许被代理吗」。
//
// 规则**不带人工顺序**（决策 15）：同一层内按具体度定序，首个匹配者决定结果。
// 拖拽排序的列表里插入一条新规则就可能悄悄改变另一条的结果，而那个副作用在
// 界面上看不见；具体度是从规则自身算出来的，因此是确定的。
type AccessRule struct {
	ID int64 `gorm:"column:id;primary_key" json:"id"`
	// UpstreamID 为 0 即全局规则（决策 14：全局与上游内两层，全局先求值）。
	//
	// 用 0 而不是 NULL 表示全局：NULL 在索引、相等比较和 gorm 的零值语义里各有
	// 一套分支，而「全局」是一个正常取值，不是「没有值」。
	UpstreamID int64  `gorm:"column:upstream_id" json:"upstream_id"`
	Action     string `gorm:"column:action" json:"action"`
	// Pattern 路径模式，* 匹配任意一段字符。空模式什么都不匹配。
	Pattern    string `gorm:"column:pattern" json:"pattern"`
	Note       string `gorm:"column:note" json:"note"`
	Createtime int64  `gorm:"column:createtime" json:"createtime"`
	Updatetime int64  `gorm:"column:updatetime" json:"updatetime"`
}

// Global 报告这是不是一条全局规则。
func (a *AccessRule) Global() bool {
	return a.UpstreamID == 0
}
