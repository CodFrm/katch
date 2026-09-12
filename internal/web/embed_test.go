package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gin-gonic/gin"
	"github.com/smartystreets/goconvey/convey"
)

func testDist() fstest.MapFS {
	return fstest.MapFS{
		"index.html":              &fstest.MapFile{Data: []byte("<!doctype html><title>katch</title>")},
		"assets/index-abc123.js":  &fstest.MapFile{Data: []byte(strings.Repeat("export const a = 1;\n", 50))},
		"assets/index-abc123.css": &fstest.MapFile{Data: []byte(".a{color:red}")},
	}
}

func serve(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.NoRoute(newNoRouteHandlerFS(testDist()))
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

// TestAssets_HashedAssetsAreCachedImmutably
//
// /assets/ 下是 vite 带内容 hash 的产物，内容一变文件名就变，可以也应该被永久缓存。
// embed.FS 的 ModTime 是零值，不显式给缓存头的话连 Last-Modified 和 ETag 都没有，
// 于是每次打开页面都把整个 bundle 重新下一遍。
func TestAssets_HashedAssetsAreCachedImmutably(t *testing.T) {
	convey.Convey("带 hash 的资源必须可永久缓存", t, func() {
		w := serve(t, "/assets/index-abc123.js")
		convey.So(w.Code, convey.ShouldEqual, http.StatusOK)
		convey.So(w.Header().Get("Cache-Control"), convey.ShouldContainSubstring, "immutable")
		convey.So(w.Header().Get("Cache-Control"), convey.ShouldContainSubstring, "max-age=31536000")
	})
}

// TestAssets_IndexHTMLMustRevalidate
//
// index.html 是指向当前一组 hash 的名片，绝不能跟着被永久缓存，否则滚动更新后
// 浏览器永远拿旧名片，新版本再也上不去。
func TestAssets_IndexHTMLMustRevalidate(t *testing.T) {
	convey.Convey("index.html 必须每次回源校验", t, func() {
		for _, path := range []string{"/", "/mirrors"} {
			w := serve(t, path)
			convey.So(w.Code, convey.ShouldEqual, http.StatusOK)
			cc := w.Header().Get("Cache-Control")
			convey.So(cc, convey.ShouldContainSubstring, "no-cache")
			convey.So(cc, convey.ShouldNotContainSubstring, "immutable")
		}
	})
}

// TestAssets_MissingAssetIs404
//
// 滚动更新期间浏览器可能拿着新 index.html 向旧副本要新文件。回落 index.html 会
// 返回 200 + HTML，浏览器把 HTML 当 JS 解析报错，而 200 不会被任何监控计成失败。
func TestAssets_MissingAssetIs404(t *testing.T) {
	convey.Convey("assets 未命中直接 404 而不是回落 index.html", t, func() {
		w := serve(t, "/assets/index-deadbeef.js")
		convey.So(w.Code, convey.ShouldEqual, http.StatusNotFound)
	})
}

// TestAssets_APIPathIsNot404FallenBack
//
// /api/ 未命中必须是 404。回落 index.html 会让调用方拿到 200 + HTML，
// 把「接口不存在」这个错误彻底藏进一个成功状态码里。
func TestAssets_APIPathIsNot404FallenBack(t *testing.T) {
	convey.Convey("API 路径未命中返回 404", t, func() {
		w := serve(t, "/api/v1/not-exist")
		convey.So(w.Code, convey.ShouldEqual, http.StatusNotFound)
		convey.So(w.Header().Get("Content-Type"), convey.ShouldNotContainSubstring, "text/html")
	})
}
