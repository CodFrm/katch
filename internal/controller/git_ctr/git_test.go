// 用例放在 _test 包里：它经 api.Router 注册真实路由，而 api 包又依赖本包，
// 同包会构成导入环。走 _test 包换来的是「测的就是生产路由 + 生产中间件」
// （同 cache_ctr_test 的理由）。
package git_ctr_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cago-frame/cago/pkg/utils/httputils"
	"github.com/cago-frame/cago/server/mux/muxclient"
	"github.com/cago-frame/cago/server/mux/muxtest"
	"github.com/gin-gonic/gin"
	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"
	"golang.org/x/crypto/bcrypt"

	"github.com/CodFrm/katch/internal/api"
	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/model/entity/git_entity"
	"github.com/CodFrm/katch/internal/model/entity/setting_entity"
	"github.com/CodFrm/katch/internal/repository/git_repo"
	mock_git_repo "github.com/CodFrm/katch/internal/repository/git_repo/mock"
	"github.com/CodFrm/katch/internal/repository/setting_repo"
	mock_setting_repo "github.com/CodFrm/katch/internal/repository/setting_repo/mock"
	"github.com/CodFrm/katch/internal/service/setting_svc"
)

const adminKey = "correct-admin-key"

// setupGitTest 装配一套「repo 是 mock、路由与中间件是生产那套」的测试环境。
//
// git 业务层用的是它自己的默认实例（没有配镜像目录的那个）：List/Delete 只碰
// git_mirrors 表，不碰盘（同 cache_ctr_test 的理由——盘上那一半由 git_svc 自己
// 的用例覆盖，见 internal/service/git_svc/sweep_test.go）。
func setupGitTest(t *testing.T) (*mock_git_repo.MockGitMirrorRepo, *muxtest.TestMux, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctrl := gomock.NewController(t)
	mirrorRepo := mock_git_repo.NewMockGitMirrorRepo(ctrl)
	git_repo.RegisterGitMirror(mirrorRepo)

	setRepo := mock_setting_repo.NewMockSettingRepo(ctrl)
	setting_repo.RegisterSetting(setRepo)
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
	return mirrorRepo, testMux, engine
}

func adminHeader(key string) muxclient.ClientDoOption {
	return muxclient.WithHeader(http.Header{"Authorization": []string{"Bearer " + key}})
}

func statusOf(t *testing.T, err error) int {
	t.Helper()
	e, ok := err.(*httputils.Error)
	if !ok {
		t.Fatalf("期望 *httputils.Error，拿到 %T（%v）", err, err)
	}
	return e.Status
}

// TestGitMirrorList 覆盖任务目标「管理界面出现 git 镜像列表页」的取数一侧。
func TestGitMirrorList(t *testing.T) {
	mirrorRepo, testMux, _ := setupGitTest(t)
	convey.Convey("列出全部镜像记录", t, func() {
		mirrorRepo.EXPECT().List(gomock.Any()).Return([]*git_entity.GitMirror{
			{
				ID: 7, Host: "github.com", Repo: "/foo/bar.git", State: git_entity.MirrorReady,
				SizeBytes: 1024, LastSyncAt: 1757000000, LastAccessAt: 1757000100,
				Createtime: 1756000000, Updatetime: 1757000000,
			},
		}, nil)

		resp := &admin.ListGitMirrorsResponse{}
		convey.So(testMux.Do(context.Background(), &admin.ListGitMirrorsRequest{}, resp,
			adminHeader(adminKey)), convey.ShouldBeNil)
		convey.So(len(resp.List), convey.ShouldEqual, 1)
		item := resp.List[0]
		convey.So(item.ID, convey.ShouldEqual, 7)
		convey.So(item.Host, convey.ShouldEqual, "github.com")
		convey.So(item.Repo, convey.ShouldEqual, "/foo/bar.git")
		convey.So(item.State, convey.ShouldEqual, git_entity.MirrorReady)
		convey.So(item.SizeBytes, convey.ShouldEqual, 1024)
		convey.So(item.LastSyncAt, convey.ShouldEqual, 1757000000)
		convey.So(item.LastAccessAt, convey.ShouldEqual, 1757000100)
	})
}

// TestGitMirrorDelete 覆盖任务目标「可删除单条」，以及删除之后 Lookup 落回
// 穿透——那一半在 git_svc 的用例里验，这里只验端点把 id 正确地交给了 service。
func TestGitMirrorDelete(t *testing.T) {
	mirrorRepo, testMux, _ := setupGitTest(t)
	convey.Convey("删除一条存在的镜像", t, func() {
		mirrorRepo.EXPECT().Find(gomock.Any(), int64(7)).Return(
			&git_entity.GitMirror{ID: 7, Host: "github.com", Repo: "/foo/bar.git",
				State: git_entity.MirrorReady}, nil)
		mirrorRepo.EXPECT().Delete(gomock.Any(), int64(7)).Return(nil)

		resp := &admin.DeleteGitMirrorResponse{}
		convey.So(testMux.Do(context.Background(), &admin.DeleteGitMirrorRequest{ID: 7}, resp,
			adminHeader(adminKey)), convey.ShouldBeNil)
	})
}

// TestGitMirrorDeleteNotFound 删一条已经不在的记录是 404，不是一次系统错误。
func TestGitMirrorDeleteNotFound(t *testing.T) {
	mirrorRepo, testMux, _ := setupGitTest(t)
	convey.Convey("删一条不存在的镜像", t, func() {
		mirrorRepo.EXPECT().Find(gomock.Any(), int64(404)).Return(nil, nil)

		err := testMux.Do(context.Background(), &admin.DeleteGitMirrorRequest{ID: 404},
			&admin.DeleteGitMirrorResponse{}, adminHeader(adminKey))
		convey.So(err, convey.ShouldNotBeNil)
		convey.So(statusOf(t, err), convey.ShouldEqual, http.StatusNotFound)
	})
}

// TestGitMirrorAdminAuth git 镜像管理是破坏性操作，两个端点都必须在密钥后面。
func TestGitMirrorAdminAuth(t *testing.T) {
	// mirrorRepo 上一个 EXPECT 都没有：鉴权一旦漏放行进 service，mock 会当场失败。
	_, testMux, engine := setupGitTest(t)
	convey.Convey("git 镜像管理端点一律要密钥", t, func() {
		for _, req := range []any{
			&admin.ListGitMirrorsRequest{},
			&admin.DeleteGitMirrorRequest{ID: 7},
		} {
			httpReq, err := testMux.Request(context.Background(), req)
			convey.So(err, convey.ShouldBeNil)
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, httpReq)
			convey.So(w.Code, convey.ShouldEqual, http.StatusUnauthorized)
		}
	})
}
