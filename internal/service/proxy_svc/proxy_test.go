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
	"github.com/CodFrm/katch/internal/proxy/destination"
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

func TestFetch_PackageOriginRejectsPrivateDial(t *testing.T) {
	convey.Convey("package profile 的显式 origin 也必须满足公网地址策略", t, func() {
		var hits int
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			hits++
		}))
		defer srv.Close()

		repo := setupRepo(t)
		repo.EXPECT().List(gomock.Any()).Return([]*upstream_entity.Upstream{{
			ID: 1, Host: "packages.example.com", Origin: srv.URL, Enabled: true,
			Protocols:      upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
			PackageProfile: upstream_entity.PackageProfileNPM,
		}}, nil).AnyTimes()

		svc := New(Options{DestinationResolver: destination.New(destination.Options{})})
		body, meta, err := svc.Fetch(context.Background(), &Target{
			Kind: dispatch.KindStatic, Host: "packages.example.com", Path: "/x", Method: http.MethodGet,
		})
		convey.So(errors.Is(err, destination.ErrDestinationNotAllowed), convey.ShouldBeTrue)
		convey.So(body, convey.ShouldBeNil)
		convey.So(meta, convey.ShouldBeNil)
		convey.So(hits, convey.ShouldEqual, 0)
	})
}

func TestDestinationConfigSourceUsesCoherentRewriteSnapshot(t *testing.T) {
	source := &staticRewriteSource{snapshot: &RewriteSnapshot{Upstreams: map[string]RewriteUpstream{
		"cdn.example.com": {
			Profile:    upstream_entity.PackageProfileNPM,
			Transports: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
		},
	}}}
	got, err := (destinationConfigSource{source: source}).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	upstream := got.Upstreams["cdn.example.com"]
	if upstream.Profile != upstream_entity.PackageProfileNPM ||
		!upstream.Transports.Has(upstream_entity.ProtocolStatic) {
		t.Fatalf("converted upstream = %+v", upstream)
	}
}

type staticRewriteSource struct {
	snapshot *RewriteSnapshot
}

func (s *staticRewriteSource) Snapshot(context.Context) (*RewriteSnapshot, error) {
	return s.snapshot, nil
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
			{ID: 1, Host: "deb.debian.org", Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic}, Origin: srv.URL, Enabled: true},
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
			{ID: 1, Host: "docker.io", Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolRegistry}, Origin: srv.URL, Enabled: true},
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
			{ID: 1, Host: "disabled.example.com", Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic}, Origin: "http://127.0.0.1:1", Enabled: false},
			{ID: 2, Host: "docker.io", Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolRegistry}, Origin: "http://127.0.0.1:1", Enabled: true},
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
			{ID: 1, Host: "deb.debian.org", Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic}, Origin: srv.URL, Enabled: true},
		}, nil).AnyTimes()

		_, _, err := Proxy().Fetch(context.Background(), &Target{
			Kind: dispatch.KindStatic, Host: "deb.debian.org", Path: "/x", Method: http.MethodGet,
		})
		convey.So(err, convey.ShouldNotBeNil)
		convey.So(errors.Is(err, ErrUpstreamNotAllowed), convey.ShouldBeFalse)
	})
}

// TestFetch_DropsAuthenticationChallenge 上游的鉴权挑战不许出现在 katch 的响应里。
//
// registry 那一侧已经明说过这条规则（registry.strip：「任何一条从 katch 出去的
// WWW-Authenticate 都是在请客户端向 katch 鉴权，而 katch 是一个公开的镜像站，
// 没有账号可以给它」），但那只盖住了两条回源方式里的一条。
//
// static 那一条同样会遇到 401：一个挂在 basic auth 后面的 apt 源、一台配错了权限的
// 对象存储，都会把 WWW-Authenticate 原样送回来。它抵达客户端之后，浏览器会为
// **katch 的域名**弹一个账号密码框——用户敲进去的凭据是交给 katch 的，而 katch 既
// 没有账号体系，也不该收到任何人的密码。
//
// 收口放在这一层而不是 origin：registry 适配器要靠这个头解析出 realm 才换得到
// token（决策 5），在 origin 那里摘掉会把 token 交换整个打断。这里是两条回源方式
// 汇合、且适配器已经用完这个头之后的那一点。
func TestFetch_DropsAuthenticationChallenge(t *testing.T) {
	convey.Convey("static 上游的 WWW-Authenticate 不跟着响应出去", t, func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("WWW-Authenticate", `Basic realm="private apt"`)
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer srv.Close()

		repo := setupRepo(t)
		repo.EXPECT().List(gomock.Any()).Return([]*upstream_entity.Upstream{
			{ID: 1, Host: "apt.private.test", Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
				Origin: srv.URL, Enabled: true},
		}, nil).AnyTimes()

		body, meta, err := Proxy().Fetch(context.Background(), &Target{
			Kind: dispatch.KindStatic, Host: "apt.private.test",
			Path: "/dists/stable/InRelease", Method: http.MethodGet,
		})
		convey.So(err, convey.ShouldBeNil)
		defer func() { _ = body.Close() }()
		// 401 本身照常透传：那是上游对这个对象的判断，katch 不替它改口径。
		convey.So(meta.StatusCode, convey.ShouldEqual, http.StatusUnauthorized)
		// 摘掉的只是「向谁鉴权」这句话。
		convey.So(meta.Header.Get("WWW-Authenticate"), convey.ShouldEqual, "")
	})
}
