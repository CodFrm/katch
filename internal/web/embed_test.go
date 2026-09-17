package web

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gin-gonic/gin"
	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/destination"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
	mock_upstream_repo "github.com/CodFrm/katch/internal/repository/upstream_repo/mock"
	"github.com/CodFrm/katch/internal/service/proxy_svc"
)

func testDist() fstest.MapFS {
	return fstest.MapFS{
		"index.html":              &fstest.MapFile{Data: []byte("<!doctype html><title>katch</title>")},
		"assets/index-abc123.js":  &fstest.MapFile{Data: []byte(strings.Repeat("export const a = 1;\n", 50))},
		"assets/index-abc123.css": &fstest.MapFile{Data: []byte(".a{color:red}")},
		// 根目录下的产物名含点，按分段规则长得像上游主机名——用它钉住 dist 优先。
		"favicon.ico": &fstest.MapFile{Data: []byte("\x00\x00\x01\x00")},
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

// upstreamTable 把上游表装进进程内缓存那一层，装配形态与 main 一致。
func upstreamTable(t *testing.T, list ...*upstream_entity.Upstream) {
	t.Helper()
	repo := mock_upstream_repo.NewMockUpstreamRepo(gomock.NewController(t))
	repo.EXPECT().List(gomock.Any()).Return(list, nil).AnyTimes()
	upstream_repo.RegisterUpstream(proxy_svc.NewCachedUpstreamRepo(repo))
}

func request(t *testing.T, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.NoRoute(newNoRouteHandlerFS(testDist()))
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}

// fakeOrigin 起一个假源站。用例里绝不打真实网络：真上游会让用例随网络状况变绿变红。
func fakeOrigin(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestProxy_StaticUpstreamIsStreamedThrough
//
// 这是整条拉取路径的主干：apt-get 请求 /deb.debian.org/... 时拿到的必须是源站的
// 字节，而不是 index.html。
func TestProxy_StaticUpstreamIsStreamedThrough(t *testing.T) {
	convey.Convey("已启用的上游把源站字节原样返回", t, func() {
		payload := strings.Repeat("Package: nginx\n", 4096)
		gotURI := ""
		origin := fakeOrigin(t, func(w http.ResponseWriter, r *http.Request) {
			gotURI = r.URL.RequestURI()
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("ETag", `"v1"`)
			_, _ = io.WriteString(w, payload)
		})
		upstreamTable(t, &upstream_entity.Upstream{
			ID: 1, Host: "deb.debian.org", Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
			Origin: origin, Enabled: true,
		})

		w := request(t, http.MethodGet, "/deb.debian.org/dists/stable/InRelease")
		convey.So(w.Code, convey.ShouldEqual, http.StatusOK)
		convey.So(gotURI, convey.ShouldEqual, "/dists/stable/InRelease")
		convey.So(w.Body.String(), convey.ShouldEqual, payload)
		// 响应头也要过来：少了 Content-Type 客户端会猜，少了 ETag 就没有条件请求。
		convey.So(w.Header().Get("Content-Type"), convey.ShouldEqual, "text/plain")
		convey.So(w.Header().Get("ETag"), convey.ShouldEqual, `"v1"`)
	})
}

// TestProxy_UpstreamStatusIsPassedThrough 上游的 4xx 原样透传，不改写成别的。
func TestProxy_UpstreamStatusIsPassedThrough(t *testing.T) {
	convey.Convey("上游 4xx 原样透传", t, func() {
		origin := fakeOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, "404 Not Found: pool/x.deb")
		})
		upstreamTable(t, &upstream_entity.Upstream{
			ID: 1, Host: "deb.debian.org", Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
			Origin: origin, Enabled: true,
		})

		w := request(t, http.MethodGet, "/deb.debian.org/pool/x.deb")
		convey.So(w.Code, convey.ShouldEqual, http.StatusNotFound)
		convey.So(w.Body.String(), convey.ShouldEqual, "404 Not Found: pool/x.deb")
	})
}

