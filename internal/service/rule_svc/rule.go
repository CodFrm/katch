// Package rule_svc 是访问规则的业务层。
//
// 规则回答一个问题：这个资源允许被代理吗。求值分全局与上游内两层，全局先于
// 上游内（决策 14），同层内按具体度定序、首个匹配者决定（决策 15），三步都没
// 命中时落到上游的默认策略。定序的内核在 decide.go，是个纯函数。
package rule_svc

import (
	"context"
	"sync"
	"time"

	"github.com/cago-frame/cago/pkg/i18n"

	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/model/entity/rule_entity"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/pkg/code"
	"github.com/CodFrm/katch/internal/proxy/dispatch"
	"github.com/CodFrm/katch/internal/repository/rule_repo"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
	"github.com/CodFrm/katch/internal/service/upstream_svc"
)

// RuleSvc 访问规则的业务操作。
type RuleSvc interface {
	// Evaluate 判定这个上游上的这条路径是否允许被代理。
	//
	// path 是上游侧路径（转义形态、以 / 开头，不含 registry 的 /v2 前缀），
	// 与回源时发给上游的那条路径同源——判定和上游看到的必须是同一串字节。
	//
	// 读不出规则时返回 error 而不是放行：一次库故障把整站变成开放代理，
	// 比返回一次暂时性失败严重得多。
	Evaluate(ctx context.Context, upstream *upstream_entity.Upstream, path string) (*Decision, error)
	List(ctx context.Context, req *admin.ListRulesRequest) (*admin.ListRulesResponse, error)
	Save(ctx context.Context, req *admin.SaveRuleRequest) (*admin.SaveRuleResponse, error)
	Delete(ctx context.Context, req *admin.DeleteRuleRequest) (*admin.DeleteRuleResponse, error)
	// Test 试算：不真的拉取，只报出判定、是哪条规则决定的，以及完整的求值过程。
	Test(ctx context.Context, req *admin.TestRuleRequest) (*admin.TestRuleResponse, error)
}

// ruleSvc 带一份进程内规则快照。
//
// 快照装在业务层而不是像上游那样包在仓储外面：规则的写入口只有本层这三个方法，
// 而拉取路径每个请求都要求值一次（一次 docker pull 是几十上百个请求），每次都
// 查一遍库等于把镜像站的吞吐绑在 sqlite 上。缓存的是整张表：规则是人工维护的
// 策略，规模是几十条，一次装载之后求值完全在内存里完成。
type ruleSvc struct {
	// mu 只护 snapshot 这个切片头；切片一旦发布就不再改，读侧因此不必持锁遍历。
	mu       sync.RWMutex
	snapshot []*rule_entity.AccessRule
	// loadMu 把并发的冷启动装载串起来，避免一堆请求同时撞上空快照时齐刷刷查库。
	loadMu sync.Mutex
}

// New 构造访问规则业务层。
func New() RuleSvc {
	return &ruleSvc{}
}

var defaultRule = New()

// Rule 返回访问规则业务层。
func Rule() RuleSvc {
	return defaultRule
}

// Register 注册实现，由测试注入。
func Register(svc RuleSvc) {
	defaultRule = svc
}

func (r *ruleSvc) Evaluate(
	ctx context.Context, upstream *upstream_entity.Upstream, path string,
) (*Decision, error) {
	rules, err := r.rules(ctx)
	if err != nil {
		return nil, err
	}
	global, own := split(rules, upstream.ID)
	return Decide(global, own, upstream.DefaultPolicy, path), nil
}

func (r *ruleSvc) List(
	ctx context.Context, _ *admin.ListRulesRequest,
) (*admin.ListRulesResponse, error) {
	// 直穿到库而不是走快照：管理界面必须看到刚写进去的那条。
	list, err := rule_repo.AccessRule().List(ctx)
	if err != nil {
		return nil, err
	}
	resp := &admin.ListRulesResponse{List: make([]*admin.RuleItem, 0, len(list))}
	for _, v := range list {
		resp.List = append(resp.List, toItem(v))
	}
	return resp, nil
}

func (r *ruleSvc) Save(
	ctx context.Context, req *admin.SaveRuleRequest,
) (*admin.SaveRuleResponse, error) {
	now := time.Now().Unix()
	rule := &rule_entity.AccessRule{Createtime: now}
	if req.ID != 0 {
		exist, err := rule_repo.AccessRule().Find(ctx, req.ID)
		if err != nil {
			return nil, err
		}
		if exist == nil {
			return nil, i18n.NewNotFoundError(ctx, code.RuleNotFound)
		}
		// 以库里那条为底再覆盖字段，别拿一个空实体去 Save：后者会把 createtime
		// 这类请求里没有的字段一起清零。
		rule = exist
	}
	if req.UpstreamID != 0 {
		// 挂在一个不存在的上游上的规则永远不会被求值，而界面上看起来它生效了。
		// 这里问的是仓储而不是 upstream_svc.FindByHost：那个方法把「已停用」
		// 折叠成了「不存在」，而给一条暂时停用的上游配规则是正常操作。
		upstream, err := upstream_repo.Upstream().Find(ctx, req.UpstreamID)
		if err != nil {
			return nil, err
		}
		if upstream == nil {
			return nil, i18n.NewNotFoundError(ctx, code.UpstreamNotFound)
		}
	}
	rule.UpstreamID = req.UpstreamID
	rule.Action = req.Action
	rule.Pattern = req.Pattern
	rule.Note = req.Note
	rule.Updatetime = now
	if err := rule_repo.AccessRule().Save(ctx, rule); err != nil {
		return nil, err
	}
	// 写成功才丢快照：写失败时库里没变，快照就还是对的。
	r.invalidate()
	return &admin.SaveRuleResponse{ID: rule.ID}, nil
}

