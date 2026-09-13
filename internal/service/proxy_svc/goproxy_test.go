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

// useGoProxyUpstream 把一条 proxy.golang.org 形态的 static 上游装成进程内那一份。
//
// Go module proxy 在决策 12 里没有自己的协议类别：它就是一条 static 上游，
// 路径尾巴原样转发——`/sumdb/` 能被代理靠的正是这一点，而不是哪段专门的代码。
func useGoProxyUpstream(t *testing.T, origin string) {
	t.Helper()
	repo := mock_upstream_repo.NewMockUpstreamRepo(gomock.NewController(t))
	repo.EXPECT().List(gomock.Any()).Return([]*upstream_entity.Upstream{{
		ID: 4, Host: "proxy.golang.org", Kind: upstream_entity.KindStatic, Origin: origin,
		Enabled: true, DefaultPolicy: upstream_entity.PolicyAllowAll,
	}}, nil).AnyTimes()
	upstream_repo.RegisterUpstream(proxy_svc.NewCachedUpstreamRepo(repo))

	prevProxy := proxy_svc.Proxy()
	proxy_svc.Register(proxy_svc.New(proxy_svc.Options{}))
	t.Cleanup(func() { proxy_svc.Register(prevProxy) })
	// 缓存层用出厂的纯透传形态：这一组用例问的是路径有没有被改写，不是缓存。
	prevCache := cache_svc.Cache()
	cache_svc.Register(cache_svc.New(nil, cache_svc.Options{}))
	t.Cleanup(func() { cache_svc.Register(prevCache) })
}

// TestPull_GoProxySumdbIsProxied
//
// `/sumdb/` 是 GOPROXY 协议的一部分，缺了它 go 命令验不了 go.sum，整条上游等于废掉。
// 它没有任何专门的代码，靠的是 static 上游把路径尾巴原样往上游送——所以这里钉住的是
// 「尾巴一个字节都没被改」：不补 /v2 协议前缀（那是 registry 的事）、不重新编码、
// 不因为第一段之后还有一个含点的主机名（sum.golang.org）就再分一次段。
func TestPull_GoProxySumdbIsProxied(t *testing.T) {
	convey.Convey("/sumdb/ 子路径原样代理给 proxy.golang.org", t, func() {
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

		convey.Convey("上游收到的就是摘掉主机名之后那一段，原封不动", func() {
			convey.So(gotURIs, convey.ShouldResemble, []string{
				"/sumdb/sum.golang.org/lookup/github.com/foo/bar@v1.0.0",
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
				"/sumdb/sum.golang.org/lookup/github.com/foo/bar@v1.0.0",
				"/github.com/!burnt!sushi/toml/@v/list",
			})
		})
	})
}
