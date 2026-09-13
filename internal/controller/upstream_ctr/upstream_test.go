// 用例放在 _test 包里：它经 api.Router 注册真实路由，而 api 包又依赖本包，
// 同包会构成导入环。走 _test 包换来的是「测的就是生产路由 + 生产中间件」。
package upstream_ctr_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cago-frame/cago/server/mux/muxclient"
	"github.com/cago-frame/cago/server/mux/muxtest"
	"github.com/gin-gonic/gin"
	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"
	"golang.org/x/crypto/bcrypt"

	"github.com/CodFrm/katch/internal/api"
	"github.com/CodFrm/katch/internal/api/admin"
	api_upstream "github.com/CodFrm/katch/internal/api/upstream"
	"github.com/CodFrm/katch/internal/model/entity/rollup_entity"
	"github.com/CodFrm/katch/internal/model/entity/setting_entity"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/backoff"
	"github.com/CodFrm/katch/internal/repository/cache_repo"
	mock_cache_repo "github.com/CodFrm/katch/internal/repository/cache_repo/mock"
	"github.com/CodFrm/katch/internal/repository/rollup_repo"
	mock_rollup_repo "github.com/CodFrm/katch/internal/repository/rollup_repo/mock"
	"github.com/CodFrm/katch/internal/repository/setting_repo"
	mock_setting_repo "github.com/CodFrm/katch/internal/repository/setting_repo/mock"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
	mock_upstream_repo "github.com/CodFrm/katch/internal/repository/upstream_repo/mock"
	"github.com/CodFrm/katch/internal/service/setting_svc"
	"github.com/CodFrm/katch/internal/service/stat_svc"
)

const adminKey = "correct-admin-key"

// setupAdminTest 装配一套「repo 是 mock、路由与中间件是生产那套」的测试环境。
func setupAdminTest(t *testing.T) (*mock_upstream_repo.MockUpstreamRepo, *mock_setting_repo.MockSettingRepo, *muxtest.TestMux, *gin.Engine) {
	t.Helper()
	up, set, _, testMux, engine := setupTest(t, nil)
	return up, set, testMux, engine
}

// setupTest 同上，外加公开列表要用的那两样：分钟桶 repo 与退避状态。
// 首页页脚的上游表要标出命中率与状态，这两样分别来自 traffic_rollup 与内存里
// 的退避（决策 16、17）。
func setupTest(t *testing.T, degraded stat_svc.DegradeReporter) (
	*mock_upstream_repo.MockUpstreamRepo, *mock_setting_repo.MockSettingRepo,
	*publicDeps, *muxtest.TestMux, *gin.Engine,
) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctrl := gomock.NewController(t)
	upRepo := mock_upstream_repo.NewMockUpstreamRepo(ctrl)
	upstream_repo.RegisterUpstream(upRepo)
	public := &publicDeps{
		rollup: mock_rollup_repo.NewMockTrafficRollupRepo(ctrl),
		cache:  mock_cache_repo.NewMockCacheObjectRepo(ctrl),
	}
	rollup_repo.RegisterTrafficRollup(public.rollup)
	cache_repo.RegisterCacheObject(public.cache)
	stat_svc.Register(stat_svc.New(stat_svc.Options{Degraded: degraded}))
	setRepo := mock_setting_repo.NewMockSettingRepo(ctrl)
	setting_repo.RegisterSetting(setRepo)
	// MinCost：用例只关心比对结果，不关心 bcrypt 的计算代价。
	hash, err := bcrypt.GenerateFromPassword([]byte(adminKey), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	setRepo.EXPECT().Find(gomock.Any(), setting_svc.AdminKeyHashSetting).Return(
		&setting_entity.Setting{Key: setting_svc.AdminKeyHashSetting, Value: string(hash)}, nil,
	).AnyTimes()

	testMux := muxtest.NewTestMux()
	if err := api.Router(context.Background(), testMux.Router); err != nil {
		t.Fatal(err)
	}
	engine, ok := testMux.IRouter.(*gin.Engine)
	if !ok {
		t.Fatal("muxtest 的 IRouter 不再是 *gin.Engine")
	}
	return upRepo, setRepo, public, testMux, engine
}

