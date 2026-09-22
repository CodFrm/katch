// 用例放在 _test 包里：registry 的换 token 要从**客户端看到的响应**判定，
// 而客户端那一侧的形态（状态码 + 响应头 + 响应体）只有把 cache_svc 叠在
// proxy_svc 上、再套一层拉取处理器才说得清，而 cache_svc 反过来依赖 proxy_svc。
package proxy_svc_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/destination"
	"github.com/CodFrm/katch/internal/proxy/dispatch"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
	mock_upstream_repo "github.com/CodFrm/katch/internal/repository/upstream_repo/mock"
	"github.com/CodFrm/katch/internal/service/cache_svc"
	"github.com/CodFrm/katch/internal/service/proxy_svc"
)

const fakeManifest = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`

// fakeRegistry 一个会 401 的假 registry，外加一个会数数的假 token 端点。
//
// 两台服务器分开，是因为真实的 registry 就是这个形状（docker.io 的 token 端点在
// auth.docker.io 上），而且「token 被复用了」只有在 token 端点独立计数时才判得出来。
type fakeRegistry struct {
	srv      *httptest.Server
	tokenSrv *httptest.Server
	// tokenHits token 端点被打了几次。复用成立的判据就是它不再增长。
	tokenHits atomic.Int64
	// paths registry 收到的上游侧路径，按顺序记下——library/ 补全只能在这里看见。
	paths  []string
	scopes []string
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{}
	const secret = "issued-token-abc"

	f.tokenSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.tokenHits.Add(1)
		f.scopes = append(f.scopes, r.URL.Query().Get("scope"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"token":"`+secret+`","expires_in":300,"issued_at":"2026-09-12T00:00:00Z"}`)
	}))
	t.Cleanup(f.tokenSrv.Close)

	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.paths = append(f.paths, r.URL.RequestURI())
		if r.Header.Get("Authorization") != "Bearer "+secret {
			// 真实 registry 的形态：401 + 一条指向 token 端点的挑战。
			repo := strings.TrimPrefix(r.URL.Path, "/v2/")
			repo = repo[:strings.LastIndex(repo, "/manifests/")]
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+f.tokenSrv.URL+
				`/token",service="registry.example.com",scope="repository:`+repo+`:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"errors":[{"code":"UNAUTHORIZED"}]}`)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", "sha256:deadbeef")
		_, _ = io.WriteString(w, fakeManifest)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

type localDestinationResolver struct{}

func (localDestinationResolver) Resolve(
	_ context.Context, target *url.URL, _ destination.DestinationRequirement,
) (*destination.ResolvedTarget, error) {
	cloned := *target
	return &destination.ResolvedTarget{URL: &cloned, Authority: target.Host, Host: target.Host,
		ServerName: target.Hostname(), DialAddress: target.Host}, nil
}

// useRegistryUpstream 把一条 docker.io 形态的 registry 上游装成进程内那一份。
//
// LibraryCompletion 是记录上的一个标志（决策 12：协议类别只有两种，其余差异
// 由记录表达），适配器不认识「docker.io」这个主机名。
func useRegistryUpstream(t *testing.T, origin string, libraryCompletion bool) {
	t.Helper()
	repo := mock_upstream_repo.NewMockUpstreamRepo(gomock.NewController(t))
	repo.EXPECT().List(gomock.Any()).Return([]*upstream_entity.Upstream{{
		ID: 3, Host: "docker.io", Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolRegistry}, Origin: origin,
		Enabled: true, DefaultPolicy: upstream_entity.PolicyAllowAll,
		LibraryCompletion: libraryCompletion,
	}}, nil).AnyTimes()
	upstream_repo.RegisterUpstream(proxy_svc.NewCachedUpstreamRepo(repo))

	prevProxy := proxy_svc.Proxy()
	proxy_svc.Register(proxy_svc.New(proxy_svc.Options{
		DestinationResolver: localDestinationResolver{},
	}))
	t.Cleanup(func() { proxy_svc.Register(prevProxy) })
	// 缓存层用出厂的纯透传形态：这一组用例问的是鉴权，不是缓存。
	prevCache := cache_svc.Cache()
	cache_svc.Register(cache_svc.New(nil, cache_svc.Options{}))
	t.Cleanup(func() { cache_svc.Register(prevCache) })
}

