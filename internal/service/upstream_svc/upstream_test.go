package upstream_svc

import (
	"context"
	"errors"
	"testing"

	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"

	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/repository/rule_repo"
	mock_rule_repo "github.com/CodFrm/katch/internal/repository/rule_repo/mock"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
	mock_upstream_repo "github.com/CodFrm/katch/internal/repository/upstream_repo/mock"
)

func setupUpstreamTest(t *testing.T) *mock_upstream_repo.MockUpstreamRepo {
	t.Helper()
	repo := mock_upstream_repo.NewMockUpstreamRepo(gomock.NewController(t))
	upstream_repo.RegisterUpstream(repo)
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
				ID: 7, Host: "docker.io", Kind: upstream_entity.KindRegistry,
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
				ID: 7, Host: "deb.debian.org", Kind: upstream_entity.KindStatic,
				Origin: "https://deb.debian.org",
			})
			convey.So(err, convey.ShouldNotBeNil)
		})

		convey.Convey("id 不存在时报错，而不是悄悄新建一条", func() {
			repo.EXPECT().Find(gomock.Any(), int64(404)).Return(nil, nil)
			_, err := Upstream().Update(ctx, &admin.UpdateUpstreamRequest{
				ID: 404, Host: "docker.io", Kind: upstream_entity.KindRegistry,
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
			Host: "docker.io", Kind: upstream_entity.KindRegistry,
			Origin: "https://registry-1.docker.io",
		})
		convey.So(err, convey.ShouldBeNil)
		convey.So(saved.DefaultPolicy, convey.ShouldEqual, upstream_entity.PolicyAllowAll)
	})
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
