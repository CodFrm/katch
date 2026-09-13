// Package rule_ctr 处理访问规则管理接口的请求。
package rule_ctr

import (
	"context"

	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/model/entity/event_entity"
	"github.com/CodFrm/katch/internal/service/event_svc"
	"github.com/CodFrm/katch/internal/service/rule_svc"
)

// Rule 访问规则管理控制器。
type Rule struct{}

// NewRule 构造访问规则管理控制器。
func NewRule() *Rule {
	return &Rule{}
}

// List 列出全部访问规则。
func (r *Rule) List(ctx context.Context, req *admin.ListRulesRequest) (*admin.ListRulesResponse, error) {
	return rule_svc.Rule().List(ctx, req)
}

// Save 新增或更新一条访问规则。
//
// 记事件记在这里而不是 rule_svc 里：业务层还要被规则闸、试算这些只读路径用到，
// 而「谁改了什么」是管理接口这一面的事实。写成功之后才记——失败的尝试不是变更。
func (r *Rule) Save(ctx context.Context, req *admin.SaveRuleRequest) (*admin.SaveRuleResponse, error) {
	resp, err := rule_svc.Rule().Save(ctx, req)
	if err != nil {
		return nil, err
	}
	// 新增与更新分成两个 kind：界面上「加了一条拒绝规则」和「把一条规则改成拒绝」
	// 是两件要分开读的事，挤进一个 kind 就只能靠看细节去猜。
	kind := event_entity.KindRuleCreated
	if req.ID != 0 {
		kind = event_entity.KindRuleUpdated
	}
	event_svc.Event().Record(ctx, &event_svc.RecordInput{
		Kind: kind, Actor: event_entity.ActorAdmin, UpstreamID: req.UpstreamID,
		Detail: map[string]any{
			"rule_id": resp.ID,
			"action":  req.Action,
			"pattern": req.Pattern,
		},
	})
	return resp, nil
}

// Delete 删除一条访问规则。
func (r *Rule) Delete(ctx context.Context, req *admin.DeleteRuleRequest) (*admin.DeleteRuleResponse, error) {
	// 先看清要删的是哪一条：删完这行就没了，而事件流是这条规则存在过的唯一
	// 记录（Out of scope：变更记进事件流，但不做草稿与回滚）。只留一个 id 的话，
	// 「是谁把那条放行规则删了」事后一个字也答不出来。
	deleted := r.ruleOf(ctx, req.ID)
	resp, err := rule_svc.Rule().Delete(ctx, req)
	if err != nil {
		return nil, err
	}
	detail := map[string]any{"rule_id": req.ID}
	upstreamID := int64(0)
	if deleted != nil {
		detail["action"] = deleted.Action
		detail["pattern"] = deleted.Pattern
		upstreamID = deleted.UpstreamID
	}
	event_svc.Event().Record(ctx, &event_svc.RecordInput{
		Kind: event_entity.KindRuleDeleted, Actor: event_entity.ActorAdmin,
		UpstreamID: upstreamID, Detail: detail,
	})
	return resp, nil
}

// ruleOf 查这条规则此刻长什么样，查不到时给 nil。
//
// 借 List 而不是另开一个按 id 查的方法：规则是人工维护的策略，规模是几十条
// （rule_repo 就是按这个前提只给了整表 List），而这条路径只在删规则时走一次。
// 取不到不是错误——事件记不全也不许让这次删除失败。
func (r *Rule) ruleOf(ctx context.Context, id int64) *admin.RuleItem {
	list, err := rule_svc.Rule().List(ctx, &admin.ListRulesRequest{})
	if err != nil {
		return nil
	}
	for _, item := range list.List {
		if item.ID == id {
			return item
		}
	}
	return nil
}

// Test 试算一条资源地址会被放行还是拒绝，并给出理由。
func (r *Rule) Test(ctx context.Context, req *admin.TestRuleRequest) (*admin.TestRuleResponse, error) {
	return rule_svc.Rule().Test(ctx, req)
}
