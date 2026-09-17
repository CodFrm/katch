package upstream_svc

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"

	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	_ "github.com/CodFrm/katch/internal/proxy/packageprofile/builtin"
	"github.com/CodFrm/katch/internal/repository/rule_repo"
	mock_rule_repo "github.com/CodFrm/katch/internal/repository/rule_repo/mock"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
	mock_upstream_repo "github.com/CodFrm/katch/internal/repository/upstream_repo/mock"
)

func TestPackageProfileReadiness(t *testing.T) {
	convey.Convey("readiness 由 site_domain 和 companion 的启用、transport、profile 共同决定", t, func() {
		primary := &upstream_entity.Upstream{
			Host: "pypi.org", Enabled: true,
			Protocols:      upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
			PackageProfile: upstream_entity.PackageProfilePyPI,
		}
		files := &upstream_entity.Upstream{
			Host: "files.pythonhosted.org", Enabled: true,
			Protocols:      upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
			PackageProfile: upstream_entity.PackageProfilePyPI,
		}

		ready := packageReadiness("https://mirror.example.com", []*upstream_entity.Upstream{primary, files}, primary)
		convey.So(ready.Ready, convey.ShouldBeTrue)
		convey.So(ready.Missing, convey.ShouldBeEmpty)
		convey.So(ready.Companions, convey.ShouldResemble, []admin.PackageCompanion{
			{Host: "files.pythonhosted.org", Transport: upstream_entity.ProtocolStatic,
				PackageProfile: upstream_entity.PackageProfilePyPI, Ready: true},
		})
		convey.So(ready.Guidance.Clients, convey.ShouldResemble, []string{"pip", "uv", "poetry"})
		convey.So(ready.Guidance.Configuration, convey.ShouldResemble, []string{
			"pip install --index-url https://mirror.example.com/pypi.org/simple/ <package>",
			"uv pip install --index-url https://mirror.example.com/pypi.org/simple/ <package>",
			"poetry source add --priority=primary katch https://mirror.example.com/pypi.org/simple/",
		})
		convey.So(ready.Guidance.Constraints, convey.ShouldContain, "trailing_slash")
		convey.So(ready.Guidance.RuntimeVerified, convey.ShouldBeFalse)

		files.Enabled = false
		blocked := packageReadiness("", []*upstream_entity.Upstream{primary, files}, primary)
		convey.So(blocked.Ready, convey.ShouldBeFalse)
		convey.So(blocked.Missing, convey.ShouldResemble, []string{"site_domain"})
		convey.So(blocked.Companions[0].Ready, convey.ShouldBeFalse)
		convey.So(blocked.Companions[0].Reason, convey.ShouldEqual, "disabled")
	})

	convey.Convey("新建草稿可以满足与 primary 同 host 的 companion", t, func() {
		candidate := &upstream_entity.Upstream{
			Host: "registry.npmjs.org", Enabled: true,
			Protocols:      upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
			PackageProfile: upstream_entity.PackageProfileNPM,
		}
		readiness := packageReadiness("https://mirror.example.com", nil, candidate)
		convey.So(readiness.Ready, convey.ShouldBeTrue)
		convey.So(readiness.Companions, convey.ShouldHaveLength, 1)
		convey.So(readiness.Companions[0].Ready, convey.ShouldBeTrue)
	})

	convey.Convey("NuGet readiness requires the complete fixed official host set", t, func() {
		hosts := []string{
			"api.nuget.org",
			"nuget.azure.cn",
			"azuresearch-usnc.nuget.org",
			"azuresearch-ea.nuget.org",
			"azuresearch-sea.nuget.org",
			"globalcdn.nuget.org",
			"www.nuget.org",
		}
		configured := make([]*upstream_entity.Upstream, 0, len(hosts))
		for _, host := range hosts {
			configured = append(configured, &upstream_entity.Upstream{
				Host: host, Enabled: true,
				Protocols:      upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
				PackageProfile: upstream_entity.PackageProfileNuGet,
			})
		}

		readiness := packageReadiness("https://mirror.example.com", configured, configured[0])
		convey.So(readiness.Ready, convey.ShouldBeTrue)
		convey.So(readiness.Companions, convey.ShouldHaveLength, len(hosts))
		for i, host := range hosts {
			convey.So(readiness.Companions[i], convey.ShouldResemble, admin.PackageCompanion{
				Host: host, Transport: upstream_entity.ProtocolStatic,
				PackageProfile: upstream_entity.PackageProfileNuGet, Ready: true,
			})
		}
	})

	for _, tc := range []struct {
		name   string
		files  *upstream_entity.Upstream
		reason string
	}{
		{name: "companion 不存在", reason: "missing"},
		{name: "companion 已停用", files: &upstream_entity.Upstream{
			Host: "files.pythonhosted.org", Enabled: false,
			Protocols:      upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
			PackageProfile: upstream_entity.PackageProfilePyPI,
		}, reason: "disabled"},
		{name: "companion transport 不兼容", files: &upstream_entity.Upstream{
			Host: "files.pythonhosted.org", Enabled: true,
			Protocols:      upstream_entity.ProtocolSet{upstream_entity.ProtocolRegistry},
			PackageProfile: upstream_entity.PackageProfilePyPI,
		}, reason: "transport"},
		{name: "companion profile 不兼容", files: &upstream_entity.Upstream{
			Host: "files.pythonhosted.org", Enabled: true,
			Protocols:      upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
			PackageProfile: upstream_entity.PackageProfileNone,
		}, reason: "profile"},
	} {
		convey.Convey(tc.name, t, func() {
			primary := &upstream_entity.Upstream{
				Host: "pypi.org", Enabled: true,
				Protocols:      upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
				PackageProfile: upstream_entity.PackageProfilePyPI,
			}
			configured := []*upstream_entity.Upstream{primary}
			if tc.files != nil {
				configured = append(configured, tc.files)
			}
			readiness := packageReadiness("https://mirror.example.com", configured, primary)
			convey.So(readiness.Ready, convey.ShouldBeFalse)
			convey.So(readiness.Companions, convey.ShouldHaveLength, 1)
			convey.So(readiness.Companions[0].Reason, convey.ShouldEqual, tc.reason)
		})
	}
}

