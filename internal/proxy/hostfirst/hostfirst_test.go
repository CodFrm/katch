// 用例放在 _test 包里：访问规则那一条经 api.Router 装上生产的闸，装配形态与 main 一致。
package hostfirst_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/cago-frame/cago/server/mux/muxtest"
	"github.com/gin-gonic/gin"
	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"

	"github.com/CodFrm/katch/internal/api"
	"github.com/CodFrm/katch/internal/model/entity/rule_entity"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/dispatch"
	"github.com/CodFrm/katch/internal/proxy/hostfirst"
	"github.com/CodFrm/katch/internal/repository/rule_repo"
	mock_rule_repo "github.com/CodFrm/katch/internal/repository/rule_repo/mock"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
	mock_upstream_repo "github.com/CodFrm/katch/internal/repository/upstream_repo/mock"
	"github.com/CodFrm/katch/internal/service/rule_svc"
)

var (
	registryOnly = upstream_entity.ProtocolSet{upstream_entity.ProtocolRegistry}
	staticOnly   = upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic}
	both         = upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic, upstream_entity.ProtocolRegistry}
)

// upstreamTable 装一张上游表；表里没有的主机查不到，broken.example 查表报错。
func upstreamTable(t *testing.T, list ...*upstream_entity.Upstream) {
	t.Helper()
	repo := mock_upstream_repo.NewMockUpstreamRepo(gomock.NewController(t))
	for _, u := range list {
		repo.EXPECT().FindByHost(gomock.Any(), u.Host).Return(u, nil).AnyTimes()
	}
	repo.EXPECT().FindByHost(gomock.Any(), "broken.example").
		Return(nil, errors.New("database is locked")).AnyTimes()
	repo.EXPECT().FindByHost(gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	prev := upstream_repo.Upstream()
	upstream_repo.RegisterUpstream(repo)
	t.Cleanup(func() { upstream_repo.RegisterUpstream(prev) })
}

// seen 下游处理器看到的那一个请求。
type seen struct {
	kind  string
	host  string
	rest  string
	path  string
	query string
}

// pull 让请求过一遍中间件，返回下游看到的样子。
func pull(t *testing.T, target string) seen {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(hostfirst.Middleware())
	var got seen
	engine.NoRoute(func(c *gin.Context) {
		kind, host, rest := dispatch.Classify(c.Request.URL.EscapedPath())
		got = seen{kind.String(), host, rest, c.Request.URL.Path, c.Request.URL.RawQuery}
		c.Status(http.StatusNoContent)
	})
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("GET %s = %d，请求没有走到下游", target, w.Code)
	}
	return got
}

func TestMiddleware(t *testing.T) {
	table := []*upstream_entity.Upstream{
		{ID: 1, Host: "ghcr.io", Protocols: registryOnly, Enabled: true},
		{ID: 2, Host: "asia-east1-docker.pkg.dev", Protocols: staticOnly, Enabled: true},
		{ID: 3, Host: "harbor.example", Protocols: both, Enabled: true},
		{ID: 4, Host: "quay.io", Protocols: registryOnly, Enabled: false},
	}

	convey.Convey("开了 registry 协议的上游，主机名在前的写法走 registry", t, func() {
		upstreamTable(t, table...)
		for _, target := range []string{
			"/ghcr.io/v2/o/r/manifests/t",
			"/https://ghcr.io/v2/o/r/manifests/t",
			"/https:/ghcr.io/v2/o/r/manifests/t",
		} {
			got := pull(t, target)
			convey.So(got.kind, convey.ShouldEqual, dispatch.KindRegistry.String())
			convey.So(got.host, convey.ShouldEqual, "ghcr.io")
			convey.So(got.rest, convey.ShouldEqual, "/o/r/manifests/t")
		}
	})

	convey.Convey("同时开了 static 的上游也一样：registry 主机上的 /v2 就是 registry 接口", t, func() {
		upstreamTable(t, table...)
		got := pull(t, "/harbor.example/v2/o/r/manifests/t")
		convey.So(got.kind, convey.ShouldEqual, dispatch.KindRegistry.String())
		convey.So(got.rest, convey.ShouldEqual, "/o/r/manifests/t")
	})

	convey.Convey("查询串与转义序列原样带过去", t, func() {
		upstreamTable(t, table...)
		got := pull(t, "/ghcr.io/v2/o/r%20x/tags/list?n=10&last=a%2Fb")
		convey.So(got.kind, convey.ShouldEqual, dispatch.KindRegistry.String())
		convey.So(got.rest, convey.ShouldEqual, "/o/r%20x/tags/list")
		convey.So(got.path, convey.ShouldEqual, "/v2/ghcr.io/o/r x/tags/list")
		convey.So(got.query, convey.ShouldEqual, "n=10&last=a%2Fb")
	})

	convey.Convey("只开 static 的上游在 /v2/ 之下的对象不动", t, func() {
		upstreamTable(t, table...)
		got := pull(t, "/asia-east1-docker.pkg.dev/v2/p/images/blobs/sha256:abc")
		convey.So(got.kind, convey.ShouldEqual, dispatch.KindStatic.String())
		convey.So(got.host, convey.ShouldEqual, "asia-east1-docker.pkg.dev")
		convey.So(got.rest, convey.ShouldEqual, "/v2/p/images/blobs/sha256:abc")
	})

	convey.Convey("不在表里或已停用的主机不动，照旧交给白名单去答", t, func() {
		upstreamTable(t, table...)
		for _, host := range []string{"unknown.example", "quay.io"} {
			got := pull(t, "/"+host+"/v2/o/r/manifests/t")
			convey.So(got.kind, convey.ShouldEqual, dispatch.KindStatic.String())
			convey.So(got.host, convey.ShouldEqual, host)
			convey.So(got.rest, convey.ShouldEqual, "/v2/o/r/manifests/t")
		}
	})

	convey.Convey("查不了上游表时不改写，请求照常往下走", t, func() {
		upstreamTable(t, table...)
		got := pull(t, "/broken.example/v2/o/r/manifests/t")
		convey.So(got.kind, convey.ShouldEqual, dispatch.KindStatic.String())
		convey.So(got.rest, convey.ShouldEqual, "/v2/o/r/manifests/t")
	})

	convey.Convey("不是这种写法的请求不查表也不动", t, func() {
		// 这张表对任何 FindByHost 都没有 EXPECT：只要查了，mock 当场失败。
		repo := mock_upstream_repo.NewMockUpstreamRepo(gomock.NewController(t))
		prev := upstream_repo.Upstream()
		upstream_repo.RegisterUpstream(repo)
		t.Cleanup(func() { upstream_repo.RegisterUpstream(prev) })
		for target, kind := range map[string]dispatch.Kind{
			"/v2/ghcr.io/o/r/manifests/t":          dispatch.KindRegistry,
			"/registry/ghcr.io/v2/o/r/manifests/t": dispatch.KindRegistry,
			"/deb.debian.org/dists/stable":         dispatch.KindStatic,
			"/api/v1/site":                         dispatch.KindSelf,
			"/settings":                            dispatch.KindSPA,
		} {
			convey.So(pull(t, target).kind, convey.ShouldEqual, kind.String())
		}
	})
}