// TestProxy_UnreachableUpstreamIs502
//
// 回源失败必须和「这个对象不存在」分开：回 404 会让客户端把一次临时故障当成
// 结论记下来（apt 会把它写进自己的状态，go 会把它缓存成一次失败）。
func TestProxy_UnreachableUpstreamIs502(t *testing.T) {
	convey.Convey("上游不可达返回 502", t, func() {
		dead := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
		dead.Close()
		upstreamTable(t, &upstream_entity.Upstream{
			ID: 1, Host: "deb.debian.org", Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
			Origin: dead.URL, Enabled: true,
		})

		w := request(t, http.MethodGet, "/deb.debian.org/pool/x.deb")
		convey.So(w.Code, convey.ShouldEqual, http.StatusBadGateway)
		convey.So(w.Body.String(), convey.ShouldNotContainSubstring, "deb.debian.org")
	})
}

// TestProxy_UnknownUpstreamIs404WithoutEchoingHost
//
// 决策 6：上游表就是白名单，不在表里或已停用的主机必须 404。这里同时钉住两件事：
// 一是它不能回落 index.html——回落会让 docker pull / apt-get 拿到 200 + HTML，
// 而 200 不会被任何监控计成失败；二是 404 的响应体和响应头都不许出现该主机名，
// 否则 katch 就成了探测内网主机是否存在的工具（能回显的就是表里有的）。
func TestProxy_UnknownUpstreamIs404WithoutEchoingHost(t *testing.T) {
	convey.Convey("未知或停用的上游 404 且不回显主机名", t, func() {
		cases := map[string]string{
			"表里没有这个主机":           "/internal.corp.local/etc/passwd",
			"表里有但已停用":            "/deb.debian.org/dists/stable/InRelease",
			"停用的主机走 registry 路径": "/v2/deb.debian.org/library/redis/manifests/7",
			"路径里带回溯段":            "/deb.debian.org/pool/../../internal.corp.local",
		}
		for name, path := range cases {
			convey.Convey(name+"："+path, func() {
				upstreamTable(t, &upstream_entity.Upstream{
					ID: 1, Host: "deb.debian.org", Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
					Origin: "http://127.0.0.1:1", Enabled: false,
				})

				w := request(t, http.MethodGet, path)
				convey.So(w.Code, convey.ShouldEqual, http.StatusNotFound)
				convey.So(w.Header().Get("Content-Type"), convey.ShouldNotContainSubstring, "text/html")
				convey.So(w.Body.String(), convey.ShouldBeEmpty)
				for k, values := range w.Header() {
					for _, v := range values {
						convey.So(k+": "+v, convey.ShouldNotContainSubstring, "deb.debian.org")
						convey.So(k+": "+v, convey.ShouldNotContainSubstring, "internal.corp.local")
					}
				}
			})
		}
	})
}

// TestProxy_RegistryPingIs200
//
// registry 客户端在拉取之前先探 /v2/。这个请求不带上游主机名，回落 index.html
// 会让 docker 认为对面不是 registry 而直接放弃。
func TestProxy_RegistryPingIs200(t *testing.T) {
	convey.Convey("/v2/ 探测返回 200", t, func() {
		upstreamTable(t)
		for _, path := range []string{"/v2/", "/v2"} {
			w := request(t, http.MethodGet, path)
			convey.So(w.Code, convey.ShouldEqual, http.StatusOK)
			convey.So(w.Header().Get("Content-Type"), convey.ShouldNotContainSubstring, "text/html")
			convey.So(w.Header().Get("Docker-Distribution-Api-Version"), convey.ShouldEqual, "registry/2.0")
		}
	})
}

