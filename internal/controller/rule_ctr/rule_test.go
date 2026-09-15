// 用例放在 _test 包里：它经 api.Router 注册真实路由，而 api 包又依赖本包，
// 同包会构成导入环。走 _test 包换来的是「测的就是生产路由 + 生产中间件」。
package rule_ctr_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cago-frame/cago/server/mux/muxclient"
	"github.com/cago-frame/cago/server/mux/muxtest"
	"github.com/gin-gonic/gin"
	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"
	"golang.org/x/crypto/bcrypt"

	"github.com/CodFrm/katch/internal/api"
	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/model/entity/rule_entity"
	"github.com/CodFrm/katch/internal/model/entity/setting_entity"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/repository/rule_repo"
	mock_rule_repo "github.com/CodFrm/katch/internal/repository/rule_repo/mock"
	"github.com/CodFrm/katch/internal/repository/setting_repo"
	mock_setting_repo "github.com/CodFrm/katch/internal/repository/setting_repo/mock"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
	mock_upstream_repo "github.com/CodFrm/katch/internal/repository/upstream_repo/mock"
	"github.com/CodFrm/katch/internal/service/rule_svc"
	"github.com/CodFrm/katch/internal/service/setting_svc"
)

const adminKey = "correct-admin-key"

func setupRuleAdminTest(t *testing.T) (
	*mock_rule_repo.MockAccessRuleRepo, *mock_upstream_repo.MockUpstreamRepo, *muxtest.TestMux,
) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctrl := gomock.NewController(t)
	ruleRepo := mock_rule_repo.NewMockAccessRuleRepo(ctrl)
	rule_repo.RegisterAccessRule(ruleRepo)
	upRepo := mock_upstream_repo.NewMockUpstreamRepo(ctrl)
	upstream_repo.RegisterUpstream(upRepo)
	setRepo := mock_setting_repo.NewMockSettingRepo(ctrl)
	setting_repo.RegisterSetting(setRepo)
	// 规则在进程内有快照，沿用上一个用例那份会串味。
	prev := rule_svc.Rule()
	rule_svc.Register(rule_svc.New())
	t.Cleanup(func() { rule_svc.Register(prev) })

	// MinCost：用例只关心比对结果，不关心 bcrypt 的计算代价。
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
	return ruleRepo, upRepo, testMux
}

func adminHeader(key string) muxclient.ClientDoOption {
	return muxclient.WithHeader(http.Header{"Authorization": []string{"Bearer " + key}})
}

// TestRuleTester_ReportsWhichRuleDecidedAndWhy 任务目标 (b)：试算接口必须报出
// 判定、是哪条规则决定的、以及完整的求值过程。
//
// 它是决策 15 的配套：具体度定序换来了确定性，代价是「为什么是这条命中」不再
// 显然——只回一个 allowed 的接口，等于让人对着一份看不出优先级的列表自己推。
func TestRuleTester_ReportsWhichRuleDecidedAndWhy(t *testing.T) {
	ruleRepo, upRepo, testMux := setupRuleAdminTest(t)
	convey.Convey("规则试算报出判定、命中的规则和完整过程", t, func() {
		upRepo.EXPECT().FindByHost(gomock.Any(), "deb.debian.org").Return(
			&upstream_entity.Upstream{
				ID: 7, Host: "deb.debian.org", Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic}, Enabled: true,
				DefaultPolicy: upstream_entity.PolicyAllowAll,
			}, nil).AnyTimes()
		// AnyTimes：goconvey 每个叶子都会把外层重跑一遍，而规则在进程内有快照，
		// 重跑时不会再查库。判据是下面那些断言，不是调用次数。
		ruleRepo.EXPECT().List(gomock.Any()).Return([]*rule_entity.AccessRule{
			// 字面前缀更长，先被考察，但它不匹配。
			{ID: 1, Action: rule_entity.ActionDeny, Pattern: "dists/stable/Packages*"},
			{ID: 2, Action: rule_entity.ActionDeny, Pattern: "dists/*", Note: "全站禁索引"},
			{ID: 3, UpstreamID: 7, Action: rule_entity.ActionAllow, Pattern: "dists/*"},
		}, nil).AnyTimes()

		resp := &admin.TestRuleResponse{}
		err := testMux.Do(context.Background(), &admin.TestRuleRequest{
			Path: "/deb.debian.org/dists/stable/InRelease",
		}, resp, adminHeader(adminKey))
		convey.So(err, convey.ShouldBeNil)

		convey.Convey("判定", func() {
			convey.So(resp.Host, convey.ShouldEqual, "deb.debian.org")
			convey.So(resp.Path, convey.ShouldEqual, "/dists/stable/InRelease")
			convey.So(resp.Allowed, convey.ShouldBeFalse)
			convey.So(string(resp.Scope), convey.ShouldEqual, "global")
			convey.So(resp.DefaultPolicy, convey.ShouldEqual, upstream_entity.PolicyAllowAll)
		})
		convey.Convey("是哪条规则决定的", func() {
			convey.So(resp.MatchedRule, convey.ShouldNotBeNil)
			convey.So(resp.MatchedRule.ID, convey.ShouldEqual, 2)
			convey.So(resp.MatchedRule.Pattern, convey.ShouldEqual, "dists/*")
			convey.So(resp.MatchedRule.Note, convey.ShouldEqual, "全站禁索引")
		})
		convey.Convey("为什么：完整的求值过程，含考察过但没命中的那条", func() {
			convey.So(len(resp.Trace), convey.ShouldEqual, 2)
			convey.So(resp.Trace[0].Pattern, convey.ShouldEqual, "dists/stable/Packages*")
			convey.So(resp.Trace[0].Matched, convey.ShouldBeFalse)
			convey.So(resp.Trace[1].RuleID, convey.ShouldEqual, 2)
			convey.So(resp.Trace[1].Matched, convey.ShouldBeTrue)
			convey.So(resp.Trace[1].Decisive, convey.ShouldBeTrue)
		})
	})
}

