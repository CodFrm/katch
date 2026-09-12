// Package web 把前端 SPA 嵌入 katch 二进制。
//
// dist 目录在 build 时由 frontend/dist 拷贝填充；dev 模式下前端由 vite 自己提供，
// 这里挂的是 make prepare-web-dist 造的占位文件。
package web

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/cago-frame/cago/configs"
	"github.com/cago-frame/cago/pkg/logger"
	"github.com/cago-frame/cago/server/mux"
	"github.com/gin-gonic/gin"

	"github.com/CodFrm/katch/internal/proxy/dispatch"
	"github.com/CodFrm/katch/internal/service/proxy_svc"
)

//go:embed dist
var distFS embed.FS

// immutableCacheControl 给 /assets/ 下的产物。vite 把内容 hash 写进文件名，内容一变
// 文件名就跟着变，所以这些 URL 的内容永不改写——正是 immutable 的定义。
//
// 不能指望 net/http 的默认行为：embed.FS 的 ModTime 是零值，http.ServeContent 因此
// 连 Last-Modified 都不会发，net/http 也不会自动生成 ETag。不显式给缓存头，每次打开
// 页面都要把整份 bundle 重新下一遍，连一次 304 都省不下来。
const immutableCacheControl = "public, max-age=31536000, immutable"

// revalidateCacheControl 给 index.html。它是那张指向当前一组 hash 的名片，**绝不能**
// 跟着一起被永久缓存——否则滚动更新之后浏览器永远拿着旧名片，新版本再也上不去。
const revalidateCacheControl = "no-cache"

// newNoRouteHandler 构造 SPA 的 NoRoute 处理器。
func newNoRouteHandler() (gin.HandlerFunc, error) {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return nil, err
	}
	return newNoRouteHandlerFS(sub), nil
}