// TestProxy_RegistryHostSitsAfterV2 registry 的主机名在 /v2/ 之后，且 /v2 前缀
// 要补回给上游。
func TestProxy_RegistryHostSitsAfterV2(t *testing.T) {
	convey.Convey("registry 请求按 /v2/<host>/ 分发", t, func() {
		gotURI := ""
		origin := fakeOrigin(t, func(w http.ResponseWriter, r *http.Request) {
			gotURI = r.URL.RequestURI()
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			_, _ = io.WriteString(w, `{"schemaVersion":2}`)
		})
		upstreamTable(t, &upstream_entity.Upstream{
			ID: 1, Host: "docker.io", Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolRegistry},
			Origin: origin, Enabled: true,
		})

		w := request(t, http.MethodGet, "/v2/docker.io/library/redis/manifests/7")
		convey.So(w.Code, convey.ShouldEqual, http.StatusOK)
		convey.So(gotURI, convey.ShouldEqual, "/v2/library/redis/manifests/7")
		convey.So(w.Body.String(), convey.ShouldEqual, `{"schemaVersion":2}`)
	})
}

// TestProxy_RegistryBaseForHomebrew reaches the same registry adapter as the legacy
// route, but reserves an unambiguous base before Homebrew appends its own /v2 path.
func TestProxy_RegistryBaseForHomebrew(t *testing.T) {
	convey.Convey("Homebrew registry base forwards exactly one /v2 prefix and keeps the whitelist", t, func() {
		gotURI := ""
		originCalls := 0
		origin := fakeOrigin(t, func(w http.ResponseWriter, r *http.Request) {
			originCalls++
			gotURI = r.URL.RequestURI()
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			_, _ = io.WriteString(w, `{"schemaVersion":2}`)
		})
		upstreamTable(t, &upstream_entity.Upstream{
			ID: 1, Host: "ghcr.io", Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolRegistry},
			Origin: origin, Enabled: true,
		})

		w := request(t, http.MethodGet, "/registry/ghcr.io/v2/homebrew/core/jq/manifests/tag")
		convey.So(w.Code, convey.ShouldEqual, http.StatusOK)
		convey.So(gotURI, convey.ShouldEqual, "/v2/homebrew/core/jq/manifests/tag")
		convey.So(originCalls, convey.ShouldEqual, 1)

		unknown := request(t, http.MethodGet, "/registry/unknown.example/v2/homebrew/core/jq/manifests/tag")
		convey.So(unknown.Code, convey.ShouldEqual, http.StatusNotFound)
		convey.So(unknown.Body.String(), convey.ShouldBeEmpty)
		convey.So(originCalls, convey.ShouldEqual, 1)
	})
}

// TestProxy_ClientCredentialsAreNotForwarded 决策 11：客户端的 Authorization
// 不转发给上游。这条在回源侧已经有用例，这里钉住它没有被拉取路径绕过去。
func TestProxy_ClientCredentialsAreNotForwarded(t *testing.T) {
	convey.Convey("客户端凭据不会随拉取转给上游", t, func() {
		var got http.Header
		origin := fakeOrigin(t, func(_ http.ResponseWriter, r *http.Request) {
			got = r.Header.Clone()
		})
		upstreamTable(t, &upstream_entity.Upstream{
			ID: 1, Host: "deb.debian.org", Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
			Origin: origin, Enabled: true,
		})

		gin.SetMode(gin.TestMode)
		engine := gin.New()
		engine.NoRoute(newNoRouteHandlerFS(testDist()))
		req := httptest.NewRequest(http.MethodGet, "/deb.debian.org/pool/x.deb", nil)
		req.Header.Set("Authorization", "Basic c2VjcmV0")
		engine.ServeHTTP(httptest.NewRecorder(), req)

		convey.So(got.Get("Authorization"), convey.ShouldBeEmpty)
	})
}

// TestProxy_WriteMethodsAreRejected 镜像站只读，写方法不回源。
func TestProxy_WriteMethodsAreRejected(t *testing.T) {
	convey.Convey("非 GET/HEAD 不回源", t, func() {
		upstreamTable(t, &upstream_entity.Upstream{
			ID: 1, Host: "deb.debian.org", Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
			Origin: "http://127.0.0.1:1", Enabled: true,
		})

		w := request(t, http.MethodPost, "/deb.debian.org/pool/x.deb")
		convey.So(w.Code, convey.ShouldEqual, http.StatusMethodNotAllowed)
	})
}