// publicDeps 公开列表额外要的两个 repo：命中率与状态来自 traffic_rollup 加
// 内存里的退避，缓存量来自 cache_object。
type publicDeps struct {
	rollup *mock_rollup_repo.MockTrafficRollupRepo
	cache  *mock_cache_repo.MockCacheObjectRepo
}

func adminHeader(key string) muxclient.ClientDoOption {
	return muxclient.WithHeader(http.Header{"Authorization": []string{"Bearer " + key}})
}

// TestUpstreamSaveAndList 覆盖任务目标 (a)：带正确密钥创建的上游能被 GET 读回，
// 且每个字段都原样穿过 controller → service → repository 的映射。
func TestUpstreamSaveAndList(t *testing.T) {
	upRepo, _, testMux, _ := setupAdminTest(t)
	convey.Convey("带正确密钥创建的上游能被列表读到", t, func() {
		var stored *upstream_entity.Upstream
		upRepo.EXPECT().FindByHost(gomock.Any(), "deb.debian.org").Return(nil, nil)
		upRepo.EXPECT().Save(gomock.Any(), gomock.Any()).DoAndReturn(
			func(_ context.Context, up *upstream_entity.Upstream) error {
				up.ID = 7
				stored = up
				return nil
			})

		saveResp := &admin.SaveUpstreamResponse{}
		err := testMux.Do(context.Background(), &admin.SaveUpstreamRequest{
			Host:              "deb.debian.org",
			Kind:              "static",
			Origin:            "https://deb.debian.org",
			Enabled:           true,
			ImmutablePatterns: []string{"pool/"},
			MutableTTLSeconds: 300,
			DefaultPolicy:     "allow_all",
			Note:              "Debian 官方源",
		}, saveResp, adminHeader(adminKey))
		convey.So(err, convey.ShouldBeNil)
		convey.So(saveResp.ID, convey.ShouldEqual, 7)
		convey.So(stored, convey.ShouldNotBeNil)
		convey.So(stored.Createtime, convey.ShouldBeGreaterThan, 0)

		upRepo.EXPECT().List(gomock.Any()).Return([]*upstream_entity.Upstream{stored}, nil)
		listResp := &admin.ListUpstreamsResponse{}
		convey.So(testMux.Do(context.Background(), &admin.ListUpstreamsRequest{}, listResp,
			adminHeader(adminKey)), convey.ShouldBeNil)
		convey.So(len(listResp.List), convey.ShouldEqual, 1)
		item := listResp.List[0]
		convey.So(item.ID, convey.ShouldEqual, 7)
		convey.So(item.Host, convey.ShouldEqual, "deb.debian.org")
		convey.So(item.Kind, convey.ShouldEqual, "static")
		convey.So(item.Origin, convey.ShouldEqual, "https://deb.debian.org")
		convey.So(item.Enabled, convey.ShouldBeTrue)
		convey.So(item.ImmutablePatterns, convey.ShouldResemble, []string{"pool/"})
		convey.So(item.MutableTTLSeconds, convey.ShouldEqual, 300)
		convey.So(item.DefaultPolicy, convey.ShouldEqual, "allow_all")
		convey.So(item.Note, convey.ShouldEqual, "Debian 官方源")
	})
}