func TestPackageProfilesComeFromRegisteredBackendAdapters(t *testing.T) {
	profiles := Upstream().PackageProfiles()
	convey.Convey("none 和所有实际注册的 adapter 都由后端返回", t, func() {
		convey.So(profiles, convey.ShouldHaveLength, 13)
		convey.So(profiles[0].Profile, convey.ShouldEqual, upstream_entity.PackageProfileNone)
		seen := make(map[upstream_entity.PackageProfile]bool, len(profiles))
		for _, profile := range profiles {
			seen[profile.Profile] = true
		}
		for _, profile := range []upstream_entity.PackageProfile{
			upstream_entity.PackageProfileNPM, upstream_entity.PackageProfilePyPI,
			upstream_entity.PackageProfileGoProxy, upstream_entity.PackageProfileMaven,
			upstream_entity.PackageProfileCargo, upstream_entity.PackageProfileNuGet,
			upstream_entity.PackageProfileRubyGems, upstream_entity.PackageProfileAPT,
			upstream_entity.PackageProfileRPM, upstream_entity.PackageProfileAPK,
			upstream_entity.PackageProfileComposer, upstream_entity.PackageProfileHomebrew,
		} {
			convey.So(seen[profile], convey.ShouldBeTrue)
		}
	})
}

func TestPackageGuidanceCoversRuntimeMatrixWithoutUnsupportedClaims(t *testing.T) {
	wantClients := map[upstream_entity.PackageProfile][]string{
		upstream_entity.PackageProfileNPM:      {"npm", "pnpm", "yarn_classic", "yarn_berry", "bun"},
		upstream_entity.PackageProfilePyPI:     {"pip", "uv", "poetry"},
		upstream_entity.PackageProfileGoProxy:  {"go"},
		upstream_entity.PackageProfileMaven:    {"maven", "gradle", "sbt"},
		upstream_entity.PackageProfileCargo:    {"cargo"},
		upstream_entity.PackageProfileNuGet:    {"dotnet", "nuget"},
		upstream_entity.PackageProfileRubyGems: {"gem", "bundler"},
		upstream_entity.PackageProfileAPT:      {"apt"},
		upstream_entity.PackageProfileRPM:      {"dnf", "yum"},
		upstream_entity.PackageProfileAPK:      {"apk"},
		upstream_entity.PackageProfileComposer: {"composer"},
		upstream_entity.PackageProfileHomebrew: {"homebrew"},
	}
	for profile, clients := range wantClients {
		convey.Convey(string(profile), t, func() {
			candidate := &upstream_entity.Upstream{
				Host: profileHost(profile), Enabled: true,
				Protocols:      upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
				PackageProfile: profile,
			}
			if profile == upstream_entity.PackageProfileHomebrew {
				candidate.Host = "formulae.brew.sh"
			}
			readiness := packageReadiness("https://mirror.example.com", []*upstream_entity.Upstream{candidate}, candidate)
			convey.So(reflect.DeepEqual(readiness.Guidance.Clients, clients), convey.ShouldBeTrue)
			joined := ""
			for _, line := range readiness.Guidance.Configuration {
				joined += line + "\n"
			}
			convey.So(joined, convey.ShouldNotContainSubstring, "publish")
			convey.So(joined, convey.ShouldNotContainSubstring, "audit")
			convey.So(joined, convey.ShouldNotContainSubstring, "prefer-source")
			convey.So(joined, convey.ShouldNotContainSubstring, "mirrorlist=http")
		})
	}

	convey.Convey("Homebrew 强制 no-fallback，RPM 使用固定 base 且关闭动态镜像", t, func() {
		homebrew := packageGuidance(upstream_entity.PackageProfileHomebrew, "formulae.brew.sh", "https://mirror.example.com")
		convey.So(homebrew.Configuration, convey.ShouldContain,
			"export HOMEBREW_ARTIFACT_DOMAIN_NO_FALLBACK=1")
		convey.So(homebrew.Constraints, convey.ShouldContain, "no_fallback")

		rpm := packageGuidance(upstream_entity.PackageProfileRPM, "mirror.stream.centos.org", "https://mirror.example.com")
		convey.So(rpm.Configuration, convey.ShouldContain,
			"baseurl=https://mirror.example.com/mirror.stream.centos.org/<repository-path>/")
		convey.So(rpm.Configuration, convey.ShouldContain, "mirrorlist=")
		convey.So(rpm.Configuration, convey.ShouldContain, "metalink=")
		convey.So(rpm.Constraints, convey.ShouldContain, "fixed_base")
	})
}

