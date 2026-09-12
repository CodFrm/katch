package proxy_svc

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/dispatch"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
	mock_upstream_repo "github.com/CodFrm/katch/internal/repository/upstream_repo/mock"
)

func setupRepo(t *testing.T) *mock_upstream_repo.MockUpstreamRepo {
	t.Helper()
	repo := mock_upstream_repo.NewMockUpstreamRepo(gomock.NewController(t))
	// 装配形态与 main 一致：读路径走的是带进程内缓存的那一层，所以用例里
	// 假源站之外唯一被打到的是 List（缓存一次性装载整张表），而不是 FindByHost。
	upstream_repo.RegisterUpstream(NewCachedUpstreamRepo(repo))
	return repo
}

// TestFetch_StaticUpstreamIsPassedThrough
//
// APT 的响应体在 InRelease 的 GPG 签名覆盖范围内，改一个字节整个源就验不过。
func TestFetch_StaticUpstreamIsPassedThrough(t *testing.T) {
	convey.Convey("已启用的 static 上游原样回源", t, func() {
		gotURI := ""
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotURI = r.URL.RequestURI()
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "Origin: Debian\n")
		}))
		defer srv.Close()

		repo := setupRepo(t)
		repo.EXPECT().List(gomock.Any()).Return([]*upstream_entity.Upstream{
			{ID: 1, Host: "deb.debian.org", Kind: upstream_entity.KindStatic, Origin: srv.URL, Enabled: true},
		}, nil).AnyTimes()

		body, meta, err := Proxy().Fetch(context.Background(), &Target{
			Kind: dispatch.KindStatic, Host: "deb.debian.org",
			Path: "/dists/stable/InRelease", Method: http.MethodGet,
		})
		convey.So(err, convey.ShouldBeNil)
		defer func() { _ = body.Close() }()
		b, err := io.ReadAll(body)
		convey.So(err, convey.ShouldBeNil)
		convey.So(string(b), convey.ShouldEqual, "Origin: Debian\n")
		convey.So(gotURI, convey.ShouldEqual, "/dists/stable/InRelease")
		convey.So(meta.StatusCode, convey.ShouldEqual, http.StatusOK)
		convey.So(meta.Header.Get("Content-Type"), convey.ShouldEqual, "text/plain")
	})
}

// TestFetch_RegistryKeepsProtocolPrefix
//
// dispatch 把 /v2 前缀摘掉了（它是协议前缀，不是上游路径的一部分），回源时必须补回去，
// 否则 registry 拿到的是一个没有 /v2 的请求。
func TestFetch_RegistryKeepsProtocolPrefix(t *testing.T) {
	convey.Convey("registry 回源时补回 /v2 前缀", t, func() {
		gotURI := ""
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			gotURI = r.URL.RequestURI()
		}))
		defer srv.Close()

		repo := setupRepo(t)
		repo.EXPECT().List(gomock.Any()).Return([]*upstream_entity.Upstream{
			{ID: 1, Host: "docker.io", Kind: upstream_entity.KindRegistry, Origin: srv.URL, Enabled: true},
		}, nil).AnyTimes()

		body, _, err := Proxy().Fetch(context.Background(), &Target{
			Kind: dispatch.KindRegistry, Host: "docker.io",
			Path: "/library/redis/manifests/7", Method: http.MethodGet,
		})
		convey.So(err, convey.ShouldBeNil)
		_ = body.Close()
		convey.So(gotURI, convey.ShouldEqual, "/v2/library/redis/manifests/7")
	})
}

// TestFetch_NotWhitelisted
//
// 决策 6：上游表就是白名单。不在表里、已停用、以及协议类别对不上的请求，对外
// 必须是同一个 ErrUpstreamNotAllowed——调用方只能回一个不带任何信息的 404，
// 三者之间的任何差别都是在告诉探测者「这个主机存在，只是被停用了」。
func TestFetch_NotWhitelisted(t *testing.T) {
	convey.Convey("不在白名单内的请求一律同一个错误", t, func() {
		table := []*upstream_entity.Upstream{
			{ID: 1, Host: "disabled.example.com", Kind: upstream_entity.KindStatic, Origin: "http://127.0.0.1:1", Enabled: false},
			{ID: 2, Host: "docker.io", Kind: upstream_entity.KindRegistry, Origin: "http://127.0.0.1:1", Enabled: true},
		}
		targets := map[string]*Target{
			"表里没有这个主机":                {Kind: dispatch.KindStatic, Host: "internal.corp.local", Path: "/x", Method: http.MethodGet},
			"表里有但已停用":                 {Kind: dispatch.KindStatic, Host: "disabled.example.com", Path: "/x", Method: http.MethodGet},
			"registry 上游走了 static 路径": {Kind: dispatch.KindStatic, Host: "docker.io", Path: "/x", Method: http.MethodGet},
		}
		for name, target := range targets {
			convey.Convey(name, func() {
				repo := setupRepo(t)
				repo.EXPECT().List(gomock.Any()).Return(table, nil).AnyTimes()

				body, meta, err := Proxy().Fetch(context.Background(), target)
				convey.So(errors.Is(err, ErrUpstreamNotAllowed), convey.ShouldBeTrue)
				convey.So(body, convey.ShouldBeNil)
				convey.So(meta, convey.ShouldBeNil)
				// 错误信息本身也不许带上主机名：它会被写进日志之外的地方。
				convey.So(err.Error(), convey.ShouldNotContainSubstring, target.Host)
			})
		}
	})
}

// TestFetch_UnreachableOriginIsNotA404
//
// 上游不可达要回 502 而不是 404：404 会让客户端认为这个对象不存在并把结论记下来，
// 而这只是一次回源失败。两者必须能被上层区分开。
func TestFetch_UnreachableOriginIsNotA404(t *testing.T) {
	convey.Convey("回源失败与不在白名单是两件事", t, func() {
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
		srv.Close()

		repo := setupRepo(t)
		repo.EXPECT().List(gomock.Any()).Return([]*upstream_entity.Upstream{
			{ID: 1, Host: "deb.debian.org", Kind: upstream_entity.KindStatic, Origin: srv.URL, Enabled: true},
		}, nil).AnyTimes()

		_, _, err := Proxy().Fetch(context.Background(), &Target{
			Kind: dispatch.KindStatic, Host: "deb.debian.org", Path: "/x", Method: http.MethodGet,
		})
		convey.So(err, convey.ShouldNotBeNil)
		convey.So(errors.Is(err, ErrUpstreamNotAllowed), convey.ShouldBeFalse)
	})
}
