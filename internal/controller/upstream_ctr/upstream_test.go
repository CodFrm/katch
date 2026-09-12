// 用例放在 _test 包里：它经 api.Router 注册真实路由，而 api 包又依赖本包，
// 同包会构成导入环。走 _test 包换来的是「测的就是生产路由 + 生产中间件」。
package upstream_ctr_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cago-frame/cago/server/mux/muxclient"
	"github.com/cago-frame/cago/server/mux/muxtest"
	"github.com/gin-gonic/gin"
	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"
	"golang.org/x/crypto/bcrypt"

	"github.com/CodFrm/katch/internal/api"
	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/model/entity/setting_entity"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/repository/setting_repo"
	mock_setting_repo "github.com/CodFrm/katch/internal/repository/setting_repo/mock"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
	mock_upstream_repo "github.com/CodFrm/katch/internal/repository/upstream_repo/mock"
	"github.com/CodFrm/katch/internal/service/setting_svc"
)

const adminKey = "correct-admin-key"

// setupAdminTest 装配一套「repo 是 mock、路由与中间件是生产那套」的测试环境。
func setupAdminTest(t *testing.T) (*mock_upstream_repo.MockUpstreamRepo, *muxtest.TestMux, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctrl := gomock.NewController(t)
	upRepo := mock_upstream_repo.NewMockUpstreamRepo(ctrl)
	upstream_repo.RegisterUpstream(upRepo)
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
	return upRepo, testMux, engine
}

func adminHeader(key string) muxclient.ClientDoOption {
	return muxclient.WithHeader(http.Header{"Authorization": []string{"Bearer " + key}})
}

// TestUpstreamSaveAndList 覆盖任务目标 (a)：带正确密钥创建的上游能被 GET 读回，
// 且每个字段都原样穿过 controller → service → repository 的映射。
func TestUpstreamSaveAndList(t *testing.T) {
	upRepo, testMux, _ := setupAdminTest(t)
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
	_, testMux, engine := setupAdminTest(t)
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
	upRepo, testMux, _ := setupAdminTest(t)
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
	upRepo, testMux, _ := setupAdminTest(t)
	convey.Convey("删除上游", t, func() {
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