// TestMiddleware_AccessRulesSeeTheRegistryPath 换一种写法不能绕开访问规则。
//
// 规则是按 registry 的仓库路径写的（private/*）。主机名在前的写法要是到了闸那里
// 还带着 /v2 前缀，这条 deny 就对它不成立——同一个镜像换个地址就拉得到了。
func TestMiddleware_AccessRulesSeeTheRegistryPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstreamTable(t, &upstream_entity.Upstream{
		ID: 1, Host: "ghcr.io", Protocols: registryOnly, Enabled: true,
		DefaultPolicy: upstream_entity.PolicyAllowAll,
	})
	ruleRepo := mock_rule_repo.NewMockAccessRuleRepo(gomock.NewController(t))
	ruleRepo.EXPECT().List(gomock.Any()).Return([]*rule_entity.AccessRule{
		{ID: 1, UpstreamID: 1, Action: rule_entity.ActionDeny, Pattern: "private/*"},
	}, nil).AnyTimes()
	prevRuleRepo := rule_repo.AccessRule()
	rule_repo.RegisterAccessRule(ruleRepo)
	t.Cleanup(func() { rule_repo.RegisterAccessRule(prevRuleRepo) })
	prevRule := rule_svc.Rule()
	rule_svc.Register(rule_svc.New())
	t.Cleanup(func() { rule_svc.Register(prevRule) })

	testMux := muxtest.NewTestMux()
	engine, ok := testMux.IRouter.(*gin.Engine)
	if !ok {
		t.Fatal("muxtest 的 IRouter 不再是 *gin.Engine")
	}
	// 装配顺序和 main 一致：归一排在闸之前。
	engine.Use(hostfirst.Middleware())
	downstream := &atomic.Int64{}
	engine.NoRoute(func(c *gin.Context) {
		downstream.Add(1)
		c.Status(http.StatusNoContent)
	})
	if err := api.Router(context.Background(), testMux.Router); err != nil {
		t.Fatal(err)
	}

	for _, target := range []string{
		"/v2/ghcr.io/private/img/manifests/t",
		"/ghcr.io/v2/private/img/manifests/t",
		"/https://ghcr.io/v2/private/img/manifests/t",
	} {
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
		if w.Code != http.StatusForbidden {
			t.Errorf("GET %s = %d, want 403", target, w.Code)
		}
	}
	if downstream.Load() != 0 {
		t.Errorf("被拒绝的请求走到了下游 %d 次", downstream.Load())
	}

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ghcr.io/v2/public/img/manifests/t", nil))
	if w.Code != http.StatusNoContent || downstream.Load() != 1 {
		t.Errorf("没被规则拒绝的路径 = %d（下游 %d 次），want 204 且走到下游", w.Code, downstream.Load())
	}
}
