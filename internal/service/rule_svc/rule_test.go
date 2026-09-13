package rule_svc

import (
	"context"
	"errors"
	"testing"

	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"

	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/model/entity/rule_entity"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/repository/rule_repo"
	mock_rule_repo "github.com/CodFrm/katch/internal/repository/rule_repo/mock"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
	mock_upstream_repo "github.com/CodFrm/katch/internal/repository/upstream_repo/mock"
)

// setupRuleTest 装一套「仓储是 mock、业务是生产那套」的环境，并把业务层换成
// 一份干净的实例：规则在进程内有快照，沿用上一个用例那份会串味。
func setupRuleTest(t *testing.T) (*mock_rule_repo.MockAccessRuleRepo, *mock_upstream_repo.MockUpstreamRepo) {
	t.Helper()
	ctrl := gomock.NewController(t)
	ruleRepo := mock_rule_repo.NewMockAccessRuleRepo(ctrl)
	rule_repo.RegisterAccessRule(ruleRepo)
	upRepo := mock_upstream_repo.NewMockUpstreamRepo(ctrl)
	upstream_repo.RegisterUpstream(upRepo)
	prev := Rule()
	Register(New())
	t.Cleanup(func() { Register(prev) })
	return ruleRepo, upRepo
}

func debian() *upstream_entity.Upstream {
	return &upstream_entity.Upstream{
		ID: 7, Host: "deb.debian.org", Kind: upstream_entity.KindStatic,
		Enabled: true, DefaultPolicy: upstream_entity.PolicyAllowAll,
	}
}

// TestEvaluate_GlobalDenyBeatsUpstreamAllow 任务目标 (a) 在业务层的那一半：
// 两条规则一条全局一条上游内，具体度完全相同，结果必须由层级决定。
func TestEvaluate_GlobalDenyBeatsUpstreamAllow(t *testing.T) {
	convey.Convey("全局 deny 压过同样具体的上游 allow", t, func() {
		ruleRepo, _ := setupRuleTest(t)
		ruleRepo.EXPECT().List(gomock.Any()).Return([]*rule_entity.AccessRule{
			{ID: 1, UpstreamID: 0, Action: rule_entity.ActionDeny, Pattern: "dists/*"},
			{ID: 2, UpstreamID: 7, Action: rule_entity.ActionAllow, Pattern: "dists/*"},
			// 别的上游的规则不该掺进来。
			{ID: 3, UpstreamID: 8, Action: rule_entity.ActionDeny, Pattern: "pool/*"},
		}, nil)

		got, err := Rule().Evaluate(context.Background(), debian(), "/dists/stable/InRelease")
		convey.So(err, convey.ShouldBeNil)
		convey.So(got.Allowed, convey.ShouldBeFalse)
		convey.So(got.Scope, convey.ShouldEqual, ScopeGlobal)
		convey.So(got.MatchedRule.ID, convey.ShouldEqual, 1)
	})
}

// TestEvaluate_OtherUpstreamRulesAreNotConsulted 上游内规则只对自己那个上游生效。
func TestEvaluate_OtherUpstreamRulesAreNotConsulted(t *testing.T) {
	convey.Convey("别的上游的规则不参与本次求值", t, func() {
		ruleRepo, _ := setupRuleTest(t)
		ruleRepo.EXPECT().List(gomock.Any()).Return([]*rule_entity.AccessRule{
			{ID: 3, UpstreamID: 8, Action: rule_entity.ActionDeny, Pattern: "dists/*"},
		}, nil)

		got, err := Rule().Evaluate(context.Background(), debian(), "/dists/stable/InRelease")
		convey.So(err, convey.ShouldBeNil)
		convey.So(got.Allowed, convey.ShouldBeTrue)
		convey.So(got.Scope, convey.ShouldEqual, ScopeDefault)
	})
}

// TestEvaluate_SnapshotIsReusedAndInvalidatedOnWrite 规则闸坐在拉取路径上：
// 一次 docker pull 是几十上百个请求，每个请求查一次库等于把镜像站的吞吐绑在
// sqlite 上。但缓存必须在管理接口写入后失效，否则「改完无需重启」就不成立。
func TestEvaluate_SnapshotIsReusedAndInvalidatedOnWrite(t *testing.T) {
	convey.Convey("规则在进程内缓存，写入后失效", t, func() {
		ruleRepo, _ := setupRuleTest(t)
		ctx := context.Background()
		ruleRepo.EXPECT().List(gomock.Any()).Return([]*rule_entity.AccessRule{}, nil).Times(1)

		for i := 0; i < 3; i++ {
			got, err := Rule().Evaluate(ctx, debian(), "/dists/stable/InRelease")
			convey.So(err, convey.ShouldBeNil)
			convey.So(got.Allowed, convey.ShouldBeTrue)
		}

		convey.Convey("新增一条 deny 之后立刻生效，不必重启", func() {
			ruleRepo.EXPECT().Save(gomock.Any(), gomock.Any()).DoAndReturn(
				func(_ context.Context, r *rule_entity.AccessRule) error {
					r.ID = 11
					return nil
				})
			_, err := Rule().Save(ctx, &admin.SaveRuleRequest{
				Action: rule_entity.ActionDeny, Pattern: "dists/*",
			})
			convey.So(err, convey.ShouldBeNil)

			ruleRepo.EXPECT().List(gomock.Any()).Return([]*rule_entity.AccessRule{
				{ID: 11, Action: rule_entity.ActionDeny, Pattern: "dists/*"},
			}, nil).Times(1)
			got, err := Rule().Evaluate(ctx, debian(), "/dists/stable/InRelease")
			convey.So(err, convey.ShouldBeNil)
			convey.So(got.Allowed, convey.ShouldBeFalse)
		})
	})
}

