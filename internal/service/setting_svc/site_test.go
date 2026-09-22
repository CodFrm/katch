package setting_svc

import (
	"context"
	"testing"

	"github.com/cago-frame/cago/pkg/logger"
	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/CodFrm/katch/internal/model/entity/setting_entity"
	"github.com/CodFrm/katch/internal/repository/setting_repo"
	mock_setting_repo "github.com/CodFrm/katch/internal/repository/setting_repo/mock"
)

func TestBaseURLReadsOnlySiteDomain(t *testing.T) {
	convey.Convey("包管理器配置只读取 site_domain 并生成规范 base URL", t, func() {
		repo := mock_setting_repo.NewMockSettingRepo(gomock.NewController(t))
		setting_repo.RegisterSetting(repo)
		repo.EXPECT().Find(gomock.Any(), SiteDomainSetting).Return(&setting_entity.Setting{
			Key: SiteDomainSetting, Value: `"mirror.example.com/"`,
		}, nil)

		baseURL, err := Setting().BaseURL(context.Background())
		convey.So(err, convey.ShouldBeNil)
		convey.So(baseURL, convey.ShouldEqual, "https://mirror.example.com")
	})
}

// TestSiteBaseURLRefusesUnusableDomains
//
// 改写快照不经过设置服务，它自己从库里读原始的 site_domain（upstream_repo
// 的 RewriteConfig().Snapshot）。校验只挂在写入与设置服务的读上是不够的：
// 库里一行旧的坏值会一路拼进 metadata 改写出去的地址，而界面上这一项此刻
// 已经显示成「没配」，运维连是谁在作怪都看不到。
func TestSiteBaseURLRefusesUnusableDomains(t *testing.T) {
	convey.Convey("拼不出可用地址的值一律给空串", t, func() {
		for _, domain := range []string{
			"https://mirror.example?x=1",
			"https://user:pass@mirror.example",
			"javascript:alert(1)",
			"ftp://mirror.example",
			"https://mirror.example:99999",
		} {
			convey.So(SiteBaseURL(domain), convey.ShouldEqual, "")
		}

		convey.So(SiteBaseURL("mirror.example"), convey.ShouldEqual, "https://mirror.example")
		convey.So(SiteBaseURL("HTTP://mirror.example/katch/"),
			convey.ShouldEqual, "http://mirror.example/katch")
	})
}

// TestBaseURLDoesNotLogRejectedCredentials
//
// 被拒的 site_domain 恰恰是可能带着用户名密码的那一类值——校验拒绝它就是为了
// 不让凭据流到公开页面上去，读不懂时再把原值抄进日志，等于换了个地方泄漏
// （可观测性：不要记完整的凭据）。日志只说原因。
func TestBaseURLDoesNotLogRejectedCredentials(t *testing.T) {
	convey.Convey("库里带凭据的旧值退回空串，且不进日志", t, func() {
		core, logs := observer.New(zapcore.WarnLevel)
		logger.SetLogger(zap.New(core))
		t.Cleanup(func() { logger.SetLogger(zap.NewNop()) })

		repo := newMemorySettingRepo()
		setting_repo.RegisterSetting(repo)
		ctx := context.Background()
		convey.So(repo.Save(ctx, &setting_entity.Setting{
			Key: SiteDomainSetting, Value: `"https://operator:password@mirror.example"`,
		}), convey.ShouldBeNil)

		got, err := Setting().BaseURL(ctx)
		convey.So(err, convey.ShouldBeNil)
		convey.So(got, convey.ShouldEqual, "")

		entries := logs.All()
		convey.So(entries, convey.ShouldNotBeEmpty)
		for _, entry := range entries {
			line := entry.Message
			for _, field := range entry.Context {
				line += " " + field.String
			}
			convey.So(line, convey.ShouldNotContainSubstring, "password")
			convey.So(line, convey.ShouldNotContainSubstring, "operator")
		}
	})
}
