// 用例放在 _test 包里：它经 api.Router 注册真实路由，而 api 包又依赖本包，
// 同包会构成导入环。走 _test 包换来的是「测的就是生产路由 + 生产中间件」。
package event_ctr_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"

	"github.com/cago-frame/cago/server/mux/muxclient"
	"github.com/cago-frame/cago/server/mux/muxtest"
	"github.com/gin-gonic/gin"
	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"
	"golang.org/x/crypto/bcrypt"

	"github.com/CodFrm/katch/internal/api"
	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/model/entity/event_entity"
	"github.com/CodFrm/katch/internal/model/entity/rule_entity"
	"github.com/CodFrm/katch/internal/model/entity/setting_entity"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/repository/event_repo"
	mock_event_repo "github.com/CodFrm/katch/internal/repository/event_repo/mock"
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

// fakeEventRepo 给 mock 背的内存表。
//
// 事件流要证明的是「这次人为变更之后，接口上多出了这一条，并排在最前面」——
// 逐次 EXPECT 返回固定值写不出这种前后关系，那样的用例即使根本没落库也照样绿。
type fakeEventRepo struct {
	mu sync.Mutex
	// failCreate 为非 nil 时每次写入都失败，用来验「记不上事件不许影响被观察的操作」。
	failCreate error
	next       int64
	rows       []*event_entity.Event
}

func newFakeEventRepo(t *testing.T, failCreate error) *fakeEventRepo {
	t.Helper()
	f := &fakeEventRepo{next: 1, failCreate: failCreate}
	m := mock_event_repo.NewMockEventRepo(gomock.NewController(t))
	m.EXPECT().Create(gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(_ context.Context, e *event_entity.Event) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.failCreate != nil {
				return f.failCreate
			}
			e.ID = f.next
			f.next++
			dup := *e
			f.rows = append(f.rows, &dup)
			return nil
		})
	m.EXPECT().List(gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(_ context.Context, limit int) ([]*event_entity.Event, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			// 倒序由仓储负责（SQL 的 ORDER BY），内存表在这里照着做一遍。
			list := make([]*event_entity.Event, 0, len(f.rows))
			for i := len(f.rows) - 1; i >= 0 && len(list) < limit; i-- {
				dup := *f.rows[i]
				list = append(list, &dup)
			}
			return list, nil
		})
	event_repo.RegisterEvent(m)
	return f
}

// setupEventTest 装配「仓储是内存表、路由与中间件是生产那套」的环境。
func setupEventTest(t *testing.T, failCreate error) (
	*mock_rule_repo.MockAccessRuleRepo, *mock_upstream_repo.MockUpstreamRepo, *muxtest.TestMux,
) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctrl := gomock.NewController(t)
	newFakeEventRepo(t, failCreate)
	ruleRepo := mock_rule_repo.NewMockAccessRuleRepo(ctrl)
	// 删除上游会连带删掉它名下的规则；这里只是让它别挡路，级联本身在
	// upstream_svc 的用例里断言。
	ruleRepo.EXPECT().DeleteByUpstream(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	rule_repo.RegisterAccessRule(ruleRepo)
	upRepo := mock_upstream_repo.NewMockUpstreamRepo(ctrl)
	upRepo.EXPECT().Find(gomock.Any(), int64(7)).Return(
		&upstream_entity.Upstream{ID: 7, Host: "deb.debian.org"}, nil).AnyTimes()
	upstream_repo.RegisterUpstream(upRepo)
	// 规则在进程内有快照，沿用上一个用例那份会串味。
	prev := rule_svc.Rule()
	rule_svc.Register(rule_svc.New())
	t.Cleanup(func() { rule_svc.Register(prev) })

	newFakeSettingRepo(t)

	testMux := muxtest.NewTestMux()
	if err := api.Router(context.Background(), testMux.Router); err != nil {
		t.Fatal(err)
	}
	return ruleRepo, upRepo, testMux
}