// TestUpstreamAdminAuth 覆盖任务目标 (b)：未提供密钥与密钥错误的响应必须逐字节相同，
// 否则探测者能靠响应差异确认「这个密钥名对了、只是值不对」。
// upRepo 上一个 EXPECT 都没有：一旦鉴权放行进 service，mock 会当场让用例失败。
func TestUpstreamAdminAuth(t *testing.T) {
	_, _, testMux, engine := setupAdminTest(t)
	convey.Convey("管理接口的密钥校验", t, func() {
		do := func(header http.Header) *httptest.ResponseRecorder {
			opts := []muxclient.ClientDoOption{}
			if header != nil {
				opts = append(opts, muxclient.WithHeader(header))
			}
			req, err := testMux.Request(context.Background(), &admin.ListUpstreamsRequest{}, opts...)
			convey.So(err, convey.ShouldBeNil)
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)
			return w
		}
		missing := do(nil)
		wrong := do(http.Header{"Authorization": []string{"Bearer wrong-admin-key"}})

		convey.Convey("未提供密钥返回 401", func() {
			convey.So(missing.Code, convey.ShouldEqual, http.StatusUnauthorized)
		})
		convey.Convey("密钥错误返回 401", func() {
			convey.So(wrong.Code, convey.ShouldEqual, http.StatusUnauthorized)
		})
		convey.Convey("两者的状态码、响应体、响应头完全一致", func() {
			convey.So(wrong.Code, convey.ShouldEqual, missing.Code)
			convey.So(wrong.Body.String(), convey.ShouldEqual, missing.Body.String())
			convey.So(wrong.Header(), convey.ShouldResemble, missing.Header())
		})
		convey.Convey("响应里不出现 WWW-Authenticate 这类可用于区分的线索", func() {
			convey.So(missing.Header().Get("WWW-Authenticate"), convey.ShouldBeBlank)
			convey.So(wrong.Header().Get("WWW-Authenticate"), convey.ShouldBeBlank)
		})
	})
}

// TestUpstreamSaveDuplicateHost host 是白名单的 key，重复注册必须被挡住，
// 否则同一个 host 会有两条记录，分发时命中哪条取决于查询顺序。
func TestUpstreamSaveDuplicateHost(t *testing.T) {
	upRepo, _, testMux, _ := setupAdminTest(t)
	convey.Convey("host 已存在时拒绝创建", t, func() {
		upRepo.EXPECT().FindByHost(gomock.Any(), "docker.io").Return(
			&upstream_entity.Upstream{ID: 3, Host: "docker.io"}, nil)
		err := testMux.Do(context.Background(), &admin.SaveUpstreamRequest{
			Host: "docker.io", Kind: "registry", Origin: "https://registry-1.docker.io",
		}, &admin.SaveUpstreamResponse{}, adminHeader(adminKey))
		convey.So(err, convey.ShouldNotBeNil)
	})
}

// TestUpstreamDelete 删除走的是 id，且不存在的 id 不能被当作删除成功。
func TestUpstreamDelete(t *testing.T) {
	upRepo, _, testMux, _ := setupAdminTest(t)
	convey.Convey("删除上游", t, func() {
		// 删除前会先列一次上游：事件流要记下被删的是哪个主机名，而删完那条
		// 记录就没了（见 upstream_ctr 的 hostOf）。
		upRepo.EXPECT().List(gomock.Any()).Return([]*upstream_entity.Upstream{
			{ID: 7, Host: "deb.debian.org"},
		}, nil).AnyTimes()
		convey.Convey("存在时删除成功", func() {
			upRepo.EXPECT().Find(gomock.Any(), int64(7)).Return(&upstream_entity.Upstream{ID: 7}, nil)
			upRepo.EXPECT().Delete(gomock.Any(), int64(7)).Return(nil)
			convey.So(testMux.Do(context.Background(), &admin.DeleteUpstreamRequest{ID: 7},
				&admin.DeleteUpstreamResponse{}, adminHeader(adminKey)), convey.ShouldBeNil)
		})
		convey.Convey("不存在时报错", func() {
			upRepo.EXPECT().Find(gomock.Any(), int64(404)).Return(nil, nil)
			convey.So(testMux.Do(context.Background(), &admin.DeleteUpstreamRequest{ID: 404},
				&admin.DeleteUpstreamResponse{}, adminHeader(adminKey)), convey.ShouldNotBeNil)
		})
	})
}