func profileHost(profile upstream_entity.PackageProfile) string {
	return map[upstream_entity.PackageProfile]string{
		upstream_entity.PackageProfileNPM: "registry.npmjs.org", upstream_entity.PackageProfilePyPI: "pypi.org",
		upstream_entity.PackageProfileGoProxy: "proxy.golang.org", upstream_entity.PackageProfileMaven: "repo.maven.apache.org",
		upstream_entity.PackageProfileCargo: "index.crates.io", upstream_entity.PackageProfileNuGet: "api.nuget.org",
		upstream_entity.PackageProfileRubyGems: "rubygems.org", upstream_entity.PackageProfileAPT: "deb.debian.org",
		upstream_entity.PackageProfileRPM: "mirror.stream.centos.org", upstream_entity.PackageProfileAPK: "dl-cdn.alpinelinux.org",
		upstream_entity.PackageProfileComposer: "repo.packagist.org",
	}[profile]
}

func setupUpstreamTest(t *testing.T) *mock_upstream_repo.MockUpstreamRepo {
	t.Helper()
	ctrl := gomock.NewController(t)
	repo := mock_upstream_repo.NewMockUpstreamRepo(ctrl)
	upstream_repo.RegisterUpstream(repo)
	rewrite := mock_upstream_repo.NewMockRewriteConfigRepo(ctrl)
	rewrite.EXPECT().Transaction(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }).AnyTimes()
	rewrite.EXPECT().AdvanceGeneration(gomock.Any()).Return(nil).AnyTimes()
	upstream_repo.RegisterRewriteConfig(rewrite)
	return repo
}

