// 用例放在 _test 包里：它经 api.Router 注册真实路由，而 api 包又依赖本包，
// 同包会构成导入环。走 _test 包换来的是「测的就是生产路由 + 生产中间件」。
package setting_ctr_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/cago-frame/cago/pkg/utils/httputils"
	"github.com/cago-frame/cago/server/mux/muxclient"
	"github.com/cago-frame/cago/server/mux/muxtest"
	"github.com/gin-gonic/gin"
	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"
	"golang.org/x/crypto/bcrypt"

	"github.com/CodFrm/katch/internal/api"
	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/model/entity/setting_entity"
	"github.com/CodFrm/katch/internal/repository/setting_repo"
	mock_setting_repo "github.com/CodFrm/katch/internal/repository/setting_repo/mock"
	"github.com/CodFrm/katch/internal/service/setting_svc"
)

const initialKey = "initial-admin-key-0001"

// fakeSettingRepo 给 mock 背的内存表。
//
// 这里要的是「写进去的下一次读得到」，而密钥轮换恰恰是「写完之后同一个进程里
// 立刻改判」——逐次 EXPECT 返回固定值写不出这种前后关系，那样的用例即使实现
// 根本没落库也照样绿。
type fakeSettingRepo struct {
	mu   sync.Mutex
	rows map[string]*setting_entity.Setting
}

func newFakeSettingRepo(t *testing.T) *fakeSettingRepo {
	t.Helper()
	f := &fakeSettingRepo{rows: map[string]*setting_entity.Setting{}}
	m := mock_setting_repo.NewMockSettingRepo(gomock.NewController(t))
	m.EXPECT().Find(gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(_ context.Context, key string) (*setting_entity.Setting, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			row, ok := f.rows[key]
			if !ok {
				return nil, nil
			}
			dup := *row
			return &dup, nil
		})
	m.EXPECT().Save(gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(_ context.Context, s *setting_entity.Setting) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			dup := *s
			f.rows[s.Key] = &dup
			return nil
		})
	setting_repo.RegisterSetting(m)
	return f
}

func (f *fakeSettingRepo) value(key string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[key]
	if !ok {
		return "", false
	}
	return row.Value, true
}