// TestPublicUpstreamList 覆盖「首页要答得出支持哪些上游」：这个列表不要密钥，
// 但它是站点名片而不是运维台账——回源地址、默认策略、不可变模式都是运营数据，
// 泄漏出去等于把这台镜像站的内部配置摊开给任何人。
func TestPublicUpstreamList(t *testing.T) {
	// deb.debian.org 此刻在退避里：页脚那张表要答的不只是「支不支持」，
	// 还有「现在好不好用」。
	tracker := backoff.New(backoff.Options{Threshold: 1, Base: time.Minute})
	tracker.Failure("deb.debian.org")
	upRepo, setRepo, public, _, engine := setupTest(t, tracker)
	convey.Convey("匿名请求公开上游列表", t, func() {
		setRepo.EXPECT().Find(gomock.Any(), setting_svc.PublicHomepageSetting).Return(nil, nil)
		// 区间边界由服务层按当前时刻算，这里只关心响应契约。
		public.rollup.EXPECT().SumByUpstream(gomock.Any(), gomock.Any(), gomock.Any()).Return(
			[]*rollup_entity.Totals{
				{UpstreamID: 7, Requests: 100, Hits: 60, Denied: 10, OriginErrors: 10},
			}, nil).AnyTimes()
		public.cache.EXPECT().SizeByUpstream(gomock.Any()).Return(map[int64]int64{7: 4096}, nil).AnyTimes()
		upRepo.EXPECT().List(gomock.Any()).AnyTimes().Return([]*upstream_entity.Upstream{
			{
				ID: 7, Host: "deb.debian.org", Kind: "static", Origin: "https://deb.debian.org",
				Enabled: true, ImmutablePatterns: upstream_entity.PatternList{"pool/"},
				MutableTTLSeconds: 300, DefaultPolicy: "deny_unless_matched", Note: "内部备注",
			},
			{
				ID: 8, Host: "docker.io", Kind: "registry", Origin: "https://registry-1.docker.io",
				Enabled: true, LibraryCompletion: true,
			},
			{ID: 9, Host: "paused.example.com", Kind: "static", Origin: "https://paused.example.com"},
		}, nil)

		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/upstreams", nil))
		convey.So(w.Code, convey.ShouldEqual, http.StatusOK)

		var resp struct {
			Data struct {
				List []map[string]any `json:"list"`
			} `json:"data"`
		}
		convey.So(json.Unmarshal(w.Body.Bytes(), &resp), convey.ShouldBeNil)

		convey.Convey("只列启用中的上游", func() {
			convey.So(len(resp.Data.List), convey.ShouldEqual, 2)
			convey.So(resp.Data.List[0]["host"], convey.ShouldEqual, "deb.debian.org")
			convey.So(resp.Data.List[0]["kind"], convey.ShouldEqual, "static")
			convey.So(resp.Data.List[1]["host"], convey.ShouldEqual, "docker.io")
			convey.So(resp.Data.List[1]["library_completion"], convey.ShouldBeTrue)
			convey.So(w.Body.String(), convey.ShouldNotContainSubstring, "paused.example.com")
		})

		convey.Convey("每一项都带上命中率、缓存量与状态", func() {
			// 命中率是 hits/(hits+misses)：被规则挡住的 10 次和回源失败的 10 次
			// 都没问过缓存，算进分母会让一次上游故障看起来像缓存变差了。
			convey.So(resp.Data.List[0]["hit_rate"], convey.ShouldAlmostEqual, 0.75, 1e-9)
			convey.So(resp.Data.List[0]["status"], convey.ShouldEqual, api_upstream.StatusDegraded)
			// 一次请求都没有、也没在退避里的上游是 normal 加 0 命中率，
			// 而不是干脆没有这两个字段。
			convey.So(resp.Data.List[1]["hit_rate"], convey.ShouldEqual, float64(0))
			convey.So(resp.Data.List[1]["status"], convey.ShouldEqual, api_upstream.StatusNormal)
			// 缓存量是页脚那张表的第三样：没缓存过的上游给 0，而不是没有这个字段。
			convey.So(resp.Data.List[0]["cache_bytes"], convey.ShouldEqual, float64(4096))
			convey.So(resp.Data.List[1]["cache_bytes"], convey.ShouldEqual, float64(0))
		})

		convey.Convey("运营字段一个都不出现在响应里", func() {
			// 断在整个响应体上而不是逐个字段比对：日后谁往这个结构上加一个
			// origin/default_policy/immutable_patterns 字段，这里就会红。
			body := w.Body.String()
			convey.So(body, convey.ShouldNotContainSubstring, "origin")
			convey.So(body, convey.ShouldNotContainSubstring, "default_policy")
			convey.So(body, convey.ShouldNotContainSubstring, "immutable_patterns")
			convey.So(body, convey.ShouldNotContainSubstring, "内部备注")
		})
	})
}

