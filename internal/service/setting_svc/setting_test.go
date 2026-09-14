package setting_svc

import (
	"context"
	"encoding/json"
	"errors"
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

// TestRecentRequestRetentionSetting 覆盖「最近请求保留时长」这一项：
// 默认一天，闭区间一小时到七天，写进去下一次读就是新的。
func TestRecentRequestRetentionSetting(t *testing.T) {
	convey.Convey("最近请求保留时长", t, func() {
		repo := newMemorySettingRepo()
		setting_repo.RegisterSetting(repo)
		ctx := context.Background()

		convey.Convey("库里没写过时默认一天", func() {
			rt, err := Setting().Runtime(ctx)
			convey.So(err, convey.ShouldBeNil)
			convey.So(rt.RecentRequestRetentionSeconds, convey.ShouldEqual, int64(86400))
		})

		convey.Convey("写进去之后读回来的是新的值", func() {
			// 界面上写的 `24h` 由前端解成秒，接口收到的是秒。
			_, err := Setting().Save(ctx, saveRequest(map[string]json.RawMessage{
				RecentRequestRetentionSecondsSetting: json.RawMessage(`604800`),
			}))
			convey.So(err, convey.ShouldBeNil)
			rt, err := Setting().Runtime(ctx)
			convey.So(err, convey.ShouldBeNil)
			convey.So(rt.RecentRequestRetentionSeconds, convey.ShouldEqual, int64(604800))
		})

		convey.Convey("闭区间外（半小时、七天零一秒、0、负数）拦在保存这一步", func() {
			for _, bad := range []string{`3599`, `604801`, `0`, `-1`} {
				_, err := Setting().Save(ctx, saveRequest(map[string]json.RawMessage{
					RecentRequestRetentionSecondsSetting: json.RawMessage(bad),
				}))
				_, c := statusAndCode(t, err)
				convey.So(c, convey.ShouldEqual, code.SettingValueInvalid)
				// 校验发生在写库之前：被拒的值不该留下一行。
				convey.So(repo.findCount(RecentRequestRetentionSecondsSetting), convey.ShouldEqual, 0)
			}
		})
	})
}

// TestPublicHomepage 覆盖「是否公开上游列表与命中率由设置控制」：
// 默认是公开的——一台镜像站默认就该答得出自己代理了什么。
func TestPublicHomepage(t *testing.T) {
	convey.Convey("首页是否公开", t, func() {
		repo := setupSettingTest(t)
		ctx := context.Background()

		convey.Convey("库里没有这个键时默认公开", func() {
			// 默认值只能是公开：默认私有会让一台刚装好的镜像站首页空着，
			// 而部署者根本不知道有个开关需要打开。
			repo.EXPECT().Find(gomock.Any(), PublicHomepageSetting).Return(nil, nil)
			public, err := Setting().PublicHomepage(ctx)
			convey.So(err, convey.ShouldBeNil)
			convey.So(public, convey.ShouldBeTrue)
		})

		convey.Convey("值为 false 时不公开", func() {
			repo.EXPECT().Find(gomock.Any(), PublicHomepageSetting).Return(
				&setting_entity.Setting{Key: PublicHomepageSetting, Value: "false"}, nil)
			public, err := Setting().PublicHomepage(ctx)
			convey.So(err, convey.ShouldBeNil)
			convey.So(public, convey.ShouldBeFalse)
		})

		convey.Convey("值为 true 时公开", func() {
			repo.EXPECT().Find(gomock.Any(), PublicHomepageSetting).Return(
				&setting_entity.Setting{Key: PublicHomepageSetting, Value: "true"}, nil)
			public, err := Setting().PublicHomepage(ctx)
			convey.So(err, convey.ShouldBeNil)
			convey.So(public, convey.ShouldBeTrue)
		})

		convey.Convey("值是读不懂的内容时按公开处理", func() {
			// 存坏了的一行不该悄悄把首页关掉：那看起来会像是功能丢了，
			// 而不是像一处配置错误。
			repo.EXPECT().Find(gomock.Any(), PublicHomepageSetting).Return(
				&setting_entity.Setting{Key: PublicHomepageSetting, Value: "yes"}, nil)
			public, err := Setting().PublicHomepage(ctx)
			convey.So(err, convey.ShouldBeNil)
			convey.So(public, convey.ShouldBeTrue)
		})

		convey.Convey("读库失败时把错误交出去，而不是替调用方猜", func() {
			repo.EXPECT().Find(gomock.Any(), PublicHomepageSetting).Return(nil, errors.New("库挂了"))
			_, err := Setting().PublicHomepage(ctx)
			convey.So(err, convey.ShouldNotBeNil)
		})
	})
}