// setupSettingTest 装配「repo 是内存表、路由与中间件是生产那套」的环境，
// 并按决策 18 把 config.yaml 里的初始密钥落一份哈希进去。
func setupSettingTest(t *testing.T) (*fakeSettingRepo, *muxtest.TestMux, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	repo := newFakeSettingRepo(t)
	// MinCost：用例只关心比对结果，不关心 bcrypt 的计算代价。
	hash, err := bcrypt.GenerateFromPassword([]byte(initialKey), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := setting_repo.Setting().Save(context.Background(), &setting_entity.Setting{
		Key: setting_svc.AdminKeyHashSetting, Value: string(hash),
	}); err != nil {
		t.Fatal(err)
	}

	testMux := muxtest.NewTestMux()
	if err := api.Router(context.Background(), testMux.Router); err != nil {
		t.Fatal(err)
	}
	engine, ok := testMux.IRouter.(*gin.Engine)
	if !ok {
		t.Fatal("muxtest 的 IRouter 不再是 *gin.Engine")
	}
	return repo, testMux, engine
}

func adminHeader(key string) muxclient.ClientDoOption {
	return muxclient.WithHeader(http.Header{"Authorization": []string{"Bearer " + key}})
}

// statusOf 把接口返回的错误摊成 HTTP 状态码。
func statusOf(t *testing.T, err error) int {
	t.Helper()
	e, ok := err.(*httputils.Error)
	if !ok {
		t.Fatalf("期望 *httputils.Error，拿到 %T（%v）", err, err)
	}
	return e.Status
}

// itemOf 从设置列表里挑出一项。
func itemOf(list []*admin.SettingItem, key string) *admin.SettingItem {
	for _, item := range list {
		if item.Key == key {
			return item
		}
	}
	return nil
}

// TestSettingsSaveAndRead 覆盖任务目标「保存一个运行时设置并读回」。
//
// 顺带是 public_homepage 变成可写的那一刻：它之前只有读的一半，界面上那个开关
// 打不动。
func TestSettingsSaveAndRead(t *testing.T) {
	convey.Convey("写入运行时设置再读回", t, func() {
		// 环境建在 Convey 里：convey 会为每个叶子重跑一遍外层，共用一张内存表
		// 会让后跑的叶子看见前一个叶子写进去的东西。
		repo, testMux, _ := setupSettingTest(t)
		convey.Convey("库里还没写过任何一项时，读出来的是默认值", func() {
			resp := &admin.ListSettingsResponse{}
			convey.So(testMux.Do(context.Background(), &admin.ListSettingsRequest{}, resp,
				adminHeader(initialKey)), convey.ShouldBeNil)
			home := itemOf(resp.List, "public_homepage")
			convey.So(home, convey.ShouldNotBeNil)
			// 默认可见：一台刚装好的镜像站首页不该是空的（任务 12 的默认值）。
			convey.So(string(home.Value), convey.ShouldEqual, "true")
			convey.So(home.Type, convey.ShouldEqual, admin.SettingTypeBool)
			convey.So(itemOf(resp.List, "cache_quota_bytes"), convey.ShouldNotBeNil)
			convey.So(itemOf(resp.List, "mutable_ttl_seconds"), convey.ShouldNotBeNil)
		})

		convey.Convey("写进去的值下一次读得到", func() {
			saveResp := &admin.SaveSettingsResponse{}
			convey.So(testMux.Do(context.Background(), &admin.SaveSettingsRequest{
				Settings: map[string]json.RawMessage{
					"public_homepage":   json.RawMessage(`false`),
					"site_name":         json.RawMessage(`"我的镜像站"`),
					"cache_quota_bytes": json.RawMessage(`5368709120`),
				},
			}, saveResp, adminHeader(initialKey)), convey.ShouldBeNil)

			// 落库的是 JSON 值本身，和这张表其余的行一样（「数据模型」一节）。
			value, ok := repo.value("public_homepage")
			convey.So(ok, convey.ShouldBeTrue)
			convey.So(value, convey.ShouldEqual, "false")

			resp := &admin.ListSettingsResponse{}
			convey.So(testMux.Do(context.Background(), &admin.ListSettingsRequest{}, resp,
				adminHeader(initialKey)), convey.ShouldBeNil)
			convey.So(string(itemOf(resp.List, "public_homepage").Value), convey.ShouldEqual, "false")
			convey.So(string(itemOf(resp.List, "site_name").Value), convey.ShouldEqual, `"我的镜像站"`)
			convey.So(string(itemOf(resp.List, "cache_quota_bytes").Value), convey.ShouldEqual, "5368709120")
			// 没写过的项不受影响，还是默认值。
			convey.So(string(itemOf(resp.List, "cache_reclaim_percent").Value), convey.ShouldEqual, "90")
		})
	})
}

// TestSettingsReadsThroughCorruptRow 库里躺着一行读不懂的值时，设置页要照常打开。
//
// 两个理由：把坏值原样吐出去会让整个响应不是合法 JSON（Value 是原始 JSON 片段），
// 而整页报错看起来像是功能丢了，而不是像一处配置错误——后者才是真相，且从界面上
// 改一次就能修好。这和 PublicHomepage 对同一种坏数据的处理是一致的。
func TestSettingsReadsThroughCorruptRow(t *testing.T) {
	repo, testMux, _ := setupSettingTest(t)
	convey.Convey("某一行的值读不懂时按默认值给出", t, func() {
		convey.So(setting_repo.Setting().Save(context.Background(), &setting_entity.Setting{
			Key: "public_homepage", Value: "yes"}), convey.ShouldBeNil)
		// 确认这一行真的坏在库里，而不是用例自己没写进去。
		value, ok := repo.value("public_homepage")
		convey.So(ok, convey.ShouldBeTrue)
		convey.So(value, convey.ShouldEqual, "yes")

		resp := &admin.ListSettingsResponse{}
		convey.So(testMux.Do(context.Background(), &admin.ListSettingsRequest{}, resp,
			adminHeader(initialKey)), convey.ShouldBeNil)
		convey.So(string(itemOf(resp.List, "public_homepage").Value), convey.ShouldEqual, "true")
	})
}

// TestSettingsHideAdminKeyHash 管理密钥的哈希和运行时设置同住一张表，但它不是
// 一项「设置」：读出来对界面毫无用处，只是多一处泄漏面；从这里写进去更是能绕开
// 轮换端点直接改掉后台凭据。
func TestSettingsHideAdminKeyHash(t *testing.T) {
	repo, testMux, engine := setupSettingTest(t)
	convey.Convey("管理密钥的哈希不在设置读写的范围里", t, func() {
		convey.Convey("读不出来", func() {
			req, err := testMux.Request(context.Background(), &admin.ListSettingsRequest{},
				adminHeader(initialKey))
			convey.So(err, convey.ShouldBeNil)
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)
			convey.So(w.Code, convey.ShouldEqual, http.StatusOK)
			convey.So(w.Body.String(), convey.ShouldNotContainSubstring, setting_svc.AdminKeyHashSetting)
			convey.So(w.Body.String(), convey.ShouldNotContainSubstring, "$2a$")
		})

		convey.Convey("也写不进去", func() {
			before, _ := repo.value(setting_svc.AdminKeyHashSetting)
			err := testMux.Do(context.Background(), &admin.SaveSettingsRequest{
				Settings: map[string]json.RawMessage{
					setting_svc.AdminKeyHashSetting: json.RawMessage(`"planted"`),
				},
			}, &admin.SaveSettingsResponse{}, adminHeader(initialKey))
			convey.So(err, convey.ShouldNotBeNil)
			convey.So(statusOf(t, err), convey.ShouldEqual, http.StatusBadRequest)
			after, _ := repo.value(setting_svc.AdminKeyHashSetting)
			convey.So(after, convey.ShouldEqual, before)
		})
	})
}

