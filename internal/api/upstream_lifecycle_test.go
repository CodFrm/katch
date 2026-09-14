// 用例放在 _test 包里，理由同 runtime_settings_test.go：api 依赖 service 与
// internal/proxy 下的包，同包会构成导入环。这一组复用那边的活体装配
// （startLiveKatch / liveAdmin / livePullHandler）——判据同样是「同一个跑着的
// engine，写入与观察之间没有第二次装配」。
package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/smartystreets/goconvey/convey"

	"github.com/CodFrm/katch/internal/model/entity/event_entity"
	"github.com/CodFrm/katch/internal/repository/event_repo"
)

// 这一组守的是决策 3 那句话的另一半：上游落库的理由是「增删上游、暂停上游是运维
// 日常动作」，而日常动作的判据是**改完立刻生效、不用重启**（决策 4）。创建那一半
// 已经有 extension_test 与 smoke 守着，这里守的是创建之后的改与停。
//
// 停用尤其不能只拦回源：白名单（决策 6）说的是「不在表内或已停用的主机返回 404」，
// 一个只挡住回源的闸会继续把盘上已有的副本发出去——那条上游看起来还活着，而运维
// 以为自己已经把它停了。

// lifecycleHost 这一组用例的假上游主机名，和 runtime_settings 那条分开，
// 免得两边的缓存记录与事件互相看见。
const lifecycleHost = "packages.upstream-lifecycle.invalid"

// lifecycleUpstream 管理接口给出的一条上游里这几条用例用得上的部分。
type lifecycleUpstream struct {
	ID                int64    `json:"id"`
	Host              string   `json:"host"`
	Kind              string   `json:"kind"`
	Origin            string   `json:"origin"`
	Enabled           bool     `json:"enabled"`
	ImmutablePatterns []string `json:"immutable_patterns"`
	MutableTTLSeconds int      `json:"mutable_ttl_seconds"`
}

// upstreamBody 一条上游的请求体。整条写回去：更新是替换而不是打补丁。
func upstreamBody(origin string, enabled bool, ttl int) string {
	return `{
		"host": "` + lifecycleHost + `",
		"kind": "static",
		"origin": "` + origin + `",
		"enabled": ` + boolLiteral(enabled) + `,
		"immutable_patterns": ["/pool/"],
		"mutable_ttl_seconds": ` + strconv.Itoa(ttl) + `
	}`
}

func boolLiteral(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

// createLifecycleUpstream 经管理接口登记那条上游，返回它的 id。
func createLifecycleUpstream(t *testing.T, engine *gin.Engine, origin string, ttl int) int64 {
	t.Helper()
	w := liveAdmin(engine, http.MethodPost, "/api/v1/admin/upstreams", upstreamBody(origin, true, ttl))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"code":0`) {
		t.Fatalf("登记上游失败：%d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			ID int64 `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析登记响应失败：%v", err)
	}
	if resp.Data.ID == 0 {
		t.Fatalf("登记上游没有返回 id：%s", w.Body.String())
	}
	return resp.Data.ID
}

// putLifecycleUpstream 整条改写那条上游，返回这次请求的响应。
func putLifecycleUpstream(engine *gin.Engine, id int64, body string) *httptest.ResponseRecorder {
	return liveAdmin(engine, http.MethodPut, "/api/v1/admin/upstreams/"+strconv.FormatInt(id, 10), body)
}