func (r *ruleSvc) Delete(
	ctx context.Context, req *admin.DeleteRuleRequest,
) (*admin.DeleteRuleResponse, error) {
	exist, err := rule_repo.AccessRule().Find(ctx, req.ID)
	if err != nil {
		return nil, err
	}
	if exist == nil {
		// 不存在的 id 不能当成删除成功：界面上「删掉了」和「这条根本不在」是
		// 两件事，后者通常意味着调用方拿的是一份过期的列表。
		return nil, i18n.NewNotFoundError(ctx, code.RuleNotFound)
	}
	if err := rule_repo.AccessRule().Delete(ctx, req.ID); err != nil {
		return nil, err
	}
	r.invalidate()
	return &admin.DeleteRuleResponse{}, nil
}

func (r *ruleSvc) Test(
	ctx context.Context, req *admin.TestRuleRequest,
) (*admin.TestRuleResponse, error) {
	host, path := req.Host, req.Path
	if host == "" {
		// 界面上是「把资源地址粘进去」，所以整条拉取路径也要认。分段用的是
		// 拉取路径上那一个 dispatch.Classify，试算才不会和真实请求分家。
		_, host, path = dispatch.Classify(req.Path)
	}
	upstream, err := upstream_svc.Upstream().FindByHost(ctx, host)
	if err != nil {
		return nil, err
	}
	if upstream == nil {
		// 试算在管理接口之下，这里说清楚「这个上游不在表里」不泄漏什么——
		// 必须一律 404、不给差别的是公开的拉取路径（决策 6）。
		return nil, i18n.NewNotFoundError(ctx, code.UpstreamNotFound)
	}
	decision, err := r.Evaluate(ctx, upstream, path)
	if err != nil {
		return nil, err
	}
	resp := &admin.TestRuleResponse{
		Host:          host,
		Path:          path,
		Allowed:       decision.Allowed,
		Scope:         admin.Scope(decision.Scope),
		DefaultPolicy: upstream.DefaultPolicy,
		Trace:         make([]*admin.RuleTraceStep, 0, len(decision.Trace)),
	}
	if resp.DefaultPolicy == "" {
		resp.DefaultPolicy = upstream_entity.PolicyAllowAll
	}
	if decision.MatchedRule != nil {
		resp.MatchedRule = toItem(decision.MatchedRule)
	}
	for _, step := range decision.Trace {
		resp.Trace = append(resp.Trace, &admin.RuleTraceStep{
			Scope:    admin.Scope(step.Scope),
			RuleID:   step.RuleID,
			Pattern:  step.Pattern,
			Action:   step.Action,
			Matched:  step.Matched,
			Decisive: step.Decisive,
		})
	}
	return resp, nil
}

// rules 取当前快照，没有就装载一份。
func (r *ruleSvc) rules(ctx context.Context) ([]*rule_entity.AccessRule, error) {
	if list := r.current(); list != nil {
		return list, nil
	}
	r.loadMu.Lock()
	defer r.loadMu.Unlock()
	// 等锁的时候别人可能已经装载好了，再看一眼。
	if list := r.current(); list != nil {
		return list, nil
	}
	list, err := rule_repo.AccessRule().List(ctx)
	if err != nil {
		return nil, err
	}
	if list == nil {
		// 空表也要留下一份非 nil 的快照，否则「表里没有规则」这个常态会让每个
		// 请求都重新装载一次。
		list = []*rule_entity.AccessRule{}
	}
	r.mu.Lock()
	r.snapshot = list
	r.mu.Unlock()
	return list, nil
}

func (r *ruleSvc) current() []*rule_entity.AccessRule {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.snapshot
}

func (r *ruleSvc) invalidate() {
	r.mu.Lock()
	r.snapshot = nil
	r.mu.Unlock()
}

// split 把一份快照分成全局层与该上游那一层，别的上游的规则不参与本次求值。
func split(rules []*rule_entity.AccessRule, upstreamID int64) (global, own []*rule_entity.AccessRule) {
	for _, v := range rules {
		switch {
		case v.Global():
			global = append(global, v)
		case v.UpstreamID == upstreamID:
			own = append(own, v)
		}
	}
	return global, own
}

// toItem 把实体映射成对外结构。
func toItem(v *rule_entity.AccessRule) *admin.RuleItem {
	return &admin.RuleItem{
		ID:         v.ID,
		UpstreamID: v.UpstreamID,
		Action:     v.Action,
		Pattern:    v.Pattern,
		Note:       v.Note,
		Createtime: v.Createtime,
		Updatetime: v.Updatetime,
	}
}