// TestSettingsRejectsInvalidValues 不认识的键和超出取值范围的值都要当场 4xx，
// 而不是安静地存进去——存进去之后，界面上看到的是一个「已保存」的非法配置，
// 而进程下一次用到它时才会出问题，那时已经追不回来是哪一次点了保存。
func TestSettingsRejectsInvalidValues(t *testing.T) {
	repo, testMux, _ := setupSettingTest(t)
	convey.Convey("设置写入的校验", t, func() {
		cases := []struct {
			name     string
			settings map[string]json.RawMessage
		}{
			{"不认识的键", map[string]json.RawMessage{"nonexistent_knob": json.RawMessage(`1`)}},
			{"配额为负", map[string]json.RawMessage{"cache_quota_bytes": json.RawMessage(`-1`)}},
			{"回收水位是百分比，不能超过 100", map[string]json.RawMessage{
				"cache_reclaim_percent": json.RawMessage(`150`)}},
			{"回收水位不能为 0", map[string]json.RawMessage{
				"cache_reclaim_percent": json.RawMessage(`0`)}},
			{"TTL 是时长，不能为 0 或负数", map[string]json.RawMessage{
				"mutable_ttl_seconds": json.RawMessage(`0`)}},
			{"回源并发不能为 0", map[string]json.RawMessage{"origin_concurrency": json.RawMessage(`0`)}},
			{"重试次数不能为负", map[string]json.RawMessage{"origin_retries": json.RawMessage(`-1`)}},
			{"类型不对：布尔项给了字符串", map[string]json.RawMessage{
				"public_homepage": json.RawMessage(`"false"`)}},
			{"类型不对：整数项给了小数", map[string]json.RawMessage{
				"mutable_ttl_seconds": json.RawMessage(`1.5`)}},
		}
		for _, c := range cases {
			convey.Convey(c.name+"，返回 4xx", func() {
				err := testMux.Do(context.Background(), &admin.SaveSettingsRequest{Settings: c.settings},
					&admin.SaveSettingsResponse{}, adminHeader(initialKey))
				convey.So(err, convey.ShouldNotBeNil)
				convey.So(statusOf(t, err), convey.ShouldEqual, http.StatusBadRequest)
			})
		}

		convey.Convey("同一次写入里只要有一项不合法，合法的那些也不落库", func() {
			// 否则「保存失败」之后库里躺着的是半套设置，而界面刚刚告诉用户没保存成功。
			err := testMux.Do(context.Background(), &admin.SaveSettingsRequest{
				Settings: map[string]json.RawMessage{
					"site_name":             json.RawMessage(`"写不进去的名字"`),
					"cache_reclaim_percent": json.RawMessage(`150`),
				},
			}, &admin.SaveSettingsResponse{}, adminHeader(initialKey))
			convey.So(err, convey.ShouldNotBeNil)
			_, ok := repo.value("site_name")
			convey.So(ok, convey.ShouldBeFalse)
		})
	})
}

