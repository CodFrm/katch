// 用例放在 _test 包里，理由同 upstream_ctr：它经 api.Router 注册生产路由，
// 而 api 包又依赖本包，同包会构成导入环。
package stat_ctr_test

import (
	"context"
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
	"github.com/CodFrm/katch/internal/api/stat"
	"github.com/CodFrm/katch/internal/model/entity/rollup_entity"
	"github.com/CodFrm/katch/internal/model/entity/setting_entity"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/backoff"
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

const testNow = int64(1700000000)

type statEnv struct {
	rollup   *mock_rollup_repo.MockTrafficRollupRepo
	upstream *mock_upstream_repo.MockUpstreamRepo
	mux      *muxtest.TestMux
	engine   *gin.Engine
}

func setupStatTest(t *testing.T, degraded stat_svc.DegradeReporter) *statEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctrl := gomock.NewController(t)
	env := &statEnv{
		rollup:   mock_rollup_repo.NewMockTrafficRollupRepo(ctrl),
		upstream: mock_upstream_repo.NewMockUpstreamRepo(ctrl),
	}
	rollup_repo.RegisterTrafficRollup(env.rollup)
	upstream_repo.RegisterUpstream(env.upstream)
	setRepo := mock_setting_repo.NewMockSettingRepo(ctrl)
	setting_repo.RegisterSetting(setRepo)
	hash, err := bcrypt.GenerateFromPassword([]byte(adminKey), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	setRepo.EXPECT().Find(gomock.Any(), setting_svc.AdminKeyHashSetting).Return(
		&setting_entity.Setting{Key: setting_svc.AdminKeyHashSetting, Value: string(hash)}, nil,
	).AnyTimes()
	stat_svc.Register(stat_svc.New(stat_svc.Options{
		Now:      func() time.Time { return time.Unix(testNow, 0) },
		Degraded: degraded,
	}))

	env.mux = muxtest.NewTestMux()
	if err := api.Router(context.Background(), env.mux.Router); err != nil {
		t.Fatal(err)
	}
	engine, ok := env.mux.IRouter.(*gin.Engine)
	if !ok {
		t.Fatal("muxtest 的 IRouter 不再是 *gin.Engine")
	}
	env.engine = engine
	return env
}

// TestStatOverviewIsPublic 首页要展示这个站省了多少流量，所以总览不要密钥。
func TestStatOverviewIsPublic(t *testing.T) {
	env := setupStatTest(t, nil)
	convey.Convey("不带密钥也能查到 24 小时总览", t, func() {
		env.rollup.EXPECT().Sum(gomock.Any(), testNow-24*3600, testNow).
			Return(&rollup_entity.Totals{
				Requests: 100, Hits: 60, Denied: 5, OriginErrors: 3,
				BytesServed: 4096, BytesOrigin: 1024,
			}, nil)

		resp := &stat.OverviewResponse{}
		convey.So(env.mux.Do(context.Background(), &stat.OverviewRequest{}, resp), convey.ShouldBeNil)
		convey.So(resp.Range, convey.ShouldEqual, "24h")
		convey.So(resp.From, convey.ShouldEqual, testNow-24*3600)
		convey.So(resp.To, convey.ShouldEqual, testNow)
		convey.So(resp.Requests, convey.ShouldEqual, 100)
		convey.So(resp.Hits, convey.ShouldEqual, 60)
		convey.So(resp.BytesServed, convey.ShouldEqual, 4096)
		convey.So(resp.BytesOrigin, convey.ShouldEqual, 1024)
	})
}

// TestStatByUpstreamRequiresKey 单上游的量是运营数据，和上游列表不一样，
// 不该由公开接口给出。
func TestStatByUpstreamRequiresKey(t *testing.T) {
	env := setupStatTest(t, nil)
	convey.Convey("按上游的统计要密钥", t, func() {
		// 一个 EXPECT 都没有：鉴权一旦放行进 service，mock 会当场让用例失败。
		req, err := env.mux.Request(context.Background(), &admin.UpstreamStatsRequest{})
		convey.So(err, convey.ShouldBeNil)
		w := httptest.NewRecorder()
		env.engine.ServeHTTP(w, req)
		convey.So(w.Code, convey.ShouldEqual, http.StatusUnauthorized)
	})
}

// TestStatByUpstreamDegraded 覆盖「连续失败的上游在界面上标为降级」：
// 降级来自内存里的退避状态，而不是 traffic_rollup。
func TestStatByUpstreamDegraded(t *testing.T) {
	clock := time.Unix(testNow, 0)
	tracker := backoff.New(backoff.Options{
		Threshold: 2, Base: 30 * time.Second, Now: func() time.Time { return clock },
	})
	env := setupStatTest(t, tracker)
	tracker.Failure("deb.debian.org")
	tracker.Failure("deb.debian.org")

	convey.Convey("带密钥能读到每个上游的量与降级标记", t, func() {
		env.upstream.EXPECT().List(gomock.Any()).Return([]*upstream_entity.Upstream{
			{ID: 7, Host: "deb.debian.org", Enabled: true},
			{ID: 8, Host: "proxy.golang.org", Enabled: true},
		}, nil)
		env.rollup.EXPECT().SumByUpstream(gomock.Any(), testNow-7*24*3600, testNow).
			Return([]*rollup_entity.Totals{{UpstreamID: 7, Requests: 100, Hits: 60}}, nil)

		resp := &admin.UpstreamStatsResponse{}
		convey.So(env.mux.Do(context.Background(), &admin.UpstreamStatsRequest{Range: "7d"}, resp,
			muxclient.WithHeader(http.Header{"Authorization": []string{"Bearer " + adminKey}})),
			convey.ShouldBeNil)
		convey.So(resp.Range, convey.ShouldEqual, "7d")
		convey.So(len(resp.List), convey.ShouldEqual, 2)
		convey.So(resp.List[0].Host, convey.ShouldEqual, "deb.debian.org")
		convey.So(resp.List[0].Requests, convey.ShouldEqual, 100)
		convey.So(resp.List[0].Degraded, convey.ShouldBeTrue)
		convey.So(resp.List[0].RetryAt, convey.ShouldEqual, testNow+30)
		convey.So(resp.List[1].Degraded, convey.ShouldBeFalse)
	})
}
