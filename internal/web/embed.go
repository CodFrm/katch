// Package web 把前端 SPA 嵌入 katch 二进制。
//
// dist 目录在 build 时由 frontend/dist 拷贝填充；dev 模式下前端由 vite 自己提供，
// 这里挂的是 make prepare-web-dist 造的占位文件。
package web

import (
	"bytes"
	"context"
	"embed"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/cago-frame/cago/configs"
	"github.com/cago-frame/cago/server/mux"
	"github.com/gin-gonic/gin"
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
		// API 未命中就是 404，不能回落 index.html：否则客户端拿到 200 + HTML，
		// 而 200 不会被任何监控计成失败，问题被彻底藏住。
		if strings.HasPrefix(path, "/api/") {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		if f, ferr := sub.Open(strings.TrimPrefix(path, "/")); ferr == nil {
			_ = f.Close()
			setCacheHeaders(c, path)
			fileSrv.ServeHTTP(c.Writer, c.Request)
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