// TestRuleAdmin_RequiresKey 规则是破坏性的：能改规则就能把整站打开或关掉，
// 它必须和其余管理接口一样要密钥。
func TestRuleAdmin_RequiresKey(t *testing.T) {
	// ruleRepo 上一个 EXPECT 都没有：一旦鉴权放行进 service，mock 会当场失败。
	_, _, testMux := setupRuleAdminTest(t)
	convey.Convey("规则接口没有密钥一律 401", t, func() {
		engine, ok := testMux.IRouter.(*gin.Engine)
		convey.So(ok, convey.ShouldBeTrue)
		for _, r := range []any{
			&admin.ListRulesRequest{},
			&admin.SaveRuleRequest{Action: "deny", Pattern: "*"},
			&admin.TestRuleRequest{Path: "/deb.debian.org/x"},
		} {
			req, err := testMux.Request(context.Background(), r)
			convey.So(err, convey.ShouldBeNil)
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)
			convey.So(w.Code, convey.ShouldEqual, http.StatusUnauthorized)
		}
	})
}

// TestRuleAdmin_SaveAndList 规则 CRUD：写进去的那条能被列表读回。
func TestRuleAdmin_SaveAndList(t *testing.T) {
	ruleRepo, _, testMux := setupRuleAdminTest(t)
	convey.Convey("新增的规则能被列表读到", t, func() {
		var stored *rule_entity.AccessRule
		ruleRepo.EXPECT().Save(gomock.Any(), gomock.Any()).DoAndReturn(
			func(_ context.Context, r *rule_entity.AccessRule) error {
				r.ID = 11
				stored = r
				return nil
			})
		saveResp := &admin.SaveRuleResponse{}
		convey.So(testMux.Do(context.Background(), &admin.SaveRuleRequest{
			Action: "deny", Pattern: "*:latest", Note: "禁 latest",
		}, saveResp, adminHeader(adminKey)), convey.ShouldBeNil)
		convey.So(saveResp.ID, convey.ShouldEqual, 11)
		convey.So(stored.Createtime, convey.ShouldBeGreaterThan, 0)

		ruleRepo.EXPECT().List(gomock.Any()).Return([]*rule_entity.AccessRule{stored}, nil)
		listResp := &admin.ListRulesResponse{}
		convey.So(testMux.Do(context.Background(), &admin.ListRulesRequest{}, listResp,
			adminHeader(adminKey)), convey.ShouldBeNil)
		convey.So(len(listResp.List), convey.ShouldEqual, 1)
		convey.So(listResp.List[0].ID, convey.ShouldEqual, 11)
		convey.So(listResp.List[0].Pattern, convey.ShouldEqual, "*:latest")
		convey.So(listResp.List[0].UpstreamID, convey.ShouldEqual, 0)
	})

	convey.Convey("动作只有 allow 和 deny，别的值当场被挡住", t, func() {
		convey.So(testMux.Do(context.Background(), &admin.SaveRuleRequest{
			Action: "drop", Pattern: "*:latest",
		}, &admin.SaveRuleResponse{}, adminHeader(adminKey)), convey.ShouldNotBeNil)
	})

	convey.Convey("空模式被挡住：它什么都不匹配，存进去是一条死规则", t, func() {
		convey.So(testMux.Do(context.Background(), &admin.SaveRuleRequest{
			Action: "deny",
		}, &admin.SaveRuleResponse{}, adminHeader(adminKey)), convey.ShouldNotBeNil)
	})
}

// TestRuleAdmin_Delete 删除走 id，不存在的 id 不能被当成删除成功。
func TestRuleAdmin_Delete(t *testing.T) {
	ruleRepo, _, testMux := setupRuleAdminTest(t)
	convey.Convey("删除规则", t, func() {
		// 删除前会先列一次规则：事件流要记下被删的那条长什么样（见 rule_ctr 的 ruleOf）。
		ruleRepo.EXPECT().List(gomock.Any()).Return([]*rule_entity.AccessRule{
			{ID: 11, Action: rule_entity.ActionDeny, Pattern: "*:latest"},
		}, nil).AnyTimes()
		ruleRepo.EXPECT().Find(gomock.Any(), int64(11)).Return(&rule_entity.AccessRule{ID: 11}, nil)
		ruleRepo.EXPECT().Delete(gomock.Any(), int64(11)).Return(nil)
		convey.So(testMux.Do(context.Background(), &admin.DeleteRuleRequest{ID: 11},
			&admin.DeleteRuleResponse{}, adminHeader(adminKey)), convey.ShouldBeNil)
	})
}
