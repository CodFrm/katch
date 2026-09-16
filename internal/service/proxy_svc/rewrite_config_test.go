package proxy_svc

import (
	"context"
	"testing"

	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
	mock_upstream_repo "github.com/CodFrm/katch/internal/repository/upstream_repo/mock"
)

func TestRewriteConfigSourceSnapshot(t *testing.T) {
	convey.Convey("一次仓储快照生成站点地址、代数与启用主机配置", t, func() {
		repo := mock_upstream_repo.NewMockRewriteConfigRepo(gomock.NewController(t))
		upstream_repo.RegisterRewriteConfig(repo)
		repo.EXPECT().Snapshot(gomock.Any()).Return(&upstream_repo.RewriteConfigSnapshot{
			SiteDomain: " mirror.example.com/ ",
			Generation: 9,
			Upstreams: []*upstream_entity.Upstream{
				{
					Host: "registry.example.com", Enabled: true,
					Protocols:      upstream_entity.ProtocolSet{upstream_entity.ProtocolRegistry},
					PackageProfile: upstream_entity.PackageProfileNone,
				},
				{
					Host: "pypi.org", Enabled: true,
					Protocols:      upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
					PackageProfile: upstream_entity.PackageProfilePyPI,
				},
			},
		}, nil)

		got, err := NewRewriteConfigSource().Snapshot(context.Background())
		convey.So(err, convey.ShouldBeNil)
		convey.So(got.SiteBaseURL, convey.ShouldEqual, "https://mirror.example.com")
		convey.So(got.Generation, convey.ShouldEqual, int64(9))
		convey.So(got.Upstreams, convey.ShouldResemble, map[string]RewriteUpstream{
			"registry.example.com": {
				Profile:    upstream_entity.PackageProfileNone,
				Transports: upstream_entity.ProtocolSet{upstream_entity.ProtocolRegistry},
			},
			"pypi.org": {
				Profile:    upstream_entity.PackageProfilePyPI,
				Transports: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
			},
		})
	})

	convey.Convey("旧数据的空 profile 在快照边界归一化成 none", t, func() {
		repo := mock_upstream_repo.NewMockRewriteConfigRepo(gomock.NewController(t))
		upstream_repo.RegisterRewriteConfig(repo)
		repo.EXPECT().Snapshot(gomock.Any()).Return(&upstream_repo.RewriteConfigSnapshot{
			Upstreams: []*upstream_entity.Upstream{{
				Host: "legacy.example.com", Enabled: true,
				Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
			}},
		}, nil)

		got, err := NewRewriteConfigSource().Snapshot(context.Background())
		convey.So(err, convey.ShouldBeNil)
		convey.So(got.Upstreams["legacy.example.com"].Profile,
			convey.ShouldEqual, upstream_entity.PackageProfileNone)
	})
}