// newNoRouteHandlerFS 是 newNoRouteHandler 的可注入形态：dist 从外面给，用例因此能
// 拿真实形状的产物（带 hash 的 js/css）来验缓存头，而不必依赖 make prepare-web-dist
// 造的那份占位 index.html。
func newNoRouteHandlerFS(sub fs.FS) gin.HandlerFunc {
	fileSrv := http.FileServer(http.FS(sub))

	return func(c *gin.Context) {
		path := c.Request.URL.Path
		kind, host, rest := dispatch.Classify(c.Request.URL.EscapedPath())
		// katch 自身端点未命中就是 404，不能回落 index.html：否则调用方拿到
		// 200 + HTML，而 200 不会被任何监控计成失败，问题被彻底藏住。
		if kind == dispatch.KindSelf {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		// dist 里真有这个文件就先给文件。这一步必须排在上游分发之前：
		// /favicon.ico 这类根目录下的产物名含点，按分段规则会被当成上游主机名，
		// 而 dist 是编译期固定的一小撮文件，让它优先才不会因为谁加了一条上游
		// 就把界面打坏。
		if f, ferr := sub.Open(strings.TrimPrefix(path, "/")); ferr == nil {
			_ = f.Close()
			setCacheHeaders(c, path)
			fileSrv.ServeHTTP(c.Writer, c.Request)
			return
		}
		if serveUpstream(c, kind, host, rest) {
			return
		}
		// /assets/ 下未命中，只可能是滚动更新期间浏览器拿着新副本的 index.html
		// 向旧副本要新文件。回落 index.html 会让浏览器把 HTML 当 JS 解析，
		// 直接 404 才让这次缺失可见、刷新即可自愈。
		if strings.HasPrefix(path, "/assets/") {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		// 其余未命中路径是 SPA 的前端路由，回落 index.html
		idx, err := fs.ReadFile(sub, "index.html")
		if err != nil {
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
		setCacheHeaders(c, path)
		c.Writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.ServeContent(c.Writer, c.Request, "index.html", time.Time{}, bytes.NewReader(idx))
	}
}

// serveUpstream 处理拉取路径，返回这次请求是否已由它接管。
//
// 它和 SPA 共用 NoRoute 而不是新开一条 gin 通配路由：通配路由会和 /api/v1
// 以及将来任何一条真实路由抢同一段前缀，而 NoRoute 天然是「所有已注册路由都没
// 命中之后」，顺序问题不存在。
func serveUpstream(c *gin.Context, kind dispatch.Kind, host, rest string) bool {
	switch kind {
	case dispatch.KindRegistryPing:
		// registry 客户端拿这个探测「对面是不是一个 v2 registry」，它固定发在
		// /v2/ 上、不带上游主机名，所以不查白名单也无从查起。
		c.Header("Docker-Distribution-Api-Version", "registry/2.0")
		c.Data(http.StatusOK, "application/json; charset=utf-8", []byte("{}"))
		return true
	case dispatch.KindInvalid:
		// 拿不出主机名或带着回溯段的请求，和主机不在白名单里一样 404。
		c.AbortWithStatus(http.StatusNotFound)
		return true
	case dispatch.KindRegistry, dispatch.KindStatic:
		serveProxy(c, kind, host, rest)
		return true
	case dispatch.KindSPA, dispatch.KindSelf:
		return false
	}
	return false
}

// serveProxy 回源并把响应流式转发给客户端。
func serveProxy(c *gin.Context, kind dispatch.Kind, host, rest string) {
	// 镜像站只读。别的方法要么是写操作，要么是探测，一律不回源——转发一个
	// 没有请求体的 POST 给上游，得到的结果没有任何意义。
	if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
		c.AbortWithStatus(http.StatusMethodNotAllowed)
		return
	}
	ctx := c.Request.Context()
	body, meta, err := proxy_svc.Proxy().Fetch(ctx, &proxy_svc.Target{
		Kind:     kind,
		Host:     host,
		Path:     rest,
		RawQuery: c.Request.URL.RawQuery,
		Method:   c.Request.Method,
		Header:   c.Request.Header,
	})
	if err != nil {
		if errors.Is(err, proxy_svc.ErrUpstreamNotAllowed) {
			// 空响应体、不加任何头。主机名一旦出现在响应里，katch 就成了探测
			// 内网主机是否存在的工具——能回显的就是表里有的（决策 6）。
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		// 回源失败是 502 而不是 404：404 会让客户端把「这个对象不存在」当成
		// 结论记下来，而这只是上游此刻不可达。主机名只进日志，不进响应。
		logger.Ctx(ctx).Sugar().Errorw("回源失败", "host", host, "path", rest, "err", err)
		c.AbortWithStatus(http.StatusBadGateway)
		return
	}
	defer func() { _ = body.Close() }()
	// 上游的状态码原样透传，包括 4xx——那是上游对这个对象的判断，改写它只会
	// 让客户端拿到一个和真实源站不一致的结果。
	header := c.Writer.Header()
	for k, values := range meta.Header {
		for _, v := range values {
			header.Add(k, v)
		}
	}
	c.Writer.WriteHeader(meta.StatusCode)
	// io.Copy 而不是先读进内存：一个几百 MB 的镜像层读完再发，既把首字节推迟到
	// 整体下载完成，又让并发拉取直接把内存吃光。
	if _, err := io.Copy(c.Writer, body); err != nil {
		// 客户端断开也会走到这里，所以是 warn 不是 error。
		logger.Ctx(ctx).Sugar().Warnw("转发响应体中断", "host", host, "path", rest, "err", err)
	}
}

func setCacheHeaders(c *gin.Context, path string) {
	// 声明 Vary，否则共享缓存会把按某个 Accept-Encoding 协商出的副本发给别的客户端。
	c.Writer.Header().Set("Vary", "Accept-Encoding")
	if strings.HasPrefix(path, "/assets/") {
		c.Writer.Header().Set("Cache-Control", immutableCacheControl)
		return
	}
	c.Writer.Header().Set("Cache-Control", revalidateCacheControl)
}

// MountSPA 把 SPA 处理器挂到 gin 的 NoRoute 上。
//
// 这里没有做 gzip 预压缩：katch 的管理界面体积小，且部署形态默认在反代之后，
// 压缩交给反代更合适。若将来 bundle 变大又要直接对外，再在 newNoRouteHandlerFS
// 里加一层构造期预压缩（dist 编译期固定，压一次即可，不必为每个请求烧 CPU）。
func MountSPA(_ context.Context, _ *configs.Config) error {
	mux.RegisterMiddleware(func(_ *configs.Config, engine *gin.Engine) error {
		handler, err := newNoRouteHandler()
		if err != nil {
			return err
		}
		engine.NoRoute(handler)
		return nil
	})
	return nil
}