// TestFindByHost 覆盖决策 6：上游表就是白名单，「已停用」对外必须和
// 「表里根本没有这条」是同一件事。分发层因此不必自己再看一遍 Enabled——
// 让每个调用方各判一次，迟早会有一个忘了判，那就是一个开放代理。
func TestFindByHost(t *testing.T) {
	convey.Convey("按 host 找可用的上游", t, func() {
		repo := setupUpstreamTest(t)
		ctx := context.Background()

		convey.Convey("启用中的上游能找到", func() {
			repo.EXPECT().FindByHost(gomock.Any(), "docker.io").Return(
				&upstream_entity.Upstream{ID: 1, Host: "docker.io", Enabled: true}, nil)
			got, err := Upstream().FindByHost(ctx, "docker.io")
			convey.So(err, convey.ShouldBeNil)
			convey.So(got.ID, convey.ShouldEqual, 1)
		})

		convey.Convey("已停用的上游等同于不存在", func() {
			repo.EXPECT().FindByHost(gomock.Any(), "docker.io").Return(
				&upstream_entity.Upstream{ID: 1, Host: "docker.io", Enabled: false}, nil)
			got, err := Upstream().FindByHost(ctx, "docker.io")
			convey.So(err, convey.ShouldBeNil)
			convey.So(got, convey.ShouldBeNil)
		})

		convey.Convey("不在表里的 host 返回 (nil, nil) 而不是错误", func() {
			repo.EXPECT().FindByHost(gomock.Any(), "evil.example.com").Return(nil, nil)
			got, err := Upstream().FindByHost(ctx, "evil.example.com")
			convey.So(err, convey.ShouldBeNil)
			convey.So(got, convey.ShouldBeNil)
		})
	})
}

// TestUpdate 改一条已存在的上游：整条替换，且这一条上游停用之后必须真的停着。
func TestUpdate(t *testing.T) {
	convey.Convey("更新已有上游", t, func() {
		repo := setupUpstreamTest(t)
		ctx := context.Background()

		convey.Convey("host 没变时不该被自己的查重挡住，且保留原 createtime", func() {
			exist := &upstream_entity.Upstream{
				ID: 7, Host: "docker.io", Enabled: true, Createtime: 111, Updatetime: 111,
			}
			repo.EXPECT().Find(gomock.Any(), int64(7)).Return(exist, nil)
			repo.EXPECT().FindByHost(gomock.Any(), "docker.io").Return(exist, nil)
			var saved *upstream_entity.Upstream
			repo.EXPECT().Save(gomock.Any(), gomock.Any()).DoAndReturn(
				func(_ context.Context, up *upstream_entity.Upstream) error {
					saved = up
					return nil
				})

			resp, err := Upstream().Update(ctx, &admin.UpdateUpstreamRequest{
				ID: 7, Host: "docker.io", Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolRegistry},
				Origin: "https://registry-1.docker.io", Enabled: false,
			})
			convey.So(err, convey.ShouldBeNil)
			convey.So(resp.ID, convey.ShouldEqual, 7)
			// createtime 来自库里那条，不是被这次请求重置的。
			convey.So(saved.Createtime, convey.ShouldEqual, 111)
			convey.So(saved.Updatetime, convey.ShouldBeGreaterThan, 111)
			// 停用必须真的落下去，这是白名单上最要紧的一次写。
			convey.So(saved.Enabled, convey.ShouldBeFalse)
		})

		convey.Convey("改成别人已占用的 host 时拒绝", func() {
			repo.EXPECT().Find(gomock.Any(), int64(7)).Return(
				&upstream_entity.Upstream{ID: 7, Host: "docker.io"}, nil)
			repo.EXPECT().FindByHost(gomock.Any(), "deb.debian.org").Return(
				&upstream_entity.Upstream{ID: 9, Host: "deb.debian.org"}, nil)

			_, err := Upstream().Update(ctx, &admin.UpdateUpstreamRequest{
				ID: 7, Host: "deb.debian.org", Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
				Origin: "https://deb.debian.org",
			})
			convey.So(err, convey.ShouldNotBeNil)
		})

		convey.Convey("id 不存在时报错，而不是悄悄新建一条", func() {
			repo.EXPECT().Find(gomock.Any(), int64(404)).Return(nil, nil)
			_, err := Upstream().Update(ctx, &admin.UpdateUpstreamRequest{
				ID: 404, Host: "docker.io", Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolRegistry},
				Origin: "https://registry-1.docker.io",
			})
			convey.So(err, convey.ShouldNotBeNil)
		})
	})
}

// TestSaveDefaultPolicy 留空的默认策略要落成 allow_all，而不是空串——
// 空串会让后续的规则求值拿到一个既不是放行也不是拒绝的第三种状态。
func TestSaveDefaultPolicy(t *testing.T) {
	convey.Convey("默认策略留空时落成 allow_all", t, func() {
		repo := setupUpstreamTest(t)
		repo.EXPECT().FindByHost(gomock.Any(), "docker.io").Return(nil, nil)
		var saved *upstream_entity.Upstream
		repo.EXPECT().Save(gomock.Any(), gomock.Any()).DoAndReturn(
			func(_ context.Context, up *upstream_entity.Upstream) error {
				saved = up
				return nil
			})

		_, err := Upstream().Save(context.Background(), &admin.SaveUpstreamRequest{
			Host: "docker.io", Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolRegistry},
			Origin: "https://registry-1.docker.io",
		})
		convey.So(err, convey.ShouldBeNil)
		convey.So(saved.DefaultPolicy, convey.ShouldEqual, upstream_entity.PolicyAllowAll)
	})
}