// TestPublicUpstreamListHidden 覆盖「是否公开上游列表由设置控制」：关掉之后
// 匿名调用方看不到这个端点，而带管理密钥的调用方照常读得到。
func TestPublicUpstreamListHidden(t *testing.T) {
	convey.Convey("关掉公开首页之后的上游列表", t, func() {
		convey.Convey("匿名请求看不到这个端点", func() {
			// 上游 repo 上一个 EXPECT 都没有：闸一旦漏放行进 service，mock 会当场失败。
			_, setRepo, _, engine := setupAdminTest(t)
			setRepo.EXPECT().Find(gomock.Any(), setting_svc.PublicHomepageSetting).Return(
				&setting_entity.Setting{Key: setting_svc.PublicHomepageSetting, Value: "false"}, nil)

			w := httptest.NewRecorder()
			engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/upstreams", nil))
			// 404 而不是 401：401 等于承认这个端点存在，而这道开关要的正是
			// 「这台站点看起来不提供这个信息」。
			convey.So(w.Code, convey.ShouldEqual, http.StatusNotFound)
		})

		convey.Convey("带管理密钥仍然读得到", func() {
			upRepo, setRepo, public, _, engine := setupTest(t, nil)
			setRepo.EXPECT().Find(gomock.Any(), setting_svc.PublicHomepageSetting).Return(
				&setting_entity.Setting{Key: setting_svc.PublicHomepageSetting, Value: "false"}, nil)
			upRepo.EXPECT().List(gomock.Any()).AnyTimes().Return([]*upstream_entity.Upstream{
				{ID: 7, Host: "deb.debian.org", Kind: "static", Enabled: true},
			}, nil)
			public.rollup.EXPECT().SumByUpstream(gomock.Any(), gomock.Any(), gomock.Any()).
				Return([]*rollup_entity.Totals{{UpstreamID: 7, Requests: 4, Hits: 3}}, nil).AnyTimes()
			public.cache.EXPECT().SizeByUpstream(gomock.Any()).
				Return(map[int64]int64{7: 512}, nil).AnyTimes()

			req := httptest.NewRequest(http.MethodGet, "/api/v1/upstreams", nil)
			req.Header.Set("Authorization", "Bearer "+adminKey)
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)
			convey.So(w.Code, convey.ShouldEqual, http.StatusOK)
			convey.So(w.Body.String(), convey.ShouldContainSubstring, "deb.debian.org")
			// 新加的命中率与状态和老字段走同一道闸：关掉之后对匿名调用方一起
			// 消失，对带密钥的调用方一起还在。
			convey.So(w.Body.String(), convey.ShouldContainSubstring, `"hit_rate":0.75`)
			convey.So(w.Body.String(), convey.ShouldContainSubstring, `"status":"normal"`)
			convey.So(w.Body.String(), convey.ShouldContainSubstring, `"cache_bytes":512`)
		})
	})
}
