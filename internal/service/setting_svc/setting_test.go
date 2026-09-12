package setting_svc

import (
	"context"
	"net/http"
	"testing"

	"github.com/cago-frame/cago/pkg/utils/httputils"
	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"
	"golang.org/x/crypto/bcrypt"

	"github.com/CodFrm/katch/internal/model/entity/setting_entity"
	"github.com/CodFrm/katch/internal/pkg/code"
	"github.com/CodFrm/katch/internal/repository/setting_repo"
	mock_setting_repo "github.com/CodFrm/katch/internal/repository/setting_repo/mock"
)

func setupSettingTest(t *testing.T) *mock_setting_repo.MockSettingRepo {
	t.Helper()
	repo := mock_setting_repo.NewMockSettingRepo(gomock.NewController(t))
	setting_repo.RegisterSetting(repo)
	return repo
}

// statusAndCode 把 service 返回的错误摊成 (HTTP 状态码, 业务码)。
func statusAndCode(t *testing.T, err error) (int, int) {
	t.Helper()
	e, ok := err.(*httputils.Error)
	if !ok {
		t.Fatalf("期望 *httputils.Error，拿到 %T", err)
	}
	return e.Status, e.Code
}

// TestEnsureAdminKey 覆盖决策 18：config.yaml 里的是**初始**密钥，
// 只在库中尚无密钥时生效。
func TestEnsureAdminKey(t *testing.T) {
	convey.Convey("初始密钥的落库时机", t, func() {
		repo := setupSettingTest(t)
		ctx := context.Background()

		convey.Convey("库里没有密钥时，用初始密钥落一份哈希", func() {
			var saved *setting_entity.Setting
			repo.EXPECT().Find(gomock.Any(), AdminKeyHashSetting).Return(nil, nil)
			repo.EXPECT().Save(gomock.Any(), gomock.Any()).DoAndReturn(
				func(_ context.Context, s *setting_entity.Setting) error {
					saved = s
					return nil
				})

			convey.So(Setting().EnsureAdminKey(ctx, "initial-key"), convey.ShouldBeNil)
			convey.So(saved.Key, convey.ShouldEqual, AdminKeyHashSetting)
			// 落的是哈希不是明文：这张表在后台是可读的。
			convey.So(saved.Value, convey.ShouldNotEqual, "initial-key")
			convey.So(bcrypt.CompareHashAndPassword([]byte(saved.Value), []byte("initial-key")),
				convey.ShouldBeNil)
		})

		convey.Convey("库里已有密钥时，改配置文件不会把它覆盖回去", func() {
			// 轮换发生在界面上。初始密钥若每次启动都覆盖，轮换就等于没发生，
			// 而且旧密钥会在下一次重启后复活。
			repo.EXPECT().Find(gomock.Any(), AdminKeyHashSetting).Return(
				&setting_entity.Setting{Key: AdminKeyHashSetting, Value: "已经轮换过的哈希"}, nil)
			// 一个 Save 的 EXPECT 都没有：真去写库会当场让用例失败。

			convey.So(Setting().EnsureAdminKey(ctx, "initial-key"), convey.ShouldBeNil)
		})

		convey.Convey("没有配置初始密钥时不算启动失败", func() {
			// 纯拉取用途的部署可以完全不开管理接口；把这当成致命错误会让它起不来。
			repo.EXPECT().Find(gomock.Any(), AdminKeyHashSetting).Return(nil, nil)

			convey.So(Setting().EnsureAdminKey(ctx, ""), convey.ShouldBeNil)
		})
	})
}

func TestVerifyAdminKey(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("right"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	convey.Convey("校验管理密钥", t, func() {
		repo := setupSettingTest(t)
		ctx := context.Background()
		stored := &setting_entity.Setting{Key: AdminKeyHashSetting, Value: string(hash)}

		convey.Convey("密钥正确时放行", func() {
			repo.EXPECT().Find(gomock.Any(), AdminKeyHashSetting).Return(stored, nil)
			convey.So(Setting().VerifyAdminKey(ctx, "right"), convey.ShouldBeNil)
		})

		convey.Convey("密钥错误与未提供密钥拿到的是同一个错误", func() {
			repo.EXPECT().Find(gomock.Any(), AdminKeyHashSetting).Return(stored, nil).Times(2)
			wrongStatus, wrongCode := statusAndCode(t, Setting().VerifyAdminKey(ctx, "wrong"))
			emptyStatus, emptyCode := statusAndCode(t, Setting().VerifyAdminKey(ctx, ""))
			convey.So(wrongStatus, convey.ShouldEqual, http.StatusUnauthorized)
			convey.So(wrongCode, convey.ShouldEqual, code.AdminKeyInvalid)
			convey.So(emptyStatus, convey.ShouldEqual, wrongStatus)
			convey.So(emptyCode, convey.ShouldEqual, wrongCode)
		})

		convey.Convey("库里没有密钥时全量拒绝，而不是敞开", func() {
			repo.EXPECT().Find(gomock.Any(), AdminKeyHashSetting).Return(nil, nil)
			status, c := statusAndCode(t, Setting().VerifyAdminKey(ctx, "anything"))
			convey.So(status, convey.ShouldEqual, http.StatusUnauthorized)
			convey.So(c, convey.ShouldEqual, code.AdminKeyNotInitialized)
		})
	})
}
