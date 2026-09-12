package api

import (
	"context"
	"strings"

	"github.com/cago-frame/cago/pkg/utils/httputils"
	"github.com/cago-frame/cago/server/mux"
	"github.com/gin-gonic/gin"

	"github.com/CodFrm/katch/internal/controller/system_ctr"
	"github.com/CodFrm/katch/internal/controller/upstream_ctr"
	"github.com/CodFrm/katch/internal/service/setting_svc"
)

// bearerPrefix 管理密钥的请求头形态：Authorization: Bearer <key>。
const bearerPrefix = "Bearer "

// Router 注册全部 HTTP 路由。
//
// 注意：这里只挂 katch 自身的管理接口，全部在 /api/ 前缀下。镜像代理走的是
// 另一套路径（/v2/... 和 /<上游host>/...），由代理层单独接管——两者必须分开，
// 否则上游 host 和管理接口会在同一个命名空间里抢路径。
func Router(_ context.Context, root *mux.Router) error {
	r := root.Group("/api/v1")

	sysCtr := system_ctr.NewSystem()
	r.Group("/").Bind(sysCtr.Version)

	// /api/v1/admin/* 一律要密钥；拉取路径与其余公开接口不经过这个中间件。
	adminGroup := r.Group("/", requireAdminKey)
	upstreamCtr := upstream_ctr.NewUpstream()
	adminGroup.Bind(upstreamCtr.List, upstreamCtr.Save, upstreamCtr.Delete)

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
	key := ""
	if v := c.GetHeader("Authorization"); strings.HasPrefix(v, bearerPrefix) {
		key = strings.TrimPrefix(v, bearerPrefix)
	}
	if err := setting_svc.Setting().VerifyAdminKey(c.Request.Context(), key); err != nil {
		// HandleError 内部走的是 AbortWithStatusJSON，后续处理器不会再被调用。
		_ = httputils.HandleError(c, err)
		return
	}
	c.Next()
}