// TestProxy_DistFileWinsOverUpstreamRule
//
// /favicon.ico 的第一段含点，按分段规则长得像上游主机名。dist 里真有这个文件时
// 必须先给文件——否则加一条上游就可能把界面上的图标打成 404。
func TestProxy_DistFileWinsOverUpstreamRule(t *testing.T) {
	convey.Convey("dist 里的产物优先于上游分发", t, func() {
		upstreamTable(t)
		w := request(t, http.MethodGet, "/favicon.ico")
		convey.So(w.Code, convey.ShouldEqual, http.StatusOK)
		convey.So(w.Body.Len(), convey.ShouldBeGreaterThan, 0)
	})
}

type fixedWebRewriteSource struct {
	snapshot *proxy_svc.RewriteSnapshot
}

func (s fixedWebRewriteSource) Snapshot(context.Context) (*proxy_svc.RewriteSnapshot, error) {
	return s.snapshot, nil
}

type webResolverFunc func(context.Context, *url.URL, destination.DestinationRequirement) (*destination.ResolvedTarget, error)

func (f webResolverFunc) Resolve(ctx context.Context, target *url.URL, requirement destination.DestinationRequirement) (*destination.ResolvedTarget, error) {
	return f(ctx, target, requirement)
}

func configuredWebSumDB() *proxy_svc.RewriteSnapshot {
	return &proxy_svc.RewriteSnapshot{Upstreams: map[string]proxy_svc.RewriteUpstream{
		"sum.golang.org": {
			Profile: upstream_entity.PackageProfileGoProxy,
			Transports: upstream_entity.ProtocolSet{
				upstream_entity.ProtocolStatic,
			},
		},
	}}
}

func useWebSumDB(t *testing.T, snapshot *proxy_svc.RewriteSnapshot, resolver destination.DestinationResolver) {
	t.Helper()
	upstreamTable(t, &upstream_entity.Upstream{
		ID: 9, Host: "sum.golang.org", Origin: "https://sum.golang.org", Enabled: true,
		Protocols:      upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
		PackageProfile: upstream_entity.PackageProfileGoProxy,
	})
	previous := proxy_svc.Proxy()
	proxy_svc.Register(proxy_svc.New(proxy_svc.Options{
		RewriteConfig: fixedWebRewriteSource{snapshot: snapshot}, DestinationResolver: resolver,
	}))
	t.Cleanup(func() { proxy_svc.Register(previous) })
}

func TestProxy_SumDBSupportedUsesBothNoRouteShapes(t *testing.T) {
	resolverCalls := 0
	useWebSumDB(t, configuredWebSumDB(), webResolverFunc(func(context.Context, *url.URL, destination.DestinationRequirement) (*destination.ResolvedTarget, error) {
		resolverCalls++
		return nil, errors.New("supported must not reach origin")
	}))

	for _, path := range []string{
		"/proxy.golang.org/sumdb/sum.golang.org/supported",
		"/sumdb/sum.golang.org/supported",
	} {
		w := request(t, http.MethodGet, path)
		if w.Code != http.StatusOK || w.Body.Len() != 0 || strings.Contains(w.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("GET %s = status %d, content-type %q, body %q; want empty 200", path, w.Code, w.Header().Get("Content-Type"), w.Body.String())
		}
	}
	if resolverCalls != 0 {
		t.Fatalf("resolver calls = %d, want 0", resolverCalls)
	}
}

func TestProxy_SumDBInvalidConfigurationIs503(t *testing.T) {
	useWebSumDB(t, &proxy_svc.RewriteSnapshot{Upstreams: map[string]proxy_svc.RewriteUpstream{}}, nil)
	for _, path := range []string{
		"/proxy.golang.org/sumdb/sum.golang.org/supported",
		"/sumdb/sum.golang.org/supported",
	} {
		if got := request(t, http.MethodGet, path).Code; got != http.StatusServiceUnavailable {
			t.Fatalf("GET %s = %d, want 503", path, got)
		}
	}
}

