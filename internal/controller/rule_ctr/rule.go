// Package rule_ctr 处理访问规则管理接口的请求。
package rule_ctr

import (
	"context"

	"github.com/CodFrm/katch/internal/api/admin"
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
func (r *Rule) Save(ctx context.Context, req *admin.SaveRuleRequest) (*admin.SaveRuleResponse, error) {
	return rule_svc.Rule().Save(ctx, req)
}

// Delete 删除一条访问规则。
func (r *Rule) Delete(ctx context.Context, req *admin.DeleteRuleRequest) (*admin.DeleteRuleResponse, error) {
	return rule_svc.Rule().Delete(ctx, req)
}

// Test 试算一条资源地址会被放行还是拒绝，并给出理由。
func (r *Rule) Test(ctx context.Context, req *admin.TestRuleRequest) (*admin.TestRuleResponse, error) {
	return rule_svc.Rule().Test(ctx, req)
}
