// Package hostfirst 把「上游主机在前」的 registry 写法在入口归一成 /v2/<host>/...。
//
// 「把 https:// 换成 https://<katch>/，其余照抄」落到 registry 上就是
// /<host>/v2/<repository>/...：整条 registry URL 粘过来是它，自己追加
// /v2/<repository> 的客户端把 <katch>/<host> 当 registry base 时也是它。可主机名
// 之后的 /v2 是协议边界还是上游路径的第一段要查上游表才分得开，判定在
// upstream_svc.CanonicalPullPath。
//
// 归一做在入口、排在计数与访问规则之前，而不是留给回源那一缝：三处各按
// dispatch.Classify 分流，其中任何一处还把它看成 static 路径，规则就会拿着
// /v2/<repository> 去比按仓库路径写的模式，一条 deny 换个写法就绕过去了。
package hostfirst

import (
	"context"
	"net/url"

	"github.com/cago-frame/cago"
	"github.com/cago-frame/cago/configs"
	"github.com/cago-frame/cago/pkg/logger"
	"github.com/cago-frame/cago/server/mux"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/CodFrm/katch/internal/service/upstream_svc"
)

// Middleware 构造归一中间件：改写的是请求路径本身，后面的计数、访问规则与拉取
// 处理器看到的都是规范形态。
func Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		escaped := c.Request.URL.EscapedPath()
		canonical, err := upstream_svc.CanonicalPullPath(ctx, escaped)
		if err != nil {
			// 不在这里收口：不改写的话它仍是一条 static 形态的拉取，后面的访问
			// 规则闸读同一张表，会按它自己的约定答 502，这次失败也照常被计数。
			logger.Ctx(ctx).Error("读取上游失败，无法判定 registry 写法", zap.Error(err))
			c.Next()
			return
		}
		if canonical != escaped {
			// canonical 由请求自己的转义路径拼出，解码不会失败。
			if decoded, uerr := url.PathUnescape(canonical); uerr == nil {
				c.Request.URL.Path = decoded
				c.Request.URL.RawPath = canonical
			}
		}
		c.Next()
	}
}

// Mount 把归一中间件挂到 gin 上，由 main 作为组件注册。
//
// 必须排在 metrics.Mount 之前：计数中间件在进入时就按路径分类，排在它后面的话
// 这类请求会被记成 static。
func Mount() cago.FuncComponent {
	return func(_ context.Context, _ *configs.Config) error {
		mux.RegisterMiddleware(func(_ *configs.Config, engine *gin.Engine) error {
			engine.Use(Middleware())
			return nil
		})
		return nil
	}
}