// lifecycleUpstreamOf 从管理接口读回那条上游此刻的登记信息。
func lifecycleUpstreamOf(t *testing.T, engine *gin.Engine) *lifecycleUpstream {
	t.Helper()
	w := liveAdmin(engine, http.MethodGet, "/api/v1/admin/upstreams", "")
	if w.Code != http.StatusOK {
		t.Fatalf("读上游列表失败：%d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			List []*lifecycleUpstream `json:"list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析上游列表失败：%v", err)
	}
	for _, item := range resp.Data.List {
		if item.Host == lifecycleHost {
			return item
		}
	}
	return nil
}

// lifecyclePull 拉一个对象。
func lifecyclePull(engine *gin.Engine, path string) *httptest.ResponseRecorder {
	return liveCall(engine, http.MethodGet, "/"+lifecycleHost+path, "", nil)
}

// echoesHost 这次响应有没有把主机名说出去——响应体和任何一个响应头里都不许有。
func echoesHost(w *httptest.ResponseRecorder) bool {
	if strings.Contains(w.Body.String(), lifecycleHost) {
		return true
	}
	for _, values := range w.Header() {
		for _, v := range values {
			if strings.Contains(v, lifecycleHost) {
				return true
			}
		}
	}
	return false
}

// TestUpstreamUpdate_TakesEffectOnNextPull 目标 (a)：在跑着的进程上改一条已存在
// 上游的 origin 与可变对象 TTL，读回是新值，且下一次拉取就按新值走。
func TestUpstreamUpdate_TakesEffectOnNextPull(t *testing.T) {
	convey.Convey("经管理接口改一条已存在的上游，下一次拉取就打到新源站", t, func() {
		engine, first := startLiveKatch(t)
		second := newLiveOrigin(t)
		id := createLifecycleUpstream(t, engine, first.srv.URL, 300)

		// ① 改之前：回源打的是第一个源站。
		convey.So(lifecyclePull(engine, "/pool/a.deb").Code, convey.ShouldEqual, http.StatusOK)
		convey.So(first.attemptsOf("/pool/a.deb"), convey.ShouldEqual, 1)

		// ② 同一个进程上把 origin 换成第二个源站，TTL 从 300 改成 60。
		w := putLifecycleUpstream(engine, id, upstreamBody(second.srv.URL, true, 60))
		convey.So(w.Code, convey.ShouldEqual, http.StatusOK)
		convey.So(w.Body.String(), convey.ShouldContainSubstring, `"code":0`)

		// ③ 读回来的就是新值。
		stored := lifecycleUpstreamOf(t, engine)
		convey.So(stored, convey.ShouldNotBeNil)
		convey.So(stored.ID, convey.ShouldEqual, id)
		convey.So(stored.Origin, convey.ShouldEqual, second.srv.URL)
		convey.So(stored.MutableTTLSeconds, convey.ShouldEqual, 60)

		// ④ 不重启：下一次回源打的是第二个源站，且可变对象按新 TTL 过期。
		convey.So(lifecyclePull(engine, "/dists/InRelease").Code, convey.ShouldEqual, http.StatusOK)
		convey.So(second.attemptsOf("/dists/InRelease"), convey.ShouldEqual, 1)
		convey.So(first.attemptsOf("/dists/InRelease"), convey.ShouldEqual, 0)
		var object *liveCacheObject
		eventually(t, "可变对象落库", func() bool {
			object = cachedObjectOf(t, engine, "/dists/InRelease")
			return object != nil
		})
		convey.So(object.ExpiresAt-time.Now().Unix(), convey.ShouldBeBetweenOrEqual, 50, 60)
	})
}

// TestUpstreamDisable_StopsServingCachedObjects 目标 (b)：停用之后，已经躺在缓存里
// 的对象也必须 404。
//
// 这一条是本任务最容易做成半对的地方：只在回源那一步加闸的实现照样会把盘上的副本
// 发出去，于是「停用」在有缓存的路径上完全没有效果，而这正是运维最指望它生效的时候。
func TestUpstreamDisable_StopsServingCachedObjects(t *testing.T) {
	convey.Convey("停用一条上游之后，未缓存与已缓存的对象都 404 且不回显主机名", t, func() {
		engine, origin := startLiveKatch(t)
		id := createLifecycleUpstream(t, engine, origin.srv.URL, 300)

		// ① 先让一个对象实实在在地进缓存：第二次拉必须是 HIT，否则后面那条
		//    「已缓存也要 404」就不成立。
		convey.So(lifecyclePull(engine, "/pool/cached.deb").Code, convey.ShouldEqual, http.StatusOK)
		var hit *httptest.ResponseRecorder
		eventually(t, "第二次拉命中缓存", func() bool {
			hit = lifecyclePull(engine, "/pool/cached.deb")
			return hit.Header().Get("X-Katch-Cache") == "HIT"
		})
		convey.So(origin.attemptsOf("/pool/cached.deb"), convey.ShouldEqual, 1)

		// ② 停用：整条写回去，只把 enabled 翻成 false。
		w := putLifecycleUpstream(engine, id, upstreamBody(origin.srv.URL, false, 300))
		convey.So(w.Code, convey.ShouldEqual, http.StatusOK)
		convey.So(lifecycleUpstreamOf(t, engine).Enabled, convey.ShouldBeFalse)

		// ③ 已经在缓存里的那个对象：404，且源站一次都没被再打（它不是回源失败，
		//    是这条上游整个不在白名单里了）。
		cached := lifecyclePull(engine, "/pool/cached.deb")
		convey.So(cached.Code, convey.ShouldEqual, http.StatusNotFound)
		convey.So(cached.Body.String(), convey.ShouldBeBlank)
		convey.So(echoesHost(cached), convey.ShouldBeFalse)
		convey.So(origin.attemptsOf("/pool/cached.deb"), convey.ShouldEqual, 1)

		// ④ 没缓存过的对象同样 404，且两者的回应必须没有可观察的差别。
		missing := lifecyclePull(engine, "/pool/never.deb")
		convey.So(missing.Code, convey.ShouldEqual, http.StatusNotFound)
		convey.So(missing.Body.String(), convey.ShouldBeBlank)
		convey.So(echoesHost(missing), convey.ShouldBeFalse)
		convey.So(origin.attemptsOf("/pool/never.deb"), convey.ShouldEqual, 0)
	})
}

// TestUpstreamReenable_RestoresServing 目标 (c)：停用是可逆的，重新启用之后恢复 200。
func TestUpstreamReenable_RestoresServing(t *testing.T) {
	convey.Convey("重新启用一条停掉的上游，下一次拉取就恢复", t, func() {
		engine, origin := startLiveKatch(t)
		id := createLifecycleUpstream(t, engine, origin.srv.URL, 300)
		convey.So(lifecyclePull(engine, "/pool/a.deb").Code, convey.ShouldEqual, http.StatusOK)

		convey.So(putLifecycleUpstream(engine, id, upstreamBody(origin.srv.URL, false, 300)).Code,
			convey.ShouldEqual, http.StatusOK)
		convey.So(lifecyclePull(engine, "/pool/a.deb").Code, convey.ShouldEqual, http.StatusNotFound)

		convey.So(putLifecycleUpstream(engine, id, upstreamBody(origin.srv.URL, true, 300)).Code,
			convey.ShouldEqual, http.StatusOK)
		convey.So(lifecyclePull(engine, "/pool/a.deb").Code, convey.ShouldEqual, http.StatusOK)
		convey.So(lifecyclePull(engine, "/pool/b.deb").Code, convey.ShouldEqual, http.StatusOK)
	})
}

// TestUpstreamChanges_RecordEvents 目标 (d)：改与停各落一条带操作人的事件。
//
// 「某个上游是谁在什么时候停掉的」是「拉取突然全挂了」之后第一个要查的东西，
// 而那时界面上只剩下现在的状态。
func TestUpstreamChanges_RecordEvents(t *testing.T) {
	convey.Convey("改一条上游和停用一条上游各落一条带操作人的事件", t, func() {
		engine, origin := startLiveKatch(t)
		// 事件仓储在 startLiveKatch 里没装（那几条用例不看事件），这里补上：
		// 没有它 event_svc 会把事件丢掉并只留一条日志。
		event_repo.RegisterEvent(event_repo.NewEvent())
		t.Cleanup(func() { event_repo.RegisterEvent(nil) })
		id := createLifecycleUpstream(t, engine, origin.srv.URL, 300)

		second := newLiveOrigin(t)
		convey.So(putLifecycleUpstream(engine, id, upstreamBody(second.srv.URL, true, 60)).Code,
			convey.ShouldEqual, http.StatusOK)
		convey.So(putLifecycleUpstream(engine, id, upstreamBody(second.srv.URL, false, 60)).Code,
			convey.ShouldEqual, http.StatusOK)

		w := liveAdmin(engine, http.MethodGet, "/api/v1/admin/events", "")
		convey.So(w.Code, convey.ShouldEqual, http.StatusOK)
		var resp struct {
			Data struct {
				List []struct {
					Kind       string          `json:"kind"`
					Actor      string          `json:"actor"`
					UpstreamID int64           `json:"upstream_id"`
					Detail     json.RawMessage `json:"detail"`
				} `json:"list"`
			} `json:"data"`
		}
		convey.So(json.Unmarshal(w.Body.Bytes(), &resp), convey.ShouldBeNil)
		// 新的在前：停用、改、创建。
		convey.So(len(resp.Data.List), convey.ShouldEqual, 3)
		paused, updated, created := resp.Data.List[0], resp.Data.List[1], resp.Data.List[2]
		convey.So(created.Kind, convey.ShouldEqual, event_entity.KindUpstreamCreated)
		convey.So(updated.Kind, convey.ShouldEqual, event_entity.KindUpstreamUpdated)
		convey.So(paused.Kind, convey.ShouldEqual, event_entity.KindUpstreamUpdated)
		for _, event := range []string{paused.Actor, updated.Actor} {
			convey.So(event, convey.ShouldEqual, event_entity.ActorAdmin)
		}
		convey.So(updated.UpstreamID, convey.ShouldEqual, id)
		convey.So(paused.UpstreamID, convey.ShouldEqual, id)
		// 细节里要认得出「改成了什么」：新源站，以及停用这一下。
		convey.So(string(updated.Detail), convey.ShouldContainSubstring, second.srv.URL)
		convey.So(string(paused.Detail), convey.ShouldContainSubstring, `"enabled":false`)
	})
}
