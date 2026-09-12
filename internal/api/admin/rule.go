package admin

import "github.com/cago-frame/cago/server/mux"

// RuleItem 一条访问规则在管理接口上的表示。
type RuleItem struct {
	ID int64 `json:"id"`
	// UpstreamID 为 0 即全局规则。
	UpstreamID int64  `json:"upstream_id"`
	Action     string `json:"action"`
	Pattern    string `json:"pattern"`
	Note       string `json:"note"`
	Createtime int64  `json:"createtime"`
	Updatetime int64  `json:"updatetime"`
}

// ListRulesRequest 列出全部访问规则（全局的与各上游的）。
type ListRulesRequest struct {
	mux.Meta `path:"/admin/rules" method:"GET"`
}

// ListRulesResponse 规则列表。
//
// 不带「第几条」这种序号：规则没有人工顺序，同层内按具体度定序（决策 15），
// 给出一个看起来像优先级的下标只会让人以为拖动它能改变结果。
type ListRulesResponse struct {
	List []*RuleItem `json:"list"`
}

// SaveRuleRequest 新增或更新一条访问规则。ID 为 0 时新增，否则更新该条。
type SaveRuleRequest struct {
	mux.Meta `path:"/admin/rules" method:"POST"`
	ID       int64 `json:"id"`
	// UpstreamID 留空即全局规则，它先于任何上游内规则求值。
	UpstreamID int64  `json:"upstream_id"`
	Action     string `json:"action" binding:"required,oneof=allow deny" label:"动作"`
	// Pattern 必填：空模式什么都不匹配，存进去的是一条永远不生效的规则。
	Pattern string `json:"pattern" binding:"required" label:"路径模式"`
	Note    string `json:"note"`
}

// SaveRuleResponse 返回该条规则的 id。
type SaveRuleResponse struct {
	ID int64 `json:"id"`
}

// DeleteRuleRequest 删除一条访问规则。
type DeleteRuleRequest struct {
	mux.Meta `path:"/admin/rules/:id" method:"DELETE"`
	ID       int64 `uri:"id"`
}

// DeleteRuleResponse 删除成功没有额外信息可返回。
type DeleteRuleResponse struct{}

// TestRuleRequest 规则试算：给一条资源地址，问它此刻会被放行还是拒绝。
//
// Host 可以留空，那时 Path 按拉取路径的形态解析（/docker.io/library/redis:7），
// 界面上就能直接把一条 katch 地址粘进来。
type TestRuleRequest struct {
	mux.Meta `path:"/admin/rules/test" method:"POST"`
	Host     string `json:"host"`
	Path     string `json:"path" binding:"required" label:"资源路径"`
}

// Scope 判定发生在哪一层：global、upstream 或 default。
type Scope string

// RuleTraceStep 试算过程中的一步。
type RuleTraceStep struct {
	Scope   Scope  `json:"scope"`
	RuleID  int64  `json:"rule_id"`
	Pattern string `json:"pattern"`
	Action  string `json:"action"`
	Matched bool   `json:"matched"`
	// Decisive 这一步是否决定了最终结果。
	Decisive bool `json:"decisive"`
}

// TestRuleResponse 试算结果。
//
// 它是决策 15 的配套：具体度定序换来了确定性，代价是「为什么是这条命中」不再
// 显然，所以这里必须同时给出判定、是哪条规则决定的、以及完整的求值过程——
// 只回一个 allowed 的接口，等于让人对着一份看不出优先级的列表自己推。
type TestRuleResponse struct {
	Host    string `json:"host"`
	Path    string `json:"path"`
	Allowed bool   `json:"allowed"`
	Scope   Scope  `json:"scope"`
	// MatchedRule 决定结果的那条规则；落到默认策略时为 null。
	MatchedRule *RuleItem `json:"matched_rule"`
	// DefaultPolicy 该上游的默认策略，即三步都没命中时的兜底。
	DefaultPolicy string `json:"default_policy"`
	// Trace 按考察顺序排列的求值过程，止于决定结果的那一步。
	Trace []*RuleTraceStep `json:"trace"`
}
