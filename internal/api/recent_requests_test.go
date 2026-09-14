package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cago-frame/cago/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/zap"

	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/metrics"
	"github.com/CodFrm/katch/internal/model/entity/request_log_entity"
	"github.com/CodFrm/katch/internal/repository/request_log_repo"
	"github.com/CodFrm/katch/internal/service/request_svc"
	"github.com/CodFrm/katch/internal/service/upstream_svc"
)

// 这一组用例守的是「最近请求读的是库」这句话的两头：拉取路径**真的**把那一行
// 经中间件与落库服务写进 recent_request，管理接口**真的**把它从库里读回来
// （TestRecentRequests_ComesFromThePullPath 是那条端到端的）。日志文件是空的
// 且没有人在里面写拉取行，所以一个还去读日志尾部的实现在这里读不到任何东西——
// 两边各写各的也能全绿的那种只测读一半的写法，被这几条夹住了。

// startRecentRequestKatch 起一套「最近请求」落在真库上的 katch，返回引擎、假上游和
// 一个空日志文件。
func startRecentRequestKatch(t *testing.T) (*gin.Engine, *liveOrigin, string) {
	t.Helper()
	engine, origin := startLiveKatch(t)
	// 仓储与业务层照 cmd/katch/main.go 装配，用的是真 sqlite：拉取那一行经
	// 中间件进进程内缓冲、经落库服务写进库，再由接口读回来。
	request_log_repo.RegisterRequestLog(request_log_repo.NewRequestLog())
	request_svc.Register(request_svc.New(request_svc.Options{}))
	// 计数中间件也照 main 装配：那一行明细就是它攒进缓冲的。
	engine.Use(metrics.Default().Middleware(metrics.Hooks{
		Lookup: func(ctx context.Context, host string) bool {
			upstream, err := upstream_svc.Upstream().FindByHost(ctx, host)
			return err == nil && upstream != nil
		},
	}))

	// 日志落到一个空文件上：这块面板的数据若还来自日志尾部，写进库的那一行就
	// 读不回来。文件从头到尾没被写过（只有 ComesFromThePullPath 真拉取，那一条
	// 用的也是自己那个文件），所以去读日志尾部的实现在这里只会给出空。
	logFile := filepath.Join(t.TempDir(), "katch.log")
	if err := os.WriteFile(logFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	log, err := logger.New(logger.AppendCore(logger.NewFileCore(zap.DebugLevel, logFile)))
	if err != nil {
		t.Fatal(err)
	}
	logger.SetLogger(log)
	t.Cleanup(func() { logger.SetLogger(zap.NewNop()) })

	registerLiveUpstream(t, engine, origin)
	return engine, origin, logFile
}

// liveUpstreamID 问管理接口那条假上游的 id。
func liveUpstreamID(t *testing.T, engine *gin.Engine) int64 {
	t.Helper()
	w := liveAdmin(engine, http.MethodGet, "/api/v1/admin/upstreams", "")
	var resp struct {
		Data struct {
			List []struct {
				ID   int64  `json:"id"`
				Host string `json:"host"`
			} `json:"list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析上游列表失败：%v", err)
	}
	for _, item := range resp.Data.List {
		if item.Host == liveUpstreamHost {
			return item.ID
		}
	}
	t.Fatalf("上游列表里没有 %s：%s", liveUpstreamHost, w.Body.String())
	return 0
}

// itoa 上游 id 拼进查询串。
func itoa(id int64) string {
	return strconv.FormatInt(id, 10)
}

type recentRequest struct {
	At         int64  `json:"at"`
	Object     string `json:"object"`
	Result     string `json:"result"`
	Bytes      int64  `json:"bytes"`
	DurationMS int64  `json:"duration_ms"`
}

// recentRequests 带密钥读某个上游的最近请求。
func recentRequests(t *testing.T, engine *gin.Engine, query string) []recentRequest {
	t.Helper()
	w := liveAdmin(engine, http.MethodGet, "/api/v1/admin/logs/requests?"+query, "")
	if w.Code != http.StatusOK {
		t.Fatalf("读最近请求失败：%d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Code int `json:"code"`
		Data struct {
			List []recentRequest `json:"list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析最近请求失败：%v", err)
	}
	if resp.Code != 0 {
		t.Fatalf("最近请求返回了错误：%s", w.Body.String())
	}
	return resp.Data.List
}

// recentDBRow 一行待入库的明细。
func recentDBRow(upstreamID, at int64, object, result string, bytes, durationMS int64) *request_log_entity.RecentRequest {
	return &request_log_entity.RecentRequest{
		UpstreamID: upstreamID,
		At:         at,
		Object:     object,
		Result:     result,
		Bytes:      bytes,
		DurationMS: durationMS,
	}
}

// saveRecentRows 直接把明细写进真实库。
func saveRecentRows(t *testing.T, rows ...*request_log_entity.RecentRequest) {
	t.Helper()
	if err := request_log_repo.RecentRequestLog().Save(context.Background(), rows); err != nil {
		t.Fatalf("写入最近请求失败：%v", err)
	}
}

func TestRecentRequests_ComesFromTheDatabase(t *testing.T) {
	convey.Convey("面板的数据来自库", t, func() {
		engine, _, _ := startRecentRequestKatch(t)
		id := liveUpstreamID(t, engine)

		saveRecentRows(t,
			recentDBRow(id, 1700000100, "/pool/recent.deb", "miss", 4096, 120),
			recentDBRow(id, 1700000200, "/dists/bookworm/InRelease", "hit", 512, 4),
			// 同一秒的第二条：顺序靠自增 id 兜底，写入顺序倒过来才是它的先后。
			recentDBRow(id, 1700000200, "/pool/same-second.deb", "hit", 8, 1),
		)

		list := recentRequests(t, engine, "upstream_id="+itoa(id))

		convey.Convey("最近的在最前，同一秒内按写入顺序倒序，字段都在", func() {
			convey.So(len(list), convey.ShouldEqual, 3)
			convey.So(list[0].Object, convey.ShouldEqual, "/pool/same-second.deb")
			convey.So(list[0].At, convey.ShouldEqual, int64(1700000200))
			convey.So(list[1].Object, convey.ShouldEqual, "/dists/bookworm/InRelease")
			convey.So(list[1].Result, convey.ShouldEqual, "hit")
			convey.So(list[1].Bytes, convey.ShouldEqual, int64(512))
			convey.So(list[1].DurationMS, convey.ShouldEqual, int64(4))
			convey.So(list[2].Object, convey.ShouldEqual, "/pool/recent.deb")
			convey.So(list[2].Result, convey.ShouldEqual, "miss")
			convey.So(list[2].Bytes, convey.ShouldEqual, int64(4096))
			convey.So(list[2].DurationMS, convey.ShouldEqual, int64(120))
		})

		convey.Convey("表里没有这个上游时给空，而不是把别人的行端出来", func() {
			convey.So(recentRequests(t, engine, "upstream_id=999999"), convey.ShouldBeEmpty)
		})
	})
}

// TestRecentRequests_ComesFromThePullPath 拉取路径到面板这一整条链路。
//
// 上面那条从仓储起、这里从拉取起：中间件把明细攒进进程内缓冲，落库服务取走
// 一批批量写进库，面板再按 upstream_id 读回来。中间任何一段的字段名或装配改了，
// 这里红。
func TestRecentRequests_ComesFromThePullPath(t *testing.T) {
	convey.Convey("一次拉取经中间件与落库服务真的进了库", t, func() {
		engine, _, _ := startRecentRequestKatch(t)
		id := liveUpstreamID(t, engine)

		// 两次拉取：第一次回源（miss），第二次由缓存服务（hit）——这块面板的
		// 用处就在这个差别上。
		convey.So(livePull(engine, "/pool/recent.deb").Code, convey.ShouldEqual, http.StatusOK)
		convey.So(livePull(engine, "/pool/recent.deb").Code, convey.ShouldEqual, http.StatusOK)
		// 落库是后台每秒一趟的事，用例自己推一趟，免得等墙钟。
		convey.So(request_svc.Request().Flush(context.Background()), convey.ShouldBeNil)

		list := recentRequests(t, engine, "upstream_id="+itoa(id))
		convey.So(list, convey.ShouldHaveLength, 2)
		convey.So(list[0].Object, convey.ShouldEqual, "/pool/recent.deb")
		convey.So(list[0].Result, convey.ShouldEqual, "hit")
		convey.So(list[0].Bytes, convey.ShouldEqual, liveObjectSize)
		convey.So(list[1].Result, convey.ShouldEqual, "miss")
	})
}

func TestRecentRequests_IgnoresTheLog(t *testing.T) {
	convey.Convey("日志里没有拉取行时接口照常给数据", t, func() {
		engine, _, logFile := startRecentRequestKatch(t)
		id := liveUpstreamID(t, engine)

		// 这条用例自己不拉取，所以这个文件连一行拉取都没有：还去读日志尾部的
		// 实现只能给出空，而写进库的那一行照样读得回来。
		content, err := os.ReadFile(logFile)
		convey.So(err, convey.ShouldBeNil)
		convey.So(strings.Contains(string(content), metrics.PullLogMessage), convey.ShouldBeFalse)

		saveRecentRows(t, recentDBRow(id, 1700000200, "/version", "hit", 12, 3))

		list := recentRequests(t, engine, "upstream_id="+itoa(id))
		convey.So(len(list), convey.ShouldEqual, 1)
		convey.So(list[0].Object, convey.ShouldEqual, "/version")
	})
}

func TestRecentRequests_EmptyAndLimitCeiling(t *testing.T) {
	convey.Convey("无数据给空列表，limit 上限仍是 100", t, func() {
		engine, _, _ := startRecentRequestKatch(t)
		id := liveUpstreamID(t, engine)

		convey.Convey("库里没有行时给空列表", func() {
			w := liveAdmin(engine, http.MethodGet,
				"/api/v1/admin/logs/requests?upstream_id="+itoa(id), "")
			convey.So(w.Code, convey.ShouldEqual, http.StatusOK)
			// 空也得是 []，不是 null：调用方不该多一种情况要处理。
			convey.So(w.Body.String(), convey.ShouldContainSubstring, `"list":[]`)
		})

		// 放进超过上限的行数：limit=100 只能给出最近的那 100 条。
		rows := make([]*request_log_entity.RecentRequest, 0, 150)
		for i := 0; i < 150; i++ {
			rows = append(rows, recentDBRow(id, 1700000000+int64(i),
				fmt.Sprintf("/pool/p-%03d.deb", i), "hit", 1, 1))
		}
		saveRecentRows(t, rows...)

		convey.Convey("limit=100 给出最近 100 条", func() {
			list := recentRequests(t, engine, "upstream_id="+itoa(id)+"&limit=100")
			convey.So(len(list), convey.ShouldEqual, admin.RecentRequestsMaxLimit)
			convey.So(list[0].Object, convey.ShouldEqual, "/pool/p-149.deb")
			convey.So(list[len(list)-1].Object, convey.ShouldEqual, "/pool/p-050.deb")
		})

		convey.Convey("超过 100 的 limit 在请求校验那一关被挡掉", func() {
			w := liveAdmin(engine, http.MethodGet,
				"/api/v1/admin/logs/requests?upstream_id="+itoa(id)+"&limit=101", "")
			convey.So(w.Code, convey.ShouldEqual, http.StatusBadRequest)
		})
	})
}
