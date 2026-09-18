package proxy_svc_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
	mock_upstream_repo "github.com/CodFrm/katch/internal/repository/upstream_repo/mock"
	"github.com/CodFrm/katch/internal/service/cache_svc"
	"github.com/CodFrm/katch/internal/service/proxy_svc"
)

// fakeSumdbLookup 一条 sumdb lookup 的响应形态：go 命令会对它做逐字节校验，
// 所以用例里也按字节比。
const fakeSumdbLookup = "github.com/foo/bar v1.0.0 h1:abc=\n" +
	"github.com/foo/bar v1.0.0/go.mod h1:def=\n\n" +
	"go.sum database tree\n42\n"

// useGoProxyUpstream 把 Go module proxy 与固定 checksum database 都装成进程内上游。
// 两条公开路径会在 dispatch 收敛到各自真正的目标主机。
func useGoProxyUpstream(t *testing.T, origin string) {
	t.Helper()
	repo := mock_upstream_repo.NewMockUpstreamRepo(gomock.NewController(t))
	repo.EXPECT().List(gomock.Any()).Return([]*upstream_entity.Upstream{
		{
			ID: 4, Host: "proxy.golang.org", Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic}, Origin: origin,
			Enabled: true, DefaultPolicy: upstream_entity.PolicyAllowAll,
		},
		{
			ID: 5, Host: "sum.golang.org", Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic}, Origin: origin,
			Enabled: true, DefaultPolicy: upstream_entity.PolicyAllowAll,
			PackageProfile: upstream_entity.PackageProfileGoProxy,
		},
	}, nil).AnyTimes()
	upstream_repo.RegisterUpstream(proxy_svc.NewCachedUpstreamRepo(repo))

	prevProxy := proxy_svc.Proxy()
	proxy_svc.Register(proxy_svc.New(proxy_svc.Options{DestinationResolver: localDestinationResolver{}}))
	t.Cleanup(func() { proxy_svc.Register(prevProxy) })
	// 缓存层用出厂的纯透传形态：这一组用例问的是路径有没有被改写，不是缓存。
	prevCache := cache_svc.Cache()
	cache_svc.Register(cache_svc.New(nil, cache_svc.Options{}))
	t.Cleanup(func() { cache_svc.Register(prevCache) })
}

// TestPull_GoProxySumdbIsProxied
//
// Go 客户端把 checksum 请求放在 GOPROXY 的 /sumdb/ 子路径下；dispatch 必须把它
// 收敛为 sum.golang.org 的 canonical target，模块本体仍归 proxy.golang.org。
func TestPull_GoProxySumdbIsProxied(t *testing.T) {
	convey.Convey("/sumdb/ 子路径按 canonical 路径代理给 sum.golang.org", t, func() {
		// 断言不写在假源站的 handler 里：它跑在另一个 goroutine 上，
		// convey.So 脱离 Convey 栈会 panic，表现成一次假的「回源失败」。
		var gotURIs []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotURIs = append(gotURIs, r.URL.RequestURI())
			w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
			_, _ = io.WriteString(w, fakeSumdbLookup)
		}))
		defer srv.Close()
		useGoProxyUpstream(t, srv.URL)
		handler := pullHandler()

		w := httptest.NewRecorder()
		handler(w, httptest.NewRequest(http.MethodGet,
			"/proxy.golang.org/sumdb/sum.golang.org/lookup/github.com/foo/bar@v1.0.0", nil))

		convey.Convey("checksum 上游收到去掉公开别名后的 canonical 路径", func() {
			convey.So(gotURIs, convey.ShouldResemble, []string{
				"/lookup/github.com/foo/bar@v1.0.0",
			})
		})
		convey.Convey("响应体逐字节回给客户端：go 命令要拿它去校验", func() {
			convey.So(w.Code, convey.ShouldEqual, http.StatusOK)
			convey.So(w.Body.String(), convey.ShouldEqual, fakeSumdbLookup)
		})

		convey.Convey("同一条上游上模块本体的路径同样原样送达", func() {
			// Go 的模块路径把大写字母写成 !x，还带一个 @v 段。这些字符在转义形态里
			// 就是它们自己，先解码再拼回去会把它们改写成 %21/%40，上游要么 404
			// 要么给回另一个模块。
			second := httptest.NewRecorder()
			handler(second, httptest.NewRequest(http.MethodGet,
				"/proxy.golang.org/github.com/!burnt!sushi/toml/@v/list", nil))

			convey.So(second.Code, convey.ShouldEqual, http.StatusOK)
			convey.So(gotURIs, convey.ShouldResemble, []string{
				"/lookup/github.com/foo/bar@v1.0.0",
				"/github.com/!burnt!sushi/toml/@v/list",
			})
		})
	})
}
