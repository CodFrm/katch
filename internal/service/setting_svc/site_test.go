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