// newFakeSettingRepo 设置表的内存版，出厂就带着当前管理密钥的哈希。
//
// 要内存表而不是逐次 EXPECT：设置写入会先 Find 再 Save，而密钥轮换之后
// 「旧密钥立刻失效」靠的就是下一次 Find 读到新写进去的那一行。
func newFakeSettingRepo(t *testing.T) {
	t.Helper()
	// MinCost：用例只关心比对结果，不关心 bcrypt 的计算代价。
	hash, err := bcrypt.GenerateFromPassword([]byte(adminKey), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	rows := map[string]*setting_entity.Setting{
		setting_svc.AdminKeyHashSetting: {Key: setting_svc.AdminKeyHashSetting, Value: string(hash)},
	}
	m := mock_setting_repo.NewMockSettingRepo(gomock.NewController(t))
	m.EXPECT().Find(gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(_ context.Context, key string) (*setting_entity.Setting, error) {
			mu.Lock()
			defer mu.Unlock()
			row, ok := rows[key]
			if !ok {
				return nil, nil
			}
			dup := *row
			return &dup, nil
		})
	m.EXPECT().Save(gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(_ context.Context, setting *setting_entity.Setting) error {
			mu.Lock()
			defer mu.Unlock()
			dup := *setting
			rows[setting.Key] = &dup
			return nil
		})
	setting_repo.RegisterSetting(m)
}

func adminHeader(key string) muxclient.ClientDoOption {
	return muxclient.WithHeader(http.Header{"Authorization": []string{"Bearer " + key}})
}

// enumPattern 稳定枚举长什么样：小写、下划线，没有空格也没有句子。
//
// 界面要按 kind 与 actor 查翻译表（spec：后端返回的英文串不直接贴给用户），
// 一旦这两个字段变成人话，翻译就只能退化成原样展示。
var enumPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// saveRule 经管理接口改一条规则，返回它的 id。
func saveRule(t *testing.T, testMux *muxtest.TestMux, req *admin.SaveRuleRequest) int64 {
	t.Helper()
	resp := &admin.SaveRuleResponse{}
	if err := testMux.Do(context.Background(), req, resp, adminHeader(adminKey)); err != nil {
		t.Fatalf("保存规则失败：%v", err)
	}
	return resp.ID
}

// TestEventFeed_RuleChangeAppearsWithActorAndKind 任务目标 (a)：改一条访问规则后，
// 事件接口按时间倒序给出这条变更，带操作人和稳定的 kind 枚举。
//
// 「同一条时间线上的人为变更」是概览那一节的要求；它也是 Out of scope 里
// 「规则不做草稿与回滚，但变更记进事件流」那条的另一半——不落这条记录，
// 一条规则被谁在什么时候改成现在这样就再也查不出来了。
func TestEventFeed_RuleChangeAppearsWithActorAndKind(t *testing.T) {
	convey.Convey("改一条规则之后，事件接口给得出这条变更", t, func() {
		ruleRepo, _, testMux := setupEventTest(t, nil)
		ruleRepo.EXPECT().Find(gomock.Any(), int64(11)).Return(
			&rule_entity.AccessRule{ID: 11, UpstreamID: 7, Action: rule_entity.ActionDeny,
				Pattern: "dists/*"}, nil).AnyTimes()
		ruleRepo.EXPECT().Save(gomock.Any(), gomock.Any()).AnyTimes().
			DoAndReturn(func(_ context.Context, r *rule_entity.AccessRule) error {
				if r.ID == 0 {
					r.ID = 12
				}
				return nil
			})

		saveRule(t, testMux, &admin.SaveRuleRequest{
			ID: 11, UpstreamID: 7, Action: "allow", Pattern: "dists/*", Note: "放开索引",
		})

		resp := &admin.ListEventsResponse{}
		convey.So(testMux.Do(context.Background(), &admin.ListEventsRequest{}, resp,
			adminHeader(adminKey)), convey.ShouldBeNil)
		convey.So(len(resp.List), convey.ShouldEqual, 1)

		item := resp.List[0]
		convey.Convey("kind 是稳定枚举，界面据此翻译", func() {
			convey.So(item.Kind, convey.ShouldEqual, event_entity.KindRuleUpdated)
			convey.So(enumPattern.MatchString(item.Kind), convey.ShouldBeTrue)
		})
		convey.Convey("带操作人，且人为变更与自动事件分得开", func() {
			convey.So(item.Actor, convey.ShouldEqual, event_entity.ActorAdmin)
			convey.So(enumPattern.MatchString(item.Actor), convey.ShouldBeTrue)
			convey.So(item.Actor, convey.ShouldNotEqual, event_entity.ActorSystem)
		})
		convey.Convey("改了什么记在结构化字段里，而不是一句拼好的话", func() {
			detail := map[string]any{}
			convey.So(json.Unmarshal(item.Detail, &detail), convey.ShouldBeNil)
			convey.So(detail["pattern"], convey.ShouldEqual, "dists/*")
			convey.So(detail["action"], convey.ShouldEqual, "allow")
			convey.So(detail["rule_id"], convey.ShouldEqual, 11)
			convey.So(item.UpstreamID, convey.ShouldEqual, 7)
		})
		convey.Convey("带时刻，时间线排得出来", func() {
			convey.So(item.Createtime, convey.ShouldBeGreaterThan, 0)
		})
	})
}

// TestEventFeed_NewestFirstWithLimit 时间线是倒序的，并且能只要最近 N 条。
func TestEventFeed_NewestFirstWithLimit(t *testing.T) {
	convey.Convey("事件按时间倒序，limit 只取最近几条", t, func() {
		ruleRepo, _, testMux := setupEventTest(t, nil)
		ruleRepo.EXPECT().Save(gomock.Any(), gomock.Any()).AnyTimes().
			DoAndReturn(func(_ context.Context, r *rule_entity.AccessRule) error {
				r.ID = 20
				return nil
			})
		for _, pattern := range []string{"a/*", "b/*", "c/*"} {
			saveRule(t, testMux, &admin.SaveRuleRequest{Action: "deny", Pattern: pattern})
		}

		convey.Convey("不给 limit 时按新到旧全给出来", func() {
			resp := &admin.ListEventsResponse{}
			convey.So(testMux.Do(context.Background(), &admin.ListEventsRequest{}, resp,
				adminHeader(adminKey)), convey.ShouldBeNil)
			convey.So(len(resp.List), convey.ShouldEqual, 3)
			patterns := make([]string, 0, 3)
			for _, item := range resp.List {
				detail := map[string]any{}
				convey.So(json.Unmarshal(item.Detail, &detail), convey.ShouldBeNil)
				patterns = append(patterns, detail["pattern"].(string))
			}
			convey.So(patterns, convey.ShouldResemble, []string{"c/*", "b/*", "a/*"})
		})

		convey.Convey("给了 limit 就只给最近那几条", func() {
			resp := &admin.ListEventsResponse{}
			convey.So(testMux.Do(context.Background(), &admin.ListEventsRequest{Limit: 2}, resp,
				adminHeader(adminKey)), convey.ShouldBeNil)
			convey.So(len(resp.List), convey.ShouldEqual, 2)
			detail := map[string]any{}
			convey.So(json.Unmarshal(resp.List[0].Detail, &detail), convey.ShouldBeNil)
			convey.So(detail["pattern"], convey.ShouldEqual, "c/*")
		})
	})
}

// TestEventFeed_RecordsEveryHumanChange 人为变更的几个面都要落在同一条时间线上：
// 上游、规则、设置、密钥轮换。少记一类，界面上那条时间线就会在最需要的时候有洞。
func TestEventFeed_RecordsEveryHumanChange(t *testing.T) {
	convey.Convey("规则的增删都记，且新增与更新是两个 kind", t, func() {
		ruleRepo, _, testMux := setupEventTest(t, nil)
		ruleRepo.EXPECT().Save(gomock.Any(), gomock.Any()).AnyTimes().
			DoAndReturn(func(_ context.Context, r *rule_entity.AccessRule) error {
				r.ID = 31
				return nil
			})
		ruleRepo.EXPECT().Find(gomock.Any(), int64(31)).Return(
			&rule_entity.AccessRule{ID: 31, Action: rule_entity.ActionDeny, Pattern: "x/*"}, nil)
		ruleRepo.EXPECT().Delete(gomock.Any(), int64(31)).Return(nil)
		// 删除前会先列一次规则：删完这行就没了，事件里得留下它长什么样。
		ruleRepo.EXPECT().List(gomock.Any()).Return([]*rule_entity.AccessRule{
			{ID: 31, Action: rule_entity.ActionDeny, Pattern: "x/*"},
		}, nil).AnyTimes()

		saveRule(t, testMux, &admin.SaveRuleRequest{Action: "deny", Pattern: "x/*"})
		convey.So(testMux.Do(context.Background(), &admin.DeleteRuleRequest{ID: 31},
			&admin.DeleteRuleResponse{}, adminHeader(adminKey)), convey.ShouldBeNil)

		resp := &admin.ListEventsResponse{}
		convey.So(testMux.Do(context.Background(), &admin.ListEventsRequest{}, resp,
			adminHeader(adminKey)), convey.ShouldBeNil)
		convey.So(len(resp.List), convey.ShouldEqual, 2)
		convey.So(resp.List[0].Kind, convey.ShouldEqual, event_entity.KindRuleDeleted)
		convey.So(resp.List[1].Kind, convey.ShouldEqual, event_entity.KindRuleCreated)

		convey.Convey("删除记下被删的那条长什么样——规则本身已经不在了", func() {
			detail := map[string]any{}
			convey.So(json.Unmarshal(resp.List[0].Detail, &detail), convey.ShouldBeNil)
			convey.So(detail["pattern"], convey.ShouldEqual, "x/*")
			convey.So(detail["action"], convey.ShouldEqual, rule_entity.ActionDeny)
		})
	})
}

// TestEventFeed_FailedRecordDoesNotFailTheChange 任务目标 (d)：事件是旁路的观察记录，
// 它写不进去不许把一次已经生效的管理写入变成 500。
//
// 反过来的实现会让一张写满的磁盘把整个后台一起锁死：规则改不了、上游停不掉，
// 而真正出问题的只是一条日志。
func TestEventFeed_FailedRecordDoesNotFailTheChange(t *testing.T) {
	convey.Convey("事件写入失败不影响被观察的那次操作", t, func() {
		ruleRepo, _, testMux := setupEventTest(t, errors.New("events 表写不进去"))
		var stored *rule_entity.AccessRule
		ruleRepo.EXPECT().Save(gomock.Any(), gomock.Any()).
			DoAndReturn(func(_ context.Context, r *rule_entity.AccessRule) error {
				r.ID = 41
				stored = r
				return nil
			})

		resp := &admin.SaveRuleResponse{}
		err := testMux.Do(context.Background(), &admin.SaveRuleRequest{
			Action: "deny", Pattern: "y/*",
		}, resp, adminHeader(adminKey))
		convey.So(err, convey.ShouldBeNil)
		convey.So(resp.ID, convey.ShouldEqual, 41)
		// 规则真的落库了：接口回了 200 而库里什么都没有才是最坏的结果。
		convey.So(stored, convey.ShouldNotBeNil)
		convey.So(stored.Pattern, convey.ShouldEqual, "y/*")
	})
}

// TestEventFeed_RequiresKey 事件流里有主机名、规则模式和设置键，是运营数据；
// 它和其余管理接口同一道闸。
func TestEventFeed_RequiresKey(t *testing.T) {
	_, _, testMux := setupEventTest(t, nil)
	convey.Convey("事件接口没有密钥一律 401", t, func() {
		engine, ok := testMux.IRouter.(*gin.Engine)
		convey.So(ok, convey.ShouldBeTrue)
		req, err := testMux.Request(context.Background(), &admin.ListEventsRequest{})
		convey.So(err, convey.ShouldBeNil)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		convey.So(w.Code, convey.ShouldEqual, http.StatusUnauthorized)
	})
}

// TestEventFeed_RecordsUpstreamChanges 上游的增删改是这台镜像站最要紧的人为变更：
// 一个上游被谁停掉，恰恰是「拉取突然全挂了」之后第一个要查的东西。
func TestEventFeed_RecordsUpstreamChanges(t *testing.T) {
	convey.Convey("上游的新增、修改、删除各落一条事件", t, func() {
		_, upRepo, testMux := setupEventTest(t, nil)
		upRepo.EXPECT().FindByHost(gomock.Any(), "deb.debian.org").Return(nil, nil).AnyTimes()
		upRepo.EXPECT().Save(gomock.Any(), gomock.Any()).AnyTimes().
			DoAndReturn(func(_ context.Context, u *upstream_entity.Upstream) error {
				if u.ID == 0 {
					u.ID = 7
				}
				return nil
			})
		upRepo.EXPECT().Delete(gomock.Any(), int64(7)).Return(nil)
		// 删除前要先把主机名取出来：删完这条记录就没了，事件里只留一个 id 的话，
		// 界面上这条「上游已删除」永远指不出删的是哪一个。
		upRepo.EXPECT().List(gomock.Any()).Return([]*upstream_entity.Upstream{
			{ID: 7, Host: "deb.debian.org", Kind: upstream_entity.KindStatic},
		}, nil).AnyTimes()

		create := &admin.SaveUpstreamRequest{
			Host: "deb.debian.org", Kind: upstream_entity.KindStatic,
			Origin: "https://deb.debian.org", Enabled: true,
		}
		convey.So(testMux.Do(context.Background(), create, &admin.SaveUpstreamResponse{},
			adminHeader(adminKey)), convey.ShouldBeNil)

		update := &admin.UpdateUpstreamRequest{
			ID: 7, Host: "deb.debian.org", Kind: upstream_entity.KindStatic,
			Origin: "https://deb.debian.org", Enabled: false,
		}
		convey.So(testMux.Do(context.Background(), update, &admin.UpdateUpstreamResponse{},
			adminHeader(adminKey)), convey.ShouldBeNil)

		convey.So(testMux.Do(context.Background(), &admin.DeleteUpstreamRequest{ID: 7},
			&admin.DeleteUpstreamResponse{}, adminHeader(adminKey)), convey.ShouldBeNil)

		resp := &admin.ListEventsResponse{}
		convey.So(testMux.Do(context.Background(), &admin.ListEventsRequest{}, resp,
			adminHeader(adminKey)), convey.ShouldBeNil)
		convey.So(len(resp.List), convey.ShouldEqual, 3)
		convey.So(resp.List[0].Kind, convey.ShouldEqual, event_entity.KindUpstreamDeleted)
		convey.So(resp.List[1].Kind, convey.ShouldEqual, event_entity.KindUpstreamUpdated)
		convey.So(resp.List[2].Kind, convey.ShouldEqual, event_entity.KindUpstreamCreated)
		for _, item := range resp.List {
			convey.So(item.Actor, convey.ShouldEqual, event_entity.ActorAdmin)
			convey.So(item.UpstreamID, convey.ShouldEqual, 7)
		}

		convey.Convey("删除记下被删的是哪个主机名——那条上游记录已经不在了", func() {
			detail := map[string]any{}
			convey.So(json.Unmarshal(resp.List[0].Detail, &detail), convey.ShouldBeNil)
			convey.So(detail["host"], convey.ShouldEqual, "deb.debian.org")
		})

		convey.Convey("停用这件事记在细节里，而不是靠一句话描述", func() {
			detail := map[string]any{}
			convey.So(json.Unmarshal(resp.List[1].Detail, &detail), convey.ShouldBeNil)
			convey.So(detail["host"], convey.ShouldEqual, "deb.debian.org")
			convey.So(detail["enabled"], convey.ShouldBeFalse)
		})
	})
}

// TestEventFeed_RecordsSettingChangeAndKeyRotation 设置与密钥轮换同样进时间线。
//
// 轮换尤其要记：它换掉的是后台的唯一凭据，事后除了这条记录再没有别的地方
// 能说出它什么时候被换过。
func TestEventFeed_RecordsSettingChangeAndKeyRotation(t *testing.T) {
	convey.Convey("改设置与轮换密钥各落一条事件", t, func() {
		_, _, testMux := setupEventTest(t, nil)
		convey.So(testMux.Do(context.Background(), &admin.SaveSettingsRequest{
			Settings: map[string]json.RawMessage{
				"public_homepage":   json.RawMessage(`false`),
				"cache_quota_bytes": json.RawMessage(`5368709120`),
			},
		}, &admin.SaveSettingsResponse{}, adminHeader(adminKey)), convey.ShouldBeNil)

		const newKey = "rotated-admin-key-0002"
		convey.So(testMux.Do(context.Background(), &admin.RotateAdminKeyRequest{NewKey: newKey},
			&admin.RotateAdminKeyResponse{}, adminHeader(adminKey)), convey.ShouldBeNil)

		resp := &admin.ListEventsResponse{}
		// 用新密钥读：旧的那把已经在上一行失效了。
		convey.So(testMux.Do(context.Background(), &admin.ListEventsRequest{}, resp,
			adminHeader(newKey)), convey.ShouldBeNil)
		convey.So(len(resp.List), convey.ShouldEqual, 2)

		convey.Convey("轮换记事实，但绝不记密钥本身", func() {
			convey.So(resp.List[0].Kind, convey.ShouldEqual, event_entity.KindAdminKeyRotated)
			convey.So(resp.List[0].Actor, convey.ShouldEqual, event_entity.ActorAdmin)
			convey.So(string(resp.List[0].Detail), convey.ShouldNotContainSubstring, newKey)
		})

		convey.Convey("改了哪几个键记在细节里，按键名排序，值不进去", func() {
			convey.So(resp.List[1].Kind, convey.ShouldEqual, event_entity.KindSettingChanged)
			detail := struct {
				Keys []string `json:"keys"`
			}{}
			convey.So(json.Unmarshal(resp.List[1].Detail, &detail), convey.ShouldBeNil)
			convey.So(detail.Keys, convey.ShouldResemble, []string{"cache_quota_bytes", "public_homepage"})
		})
	})
}