// TestEvaluate_RepoErrorIsNotAnImplicitAllow 读不出规则不能当成放行：
// 一次库故障把整站变成开放代理，比返回一次失败严重得多。
func TestEvaluate_RepoErrorIsNotAnImplicitAllow(t *testing.T) {
	convey.Convey("读规则失败时返回错误而不是放行", t, func() {
		ruleRepo, _ := setupRuleTest(t)
		ruleRepo.EXPECT().List(gomock.Any()).Return(nil, errors.New("库连不上"))

		got, err := Rule().Evaluate(context.Background(), debian(), "/dists/stable/InRelease")
		convey.So(err, convey.ShouldNotBeNil)
		convey.So(got, convey.ShouldBeNil)
	})
}

// TestTest_ReportsVerdictDecidingRuleAndTrace 任务目标 (b)：试算要报出判定、
// 是哪条规则决定的、以及完整的求值过程。
func TestTest_ReportsVerdictDecidingRuleAndTrace(t *testing.T) {
	convey.Convey("试算报出判定、命中的规则和完整过程", t, func() {
		ruleRepo, upRepo := setupRuleTest(t)
		upRepo.EXPECT().FindByHost(gomock.Any(), "deb.debian.org").Return(debian(), nil)
		ruleRepo.EXPECT().List(gomock.Any()).Return([]*rule_entity.AccessRule{
			{ID: 1, UpstreamID: 0, Action: rule_entity.ActionDeny,
				Pattern: "dists/stable/Packages*", Note: "先考察它，但它不匹配"},
			{ID: 2, UpstreamID: 0, Action: rule_entity.ActionDeny, Pattern: "dists/*"},
			{ID: 3, UpstreamID: 7, Action: rule_entity.ActionAllow, Pattern: "dists/*"},
		}, nil)

		resp, err := Rule().Test(context.Background(), &admin.TestRuleRequest{
			Host: "deb.debian.org", Path: "/dists/stable/InRelease",
		})
		convey.So(err, convey.ShouldBeNil)
		convey.So(resp.Allowed, convey.ShouldBeFalse)
		convey.So(resp.Scope, convey.ShouldEqual, admin.Scope(ScopeGlobal))
		convey.So(resp.DefaultPolicy, convey.ShouldEqual, upstream_entity.PolicyAllowAll)

		convey.Convey("报出是哪条规则决定的", func() {
			convey.So(resp.MatchedRule, convey.ShouldNotBeNil)
			convey.So(resp.MatchedRule.ID, convey.ShouldEqual, 2)
			convey.So(resp.MatchedRule.Pattern, convey.ShouldEqual, "dists/*")
			convey.So(resp.MatchedRule.Action, convey.ShouldEqual, rule_entity.ActionDeny)
		})
		convey.Convey("报出为什么：走过的每一步，含没命中的那些", func() {
			convey.So(len(resp.Trace), convey.ShouldEqual, 2)
			convey.So(resp.Trace[0].RuleID, convey.ShouldEqual, 1)
			convey.So(resp.Trace[0].Matched, convey.ShouldBeFalse)
			convey.So(resp.Trace[1].RuleID, convey.ShouldEqual, 2)
			convey.So(resp.Trace[1].Decisive, convey.ShouldBeTrue)
		})
	})
}

// TestTest_AcceptsPastedPullPath 界面上是「把资源地址粘进去」，所以不带 host
// 的整条拉取路径也要能试算。
func TestTest_AcceptsPastedPullPath(t *testing.T) {
	convey.Convey("整条拉取路径能被直接试算", t, func() {
		ruleRepo, upRepo := setupRuleTest(t)
		upRepo.EXPECT().FindByHost(gomock.Any(), "deb.debian.org").Return(debian(), nil)
		ruleRepo.EXPECT().List(gomock.Any()).Return([]*rule_entity.AccessRule{}, nil)

		resp, err := Rule().Test(context.Background(), &admin.TestRuleRequest{
			Path: "/deb.debian.org/dists/stable/InRelease",
		})
		convey.So(err, convey.ShouldBeNil)
		convey.So(resp.Host, convey.ShouldEqual, "deb.debian.org")
		convey.So(resp.Path, convey.ShouldEqual, "/dists/stable/InRelease")
		convey.So(resp.Allowed, convey.ShouldBeTrue)
	})
}

