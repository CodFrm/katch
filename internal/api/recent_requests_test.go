package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/cago-frame/cago/configs"
	"github.com/cago-frame/cago/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/zap"

	"github.com/CodFrm/katch/internal/bootstrap"
	"github.com/CodFrm/katch/internal/metrics"
	"github.com/CodFrm/katch/internal/service/upstream_svc"
)

// 这一组用例守的是「最近请求读的是结构化日志」这句话的两头：拉取路径**真的**把
// 那一行写进了配置里那个文件，管理接口**真的**从那个文件的尾部把它读回来。
//
// 中间没有替身：日志由计数中间件按 cmd/katch/main.go 的装配写出，文件路径由
// configs 里的 logger.logFile 决定，读取走 log_svc 的默认实例。任何一头改了字段名，
// 这组用例当场红——只测读那一半的话，两边各写各的也能全绿。

// startLogKatch 起一套会把日志落到文件上的 katch，返回引擎、假上游和日志文件路径。
func startLogKatch(t *testing.T) (*gin.Engine, *liveOrigin, string) {
	t.Helper()
	engine, origin := startLiveKatch(t)

	dir := t.TempDir()
	logFile := filepath.Join(dir, "katch.log")
	// 配置文件是日志路径的唯一出处，用例也走这条路：直接给 log_svc 塞一个路径的话，
	// 「端点不认调用方给的文件名」这句话就没被任何东西验证过。
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(`env: TEST
debug: false
source: file
logger:
  level: info
  disableConsole: true
  logFile:
    enable: true
    filename: `+logFile+`
`), 0o600); err != nil {
		t.Fatal(err)
	}
	src, err := bootstrap.NewConfigSource(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := configs.NewConfig("katch", configs.WithSource(src)); err != nil {
		t.Fatal(err)
	}
	log, err := logger.New(logger.AppendCore(logger.NewFileCore(zap.InfoLevel, logFile)))
	if err != nil {
		t.Fatal(err)
	}
	logger.SetLogger(log)
	t.Cleanup(func() { logger.SetLogger(zap.NewNop()) })

	// 计数中间件照 main 装配：拉取那一行就是它写的，和指标出自同一个缝。
	engine.Use(metrics.Default().Middleware(metrics.Hooks{
		Lookup: func(ctx context.Context, host string) bool {
			upstream, err := upstream_svc.Upstream().FindByHost(ctx, host)
			return err == nil && upstream != nil
		},
	}))
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

func TestRecentRequests_ComesFromTheRequestLog(t *testing.T) {
	convey.Convey("带密钥读某个上游的最近请求", t, func() {
		engine, _, _ := startLogKatch(t)
		id := liveUpstreamID(t, engine)

		convey.So(livePull(engine, "/pool/recent.deb").Code, convey.ShouldEqual, http.StatusOK)
		convey.So(livePull(engine, "/pool/recent.deb").Code, convey.ShouldEqual, http.StatusOK)

		list := recentRequests(t, engine, "upstream_id="+itoa(id))

		convey.Convey("两次拉取都在，最近的在最前，时间/对象/结果/大小/耗时都有", func() {
			convey.So(len(list), convey.ShouldEqual, 2)
			convey.So(list[0].Object, convey.ShouldEqual, "/pool/recent.deb")
			// 第二次由缓存服务，第一次回源取回——这块面板的用处就在这个差别上。
			convey.So(list[0].Result, convey.ShouldEqual, "hit")
			convey.So(list[1].Result, convey.ShouldEqual, "miss")
			convey.So(list[0].Bytes, convey.ShouldEqual, liveObjectSize)
			convey.So(list[0].At, convey.ShouldBeGreaterThan, 0)
			convey.So(list[0].DurationMS, convey.ShouldBeGreaterThanOrEqualTo, 0)
		})

		convey.Convey("表里没有这个上游时给空，而不是把别人的行端出来", func() {
			convey.So(livePull(engine, "/pool/recent.deb").Code, convey.ShouldEqual, http.StatusOK)
			other := recentRequests(t, engine, "upstream_id=999999")
			convey.So(other, convey.ShouldBeEmpty)
		})
	})
}

func TestRecentRequests_TakesNoFilenameFromCaller(t *testing.T) {
	convey.Convey("端点不接受调用方给的文件名", t, func() {
		engine, _, _ := startLogKatch(t)
		id := liveUpstreamID(t, engine)
		convey.So(livePull(engine, "/pool/recent.deb").Code, convey.ShouldEqual, http.StatusOK)

		plain := recentRequests(t, engine, "upstream_id="+itoa(id))
		// 挑几个「像是能指定文件」的参数名一起带上：任何一个被接住，管理接口就成了
		// 一条任意文件读取的路。
		crafted := recentRequests(t, engine, "upstream_id="+itoa(id)+
			"&file=/etc/passwd&filename=/etc/passwd&path=../../../../etc/passwd&log=/etc/passwd")

		convey.So(crafted, convey.ShouldResemble, plain)
		convey.So(len(crafted), convey.ShouldEqual, 1)
		convey.So(crafted[0].Object, convey.ShouldEqual, "/pool/recent.deb")
	})
}

func TestRecentRequests_DegradesToAbsentWhenLogIsGone(t *testing.T) {
	convey.Convey("日志被轮转走之后这一块是空的，而不是一个错误", t, func() {
		engine, _, logFile := startLogKatch(t)
		id := liveUpstreamID(t, engine)
		convey.So(livePull(engine, "/pool/recent.deb").Code, convey.ShouldEqual, http.StatusOK)
		convey.So(len(recentRequests(t, engine, "upstream_id="+itoa(id))), convey.ShouldEqual, 1)

		if err := os.Remove(logFile); err != nil {
			t.Fatal(err)
		}

		w := liveAdmin(engine, http.MethodGet,
			"/api/v1/admin/logs/requests?upstream_id="+itoa(id), "")
		convey.So(w.Code, convey.ShouldEqual, http.StatusOK)
		convey.So(recentRequests(t, engine, "upstream_id="+itoa(id)), convey.ShouldBeEmpty)
	})
}
