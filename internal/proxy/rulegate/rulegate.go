// Package rulegate 是访问规则在拉取路径上的那道闸。
//
// 它挂在 gin 引擎上，排在拉取处理器之前收口：被规则拒绝的请求返回 403，
// **不回源、不写缓存**（「访问规则」一节）。闸装在这一层而不是回源那一缝上，
// 是因为拒绝必须对缓存命中同样成立——判定说「这个资源不该被代理」，盘上碰巧
// 有一份旧副本并不改变这个结论。
package rulegate

import (
	"net/http"

	"github.com/cago-frame/cago/pkg/logger"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/CodFrm/katch/internal/metrics"
	"github.com/CodFrm/katch/internal/proxy/dispatch"
	"github.com/CodFrm/katch/internal/service/rule_svc"
	"github.com/CodFrm/katch/internal/service/upstream_svc"
)

// Middleware 构造访问规则闸。
//
// 由 internal/api/router.go 挂到 gin 引擎上。gin 的 Engine.Use 会重建 NoRoute 的
// 处理链，所以先前注册的那个拉取处理器一样会先过这道闸。
//
// 计数不在这里做：/metrics 上的 katch_requests_total 由拉取路径最外层那个计数
// 中间件按响应本身判定，403 就是 denied。判定只有一个出处，计数也只有一个出处。
func Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		kind, host, rest := dispatch.Classify(c.Request.URL.EscapedPath())
		// 只有拉取才受访问规则约束。管理接口、前端路由、静态产物与 /v2/ 探测
		// 都原样通过——否则一条 deny "*" 会把界面本身一起关掉，连进去把它删了
		// 都做不到。
		if !dispatch.IsPull(kind) {
			c.Next()
			return
		}
		ctx := c.Request.Context()
		upstream, err := upstream_svc.Upstream().FindByHost(ctx, host)
		if err != nil {
			// 读不出上游表就判不了这次拉取是否被允许。放行等于在库故障时把
			// 规则整个失效掉，所以这里收口；502 而不是 403：这是一次暂时的
			// 失败，不是「这个资源不许代理」这个结论。
			logger.Ctx(ctx).Error("读取上游失败，无法判定访问规则",
				zap.String("host", host), zap.Error(err))
			c.AbortWithStatus(http.StatusBadGateway)
			return
		}
		if upstream == nil {
			// 不在白名单里的主机交给既有那条路去答 404。在这里换成 403 会让
			// 「表里有这个上游但规则拒绝」和「表里根本没有」可被区分，那正是
			// 探测内网主机是否存在需要的信号（决策 6）。
			c.Next()
			return
		}
		decision, err := rule_svc.Rule().Evaluate(ctx, upstream, rest)
		if err != nil {
			logger.Ctx(ctx).Error("求值访问规则失败",
				zap.String("host", host), zap.String("path", rest), zap.Error(err))
			c.AbortWithStatus(http.StatusBadGateway)
			return
		}
		// 判定数按「落在哪一层、判成什么」记（可观测性一节）。放行也记：
		// 只记拒绝的话，这一族回答不了「策略整体在放行还是在拦」——而那正是
		// 改完一条规则之后要看的第一个数。
		metrics.RecordRuleDecision(string(decision.Scope), decisionLabel(decision.Allowed))
		if decision.Allowed {
			c.Next()
			return
		}
		// 空响应体、不加任何头，和白名单那条 404 同一个形态：主机名一旦出现在
		// 响应里，katch 就成了回显内网主机名的工具。理由进日志，不进响应。
		logger.Ctx(ctx).Info("访问规则拒绝了这次拉取",
			zap.String("host", host), zap.String("path", rest),
			zap.String("scope", string(decision.Scope)), zap.Int64("rule_id", ruleID(decision)))
		c.AbortWithStatus(http.StatusForbidden)
	}
}

// ruleID 取决定结果的那条规则的 id，落到默认策略时是 0。
func ruleID(decision *rule_svc.Decision) int64 {
	if decision.MatchedRule == nil {
		return 0
	}
	return decision.MatchedRule.ID
}

// decisionLabel 判定结果的标签值，和访问规则里 action 的取值对齐。
func decisionLabel(allowed bool) string {
	if allowed {
		return "allow"
	}
	return "deny"
}