// pullHandler 拉取路径处理器的替身，映射与 internal/web 的那一个逐条对齐：
// 入口是 cache_svc.Get，上游响应头原样复制、状态码原样透传。
// 它的构造器不导出，所以这里照着写一份——判据仍然是客户端拿到的那个响应。
func pullHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		kind, host, rest := dispatch.Classify(r.URL.EscapedPath())
		if kind != dispatch.KindRegistry && kind != dispatch.KindStatic && kind != dispatch.KindSumDB {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, meta, err := cache_svc.Cache().Get(r.Context(), &proxy_svc.Target{
			Kind: kind, Host: host, Path: rest, RawQuery: r.URL.RawQuery,
			Method: r.Method, Header: r.Header,
		})
		if err != nil {
			if errors.Is(err, proxy_svc.ErrUpstreamNotAllowed) {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer func() { _ = body.Close() }()
		for k, values := range meta.Header {
			for _, v := range values {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(meta.StatusCode)
		_, _ = io.Copy(w, body)
	}
}

// TestPull_RegistryTokenExchange 任务目标的四段：401 → 换 token → 以 library/redis
// 重试 → 无鉴权地把 manifest 交给客户端，且第二次请求复用同一个 token。
func TestPull_RegistryTokenExchange(t *testing.T) {
	convey.Convey("对返回 401 的 registry 自行换取 token 并补全 library/", t, func() {
		f := newFakeRegistry(t)
		useRegistryUpstream(t, f.srv.URL, true)
		handler := pullHandler()

		w := httptest.NewRecorder()
		handler(w, httptest.NewRequest(http.MethodGet, "/v2/docker.io/redis/manifests/7", nil))

		convey.Convey("客户端拿到的是 manifest 本身，而不是 401", func() {
			convey.So(w.Code, convey.ShouldEqual, http.StatusOK)
			convey.So(w.Body.String(), convey.ShouldEqual, fakeManifest)
			convey.So(w.Header().Get("Content-Type"), convey.ShouldEqual,
				"application/vnd.oci.image.manifest.v1+json")
		})
		convey.Convey("挑战不许漏给客户端：漏了 docker 会转头来向 katch 鉴权", func() {
			convey.So(w.Header().Get("WWW-Authenticate"), convey.ShouldBeBlank)
		})
		convey.Convey("打给上游的一直是补全后的 library/redis，重试也是", func() {
			// 补全不是「重试时才做」的补救：使用者写 docker.io/redis，向上游
			// 请求的就是 library/redis，第一发就该是它。
			convey.So(f.paths, convey.ShouldResemble, []string{
				"/v2/library/redis/manifests/7", "/v2/library/redis/manifests/7",
			})
		})
		convey.Convey("token 是 katch 自己换的，scope 用补全后的仓库名", func() {
			convey.So(f.tokenHits.Load(), convey.ShouldEqual, 1)
			convey.So(f.scopes, convey.ShouldResemble, []string{"repository:library/redis:pull"})
		})

		convey.Convey("下一次同仓库的请求复用缓存的 token", func() {
			second := httptest.NewRecorder()
			handler(second, httptest.NewRequest(http.MethodGet,
				"/v2/docker.io/redis/manifests/7", nil))

			convey.So(second.Code, convey.ShouldEqual, http.StatusOK)
			convey.So(second.Body.String(), convey.ShouldEqual, fakeManifest)
			convey.Convey("token 端点没有被第二次打到", func() {
				convey.So(f.tokenHits.Load(), convey.ShouldEqual, 1)
			})
			convey.Convey("也没有再吃一次 401：第一发就带着 token 出去", func() {
				convey.So(f.paths, convey.ShouldResemble, []string{
					"/v2/library/redis/manifests/7",
					"/v2/library/redis/manifests/7",
					"/v2/library/redis/manifests/7",
				})
			})
		})
	})
}
