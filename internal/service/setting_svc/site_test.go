package setting_svc

import (
	"context"
	"testing"

	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"

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