func TestProxy_SumDBRejectsMalformedHostsAndMethods(t *testing.T) {
	resolverCalls := 0
	useWebSumDB(t, configuredWebSumDB(), webResolverFunc(func(context.Context, *url.URL, destination.DestinationRequirement) (*destination.ResolvedTarget, error) {
		resolverCalls++
		return nil, errors.New("rejected request reached origin")
	}))

	for _, path := range []string{
		"/sumdb/other.example/supported",
		"/sumdb/sum.golang.org",
		"/sumdb/sum.golang.org/not-a-route",
		"/proxy.golang.org/sumdb/other.example/supported",
		"/proxy.golang.org/sumdb/sum.golang.org/latest/extra",
	} {
		if got := request(t, http.MethodGet, path).Code; got != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", path, got)
		}
	}
	for _, path := range []string{
		"/proxy.golang.org/sumdb/sum.golang.org/supported",
		"/sumdb/sum.golang.org/lookup/example.com/mod@v1.0.0",
	} {
		if got := request(t, http.MethodPost, path).Code; got != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s = %d, want 405", path, got)
		}
	}
	if resolverCalls != 0 {
		t.Fatalf("resolver calls = %d, want 0", resolverCalls)
	}
}

func TestProxy_SumDBNestedRoutesAreCanonicalAndOriginFailuresAre502(t *testing.T) {
	var requests []string
	origin := fakeOrigin(t, func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.RequestURI())
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, "checksum-bytes")
	})
	originURL, err := url.Parse(origin)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(originURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	resolver := webResolverFunc(func(_ context.Context, target *url.URL, requirement destination.DestinationRequirement) (*destination.ResolvedTarget, error) {
		if target.Scheme != "https" || target.Host != "sum.golang.org" || !requirement.RequireRegistered ||
			requirement.Transport != upstream_entity.ProtocolStatic || requirement.Profile != upstream_entity.PackageProfileGoProxy {
			t.Fatalf("resolver target = %s, requirement = %+v", target, requirement)
		}
		mapped := *target
		mapped.Scheme = "http"
		return &destination.ResolvedTarget{
			URL: &mapped, Authority: "sum.golang.org", Host: "sum.golang.org",
			ServerName: "sum.golang.org", DialAddress: net.JoinHostPort("127.0.0.1", port),
		}, nil
	})
	useWebSumDB(t, configuredWebSumDB(), resolver)

	cases := []struct{ request, origin string }{
		{"/proxy.golang.org/sumdb/sum.golang.org/lookup/example.com/mod@v1.0.0?x=1", "/lookup/example.com/mod@v1.0.0?x=1"},
		{"/sumdb/sum.golang.org/tile/8/1/000.p/16", "/tile/8/1/000.p/16"},
	}
	for _, tc := range cases {
		w := request(t, http.MethodGet, tc.request)
		if w.Code != http.StatusTeapot || w.Body.String() != "checksum-bytes" {
			t.Fatalf("GET %s = status %d body %q", tc.request, w.Code, w.Body.String())
		}
	}
	if len(requests) != len(cases) || requests[0] != cases[0].origin || requests[1] != cases[1].origin {
		t.Fatalf("origin requests = %v", requests)
	}

	useWebSumDB(t, configuredWebSumDB(), webResolverFunc(func(context.Context, *url.URL, destination.DestinationRequirement) (*destination.ResolvedTarget, error) {
		return nil, errors.New("origin unavailable")
	}))
	if got := request(t, http.MethodGet, "/sumdb/sum.golang.org/latest").Code; got != http.StatusBadGateway {
		t.Fatalf("origin failure status = %d, want 502", got)
	}
}
