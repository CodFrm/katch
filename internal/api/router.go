package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/cago-frame/cago/pkg/logger"
	"github.com/cago-frame/cago/pkg/utils/httputils"
	"github.com/cago-frame/cago/server/mux"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/CodFrm/katch/internal/controller/rule_ctr"
	"github.com/CodFrm/katch/internal/controller/stat_ctr"
	"github.com/CodFrm/katch/internal/controller/system_ctr"
	"github.com/CodFrm/katch/internal/controller/upstream_ctr"
	"github.com/CodFrm/katch/internal/proxy/rulegate"
	"github.com/CodFrm/katch/internal/service/setting_svc"
)

// bearerPrefix 管理密钥的请求头形态：Authorization: Bearer <key>。
const bearerPrefix = "Bearer "

// Router 注册全部 HTTP 路由。
//
// 注意：这里只挂 katch 自身的管理接口，全部在 /api/ 前缀下。镜像代理走的是
// 另一套路径（/v2/... 和 /<上游host>/...），由代理层单独接管——两者必须分开，
// 否则上游 host 和管理接口会在同一个命名空间里抢路径。唯一伸到拉取路径上的是
// 下面那道访问规则闸：它没有自己的路由，只能挂在引擎上，理由见那里。
func Router(_ context.Context, root *mux.Router) error {
	// 访问规则闸挂在 gin 引擎上而不是某个路由组里：它守的是拉取路径，而拉取
	// 路径走的是 web.MountSPA 注册的 NoRoute，没有自己的路由可挂。gin 的
	// Engine.Use 会重建 NoRoute 的处理链，所以这里挂上去之后，先注册的那个
	// NoRoute 一样会先过这道闸。闸自己只认拉取路径，其余一律原样通过。
	root.Use(rulegate.Middleware())

	r := root.Group("/api/v1")

	sysCtr := system_ctr.NewSystem()
	statCtr := stat_ctr.NewStat()
	upstreamCtr := upstream_ctr.NewUpstream()
	// 版本号无条件公开：它不描述这台镜像站代理了什么，关掉也藏不住什么。
	r.Group("/").Bind(sysCtr.Version)

	// 站点总览与上游列表是首页那张名片：总览只给全站合计，不暴露单条路径、
	// 单个调用方或单个上游的量；上游列表只给启用中的上游和名片级字段。
	// 两者默认公开，站长可以用「公开首页」设置一起关掉——这台镜像站是不是
	// 只给自己人用，是部署者的决定，而不是这里替他做的决定。
	homeGroup := r.Group("/", allowPublicHome)
	homeGroup.Bind(statCtr.Overview, upstreamCtr.PublicList)

	// /api/v1/admin/* 一律要密钥；拉取路径与其余公开接口不经过这个中间件。
	adminGroup := r.Group("/", requireAdminKey)
	adminGroup.Bind(upstreamCtr.List, upstreamCtr.Save, upstreamCtr.Delete)
	adminGroup.Bind(statCtr.ByUpstream)
	ruleCtr := rule_ctr.NewRule()
	adminGroup.Bind(ruleCtr.List, ruleCtr.Save, ruleCtr.Delete, ruleCtr.Test)

	return nil
}

// requireAdminKey 校验管理密钥。
//
// 中间件放在路由层而不是单独的 pkg：它要引用 service，而 internal/api 是没有任何
// 包反向引用的叶子，放这里不会有循环依赖的风险。
//
// 「未提供密钥」和「密钥错误」必须返回完全相同的响应——状态码、响应体、响应头都
// 一致，且不带 WWW-Authenticate。任何差异都会告诉探测者「密钥这个字段你找对了」。
func requireAdminKey(c *gin.Context) {
	if err := setting_svc.Setting().VerifyAdminKey(c.Request.Context(), adminKey(c)); err != nil {
		// HandleError 内部走的是 AbortWithStatusJSON，后续处理器不会再被调用。
		_ = httputils.HandleError(c, err)
		return
	}
	c.Next()
}

// allowPublicHome 守首页那两个接口：设置说公开就放行，说不公开就只放行带管理
// 密钥的调用方。
//
// 关掉之后返回 404 而不是 401：401 会告诉探测者「这个端点在，只是你没权限」，
// 而这道开关的意图就是让站点看起来不提供这些信息。管理接口那边是另一回事——
// 后台端点本来就人尽皆知，藏的是密钥而不是端点。
//
// 先读设置再验密钥，顺序不能反：绝大多数请求是匿名的，默认公开的部署里这样
// 一次 bcrypt 都不用算。
func allowPublicHome(c *gin.Context) {
	ctx := c.Request.Context()
	public, err := setting_svc.Setting().PublicHomepage(ctx)
	if err != nil {
		// 读不出设置就判不了这次请求该不该看见。放行等于在库故障时把开关整个
		// 失效掉，所以这里收口。
		logger.Ctx(ctx).Error("读取公开首页设置失败", zap.Error(err))
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	if public {
		c.Next()
		return
	}
	if err := setting_svc.Setting().VerifyAdminKey(ctx, adminKey(c)); err != nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	c.Next()
}

// adminKey 取请求里的管理密钥，没带时是空串。
func adminKey(c *gin.Context) string {
	v := c.GetHeader("Authorization")
	if !strings.HasPrefix(v, bearerPrefix) {
		return ""
	}
	return strings.TrimPrefix(v, bearerPrefix)
}