// TestRotateAdminKey 覆盖任务目标「轮换密钥后旧密钥立刻 401 而新密钥可用」。
//
// 用真实请求来证明，而不是去翻库里存了什么：轮换要立刻生效，靠的是校验那一步
// 每次都去读库；实现里但凡加一层进程内缓存，翻库的断言照样绿，而后台其实还认旧密钥。
func TestRotateAdminKey(t *testing.T) {
	const newKey = "rotated-admin-key-0002"
	convey.Convey("轮换管理密钥", t, func() {
		// 环境建在 Convey 里：轮换会改掉这张表，而 convey 为每个叶子都要重跑
		// 一遍这段外层——共用一张表的话，第二个叶子的轮换会拿旧密钥吃 401。
		repo, testMux, _ := setupSettingTest(t)
		convey.So(testMux.Do(context.Background(), &admin.RotateAdminKeyRequest{NewKey: newKey},
			&admin.RotateAdminKeyResponse{}, adminHeader(initialKey)), convey.ShouldBeNil)

		convey.Convey("旧密钥立刻失效", func() {
			err := testMux.Do(context.Background(), &admin.ListSettingsRequest{},
				&admin.ListSettingsResponse{}, adminHeader(initialKey))
			convey.So(err, convey.ShouldNotBeNil)
			convey.So(statusOf(t, err), convey.ShouldEqual, http.StatusUnauthorized)
		})

		convey.Convey("新密钥可用", func() {
			convey.So(testMux.Do(context.Background(), &admin.ListSettingsRequest{},
				&admin.ListSettingsResponse{}, adminHeader(newKey)), convey.ShouldBeNil)
		})

		convey.Convey("库里存的是哈希而不是新密钥本身（决策 18）", func() {
			stored, ok := repo.value(setting_svc.AdminKeyHashSetting)
			convey.So(ok, convey.ShouldBeTrue)
			convey.So(stored, convey.ShouldNotContainSubstring, newKey)
			convey.So(bcrypt.CompareHashAndPassword([]byte(stored), []byte(newKey)), convey.ShouldBeNil)
		})

		convey.Convey("轮换之后，config.yaml 里的初始密钥仍然不会把它覆盖回去（决策 18）", func() {
			// 每次启动都会走一遍 EnsureAdminKey；它若覆盖，轮换等于没发生，
			// 而且旧密钥会在下一次重启时复活。
			convey.So(setting_svc.Setting().EnsureAdminKey(context.Background(), initialKey),
				convey.ShouldBeNil)
			err := testMux.Do(context.Background(), &admin.ListSettingsRequest{},
				&admin.ListSettingsResponse{}, adminHeader(initialKey))
			convey.So(err, convey.ShouldNotBeNil)
			convey.So(statusOf(t, err), convey.ShouldEqual, http.StatusUnauthorized)
		})
	})
}

// TestRotateAdminKeyRejectsWeakKey 密钥是这台镜像站后台的唯一凭据，
// 允许轮换成一个两位数等于把决策 18 的哈希存储做成摆设。
func TestRotateAdminKeyRejectsWeakKey(t *testing.T) {
	repo, testMux, _ := setupSettingTest(t)
	convey.Convey("太短的新密钥被拒绝，且不会改动库里的密钥", t, func() {
		before, _ := repo.value(setting_svc.AdminKeyHashSetting)
		err := testMux.Do(context.Background(), &admin.RotateAdminKeyRequest{NewKey: "short"},
			&admin.RotateAdminKeyResponse{}, adminHeader(initialKey))
		convey.So(err, convey.ShouldNotBeNil)
		convey.So(statusOf(t, err), convey.ShouldEqual, http.StatusBadRequest)
		after, _ := repo.value(setting_svc.AdminKeyHashSetting)
		convey.So(after, convey.ShouldEqual, before)
	})
}

// TestSettingAdminAuth 设置读写与密钥轮换都在 /api/v1/admin 下（决策 10）：
// 没有密钥的调用方一个都不该碰得到，尤其是轮换——那等于把后台直接送出去。
func TestSettingAdminAuth(t *testing.T) {
	_, testMux, engine := setupSettingTest(t)
	convey.Convey("设置端点一律要密钥", t, func() {
		for _, req := range []any{
			&admin.ListSettingsRequest{},
			&admin.SaveSettingsRequest{Settings: map[string]json.RawMessage{
				"public_homepage": json.RawMessage(`false`)}},
			&admin.RotateAdminKeyRequest{NewKey: "attacker-supplied-key"},
		} {
			httpReq, err := testMux.Request(context.Background(), req)
			convey.So(err, convey.ShouldBeNil)
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, httpReq)
			convey.So(w.Code, convey.ShouldEqual, http.StatusUnauthorized)
		}
	})
}