func TestPackageProfilePersistenceAndValidation(t *testing.T) {
	profiles := []upstream_entity.PackageProfile{
		upstream_entity.PackageProfileNone,
		upstream_entity.PackageProfileNPM,
		upstream_entity.PackageProfilePyPI,
		upstream_entity.PackageProfileGoProxy,
		upstream_entity.PackageProfileMaven,
		upstream_entity.PackageProfileCargo,
		upstream_entity.PackageProfileNuGet,
		upstream_entity.PackageProfileRubyGems,
		upstream_entity.PackageProfileAPT,
		upstream_entity.PackageProfileRPM,
		upstream_entity.PackageProfileAPK,
		upstream_entity.PackageProfileComposer,
		upstream_entity.PackageProfileHomebrew,
	}
	convey.Convey("每个批准的 package profile 都能保存", t, func() {
		repo := setupUpstreamTest(t)
		for _, profile := range profiles {
			host := string(profile) + ".example.com"
			repo.EXPECT().FindByHost(gomock.Any(), host).Return(nil, nil)
			repo.EXPECT().Save(gomock.Any(), gomock.Any()).DoAndReturn(
				func(_ context.Context, got *upstream_entity.Upstream) error {
					convey.So(got.PackageProfile, convey.ShouldEqual, profile)
					return nil
				})
			_, err := Upstream().Save(context.Background(), &admin.SaveUpstreamRequest{
				Host: host, Protocols: []string{upstream_entity.ProtocolStatic},
				Origin: "https://" + host, Enabled: true, PackageProfile: profile,
			})
			convey.So(err, convey.ShouldBeNil)
		}
	})

	convey.Convey("省略 profile 时持久化为 none", t, func() {
		repo := setupUpstreamTest(t)
		repo.EXPECT().FindByHost(gomock.Any(), "legacy.example.com").Return(nil, nil)
		repo.EXPECT().Save(gomock.Any(), gomock.Any()).DoAndReturn(
			func(_ context.Context, got *upstream_entity.Upstream) error {
				convey.So(got.PackageProfile, convey.ShouldEqual, upstream_entity.PackageProfileNone)
				return nil
			})
		_, err := Upstream().Save(context.Background(), &admin.SaveUpstreamRequest{
			Host: "legacy.example.com", Protocols: []string{upstream_entity.ProtocolStatic},
			Origin: "https://legacy.example.com",
		})
		convey.So(err, convey.ShouldBeNil)
	})

	convey.Convey("非 none profile 没有 static transport 时拒绝且不写库", t, func() {
		setupUpstreamTest(t)
		_, err := Upstream().Save(context.Background(), &admin.SaveUpstreamRequest{
			Host: "registry.example.com", Protocols: []string{upstream_entity.ProtocolRegistry},
			Origin: "https://registry.example.com", PackageProfile: upstream_entity.PackageProfileNPM,
		})
		convey.So(err, convey.ShouldNotBeNil)
	})
}