// TestTest_UnknownUpstreamIsAnError 试算是管理接口，这里说清楚「这个上游不在
// 表里」不会泄漏什么——拉取路径上的 404 才是那条不能有差别的路。
func TestTest_UnknownUpstreamIsAnError(t *testing.T) {
	convey.Convey("试算一个不存在的上游时明确报错", t, func() {
		_, upRepo := setupRuleTest(t)
		upRepo.EXPECT().FindByHost(gomock.Any(), "evil.example.com").Return(nil, nil)

		_, err := Rule().Test(context.Background(), &admin.TestRuleRequest{
			Path: "/evil.example.com/x",
		})
		convey.So(err, convey.ShouldNotBeNil)
	})
}

// TestSaveRule_UpstreamMustExist 规则挂在一个不存在的上游上等于一条永远不会被
// 求值的死规则，而界面上看起来它生效了。
func TestSaveRule_UpstreamMustExist(t *testing.T) {
	convey.Convey("上游内规则的上游必须存在", t, func() {
		_, upRepo := setupRuleTest(t)
		upRepo.EXPECT().Find(gomock.Any(), int64(404)).Return(nil, nil)

		_, err := Rule().Save(context.Background(), &admin.SaveRuleRequest{
			UpstreamID: 404, Action: rule_entity.ActionDeny, Pattern: "dists/*",
		})
		convey.So(err, convey.ShouldNotBeNil)
	})
}

// TestDeleteRule 删除走 id，不存在的 id 不能被当成删除成功。
func TestDeleteRule(t *testing.T) {
	convey.Convey("删除规则", t, func() {
		ruleRepo, _ := setupRuleTest(t)
		ctx := context.Background()

		convey.Convey("存在时删除成功", func() {
			ruleRepo.EXPECT().Find(gomock.Any(), int64(5)).Return(
				&rule_entity.AccessRule{ID: 5}, nil)
			ruleRepo.EXPECT().Delete(gomock.Any(), int64(5)).Return(nil)
			_, err := Rule().Delete(ctx, &admin.DeleteRuleRequest{ID: 5})
			convey.So(err, convey.ShouldBeNil)
		})

		convey.Convey("不存在时报错", func() {
			ruleRepo.EXPECT().Find(gomock.Any(), int64(404)).Return(nil, nil)
			_, err := Rule().Delete(ctx, &admin.DeleteRuleRequest{ID: 404})
			convey.So(err, convey.ShouldNotBeNil)
		})
	})
}

// TestListRules 列表把存储形态映射成对外结构，全局规则的 upstream_id 是 0。
func TestListRules(t *testing.T) {
	convey.Convey("列出全部规则", t, func() {
		ruleRepo, _ := setupRuleTest(t)
		ruleRepo.EXPECT().List(gomock.Any()).Return([]*rule_entity.AccessRule{
			{ID: 1, UpstreamID: 0, Action: rule_entity.ActionDeny, Pattern: "*:latest", Note: "禁 latest"},
		}, nil)

		resp, err := Rule().List(context.Background(), &admin.ListRulesRequest{})
		convey.So(err, convey.ShouldBeNil)
		convey.So(len(resp.List), convey.ShouldEqual, 1)
		convey.So(resp.List[0].UpstreamID, convey.ShouldEqual, 0)
		convey.So(resp.List[0].Pattern, convey.ShouldEqual, "*:latest")
		convey.So(resp.List[0].Note, convey.ShouldEqual, "禁 latest")
	})
}

// TestListRules_OrderedBySpecificity 管理界面上那张表是「按具体度排序的规则表」：
// 库里那个 pattern 字典序只是个稳定的底序，真按它显示，界面上的先后和求值时的
// 先后就是两回事了。两边必须出自同一个 MoreSpecific（决策 15）。
func TestListRules_OrderedBySpecificity(t *testing.T) {
	convey.Convey("规则表按层分组、层内按具体度排序", t, func() {
		ruleRepo, _ := setupRuleTest(t)
		// 这是仓储那条 ORDER BY upstream_id asc,pattern asc 给出的顺序，
		// 它和具体度顺序恰好相反。
		ruleRepo.EXPECT().List(gomock.Any()).Return([]*rule_entity.AccessRule{
			{ID: 1, UpstreamID: 0, Action: rule_entity.ActionDeny, Pattern: "*:latest"},
			{ID: 2, UpstreamID: 0, Action: rule_entity.ActionAllow, Pattern: "library/alpine:*"},
			{ID: 3, UpstreamID: 7, Action: rule_entity.ActionDeny, Pattern: "dists/*"},
			{ID: 4, UpstreamID: 7, Action: rule_entity.ActionAllow, Pattern: "dists/stable/InRelease"},
		}, nil)

		resp, err := Rule().List(context.Background(), &admin.ListRulesRequest{})
		convey.So(err, convey.ShouldBeNil)
		convey.So(len(resp.List), convey.ShouldEqual, 4)
		// 全局层整层在前（决策 14），层内字面前缀长者先说了算。
		convey.So(resp.List[0].ID, convey.ShouldEqual, 2)
		convey.So(resp.List[1].ID, convey.ShouldEqual, 1)
		convey.So(resp.List[2].ID, convey.ShouldEqual, 4)
		convey.So(resp.List[3].ID, convey.ShouldEqual, 3)
	})
}