func TestRewriteGenerationChangesOnlyForEffectiveUpstreamConfig(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*admin.UpdateUpstreamRequest)
		bump   bool
	}{
		{name: "完全相同", mutate: func(*admin.UpdateUpstreamRequest) {}, bump: false},
		{name: "只改 origin", mutate: func(r *admin.UpdateUpstreamRequest) { r.Origin = "https://cdn.example.com" }, bump: false},
		{name: "改 profile", mutate: func(r *admin.UpdateUpstreamRequest) { r.PackageProfile = upstream_entity.PackageProfileNPM }, bump: true},
		{name: "改 transport", mutate: func(r *admin.UpdateUpstreamRequest) {
			r.Protocols = []string{upstream_entity.ProtocolStatic, upstream_entity.ProtocolGit}
		}, bump: true},
		{name: "改 enabled", mutate: func(r *admin.UpdateUpstreamRequest) { r.Enabled = false }, bump: true},
		{name: "改 companion host", mutate: func(r *admin.UpdateUpstreamRequest) { r.Host = "files.example.com" }, bump: true},
	}
	for _, tc := range cases {
		convey.Convey(tc.name, t, func() {
			ctrl := gomock.NewController(t)
			repo := mock_upstream_repo.NewMockUpstreamRepo(ctrl)
			rewrite := mock_upstream_repo.NewMockRewriteConfigRepo(ctrl)
			upstream_repo.RegisterUpstream(repo)
			upstream_repo.RegisterRewriteConfig(rewrite)
			rewrite.EXPECT().Transaction(gomock.Any(), gomock.Any()).DoAndReturn(
				func(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) })
			if tc.bump {
				rewrite.EXPECT().AdvanceGeneration(gomock.Any()).Return(nil)
			}
			existing := &upstream_entity.Upstream{
				ID: 7, Host: "index.example.com", Origin: "https://index.example.com", Enabled: true,
				Protocols:      upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
				PackageProfile: upstream_entity.PackageProfileNone, Createtime: 11,
			}
			req := &admin.UpdateUpstreamRequest{
				ID: 7, Host: existing.Host, Origin: existing.Origin, Enabled: existing.Enabled,
				Protocols: []string{upstream_entity.ProtocolStatic}, PackageProfile: existing.PackageProfile,
			}
			tc.mutate(req)
			repo.EXPECT().Find(gomock.Any(), int64(7)).Return(existing, nil)
			repo.EXPECT().FindByHost(gomock.Any(), req.Host).Return(existing, nil)
			repo.EXPECT().Save(gomock.Any(), gomock.Any()).Return(nil)

			_, err := Upstream().Update(context.Background(), req)
			convey.So(err, convey.ShouldBeNil)
		})
	}
}

// TestDeleteCascadesRules 删掉一个上游，它名下的规则必须跟着走。
//
// 留着的孤儿规则不会马上出事——求值只看全局那层和当前上游那层，id 不在表里时
// 谁都匹配不到它们。真正的问题是自增 id 会被复用：等到下一个上游拿到这个 id，
// 那批本该消失的规则会毫无征兆地在一个完全不相干的上游上生效。写入侧已经有
// 对称的一半（rule_svc.Save 拒绝挂到不存在的上游上），删除侧不能缺。
func TestDeleteCascadesRules(t *testing.T) {
	convey.Convey("删除上游时连带删掉它的规则", t, func() {
		upRepo := setupUpstreamTest(t)
		ruleRepo := mock_rule_repo.NewMockAccessRuleRepo(gomock.NewController(t))
		rule_repo.RegisterAccessRule(ruleRepo)
		ctx := context.Background()

		convey.Convey("规则按这个上游的 id 删，上游本身照删", func() {
			upRepo.EXPECT().Find(gomock.Any(), int64(7)).Return(
				&upstream_entity.Upstream{ID: 7, Host: "deb.debian.org"}, nil)
			ruleRepo.EXPECT().DeleteByUpstream(gomock.Any(), int64(7)).Return(nil)
			upRepo.EXPECT().Delete(gomock.Any(), int64(7)).Return(nil)

			_, err := Upstream().Delete(ctx, &admin.DeleteUpstreamRequest{ID: 7})
			convey.So(err, convey.ShouldBeNil)
		})

		convey.Convey("规则删不掉时不删上游，免得留下一批孤儿", func() {
			// 顺序是刻意的：先删规则再删上游。反过来的话，规则这一步失败就正好
			// 制造出这个缺陷本身，而且重试时上游已经不在了，再也补不回来。
			upRepo.EXPECT().Find(gomock.Any(), int64(7)).Return(
				&upstream_entity.Upstream{ID: 7, Host: "deb.debian.org"}, nil)
			ruleRepo.EXPECT().DeleteByUpstream(gomock.Any(), int64(7)).Return(errors.New("db down"))

			_, err := Upstream().Delete(ctx, &admin.DeleteUpstreamRequest{ID: 7})
			convey.So(err, convey.ShouldNotBeNil)
		})

		convey.Convey("上游不存在时一条规则都不碰", func() {
			upRepo.EXPECT().Find(gomock.Any(), int64(404)).Return(nil, nil)

			_, err := Upstream().Delete(ctx, &admin.DeleteUpstreamRequest{ID: 404})
			convey.So(err, convey.ShouldNotBeNil)
		})
	})
}
