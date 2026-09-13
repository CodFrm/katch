package log_svc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"

	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/metrics"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
	mock_upstream_repo "github.com/CodFrm/katch/internal/repository/upstream_repo/mock"
)

// 这一层要验的是「怎么从一份结构化日志的尾部读出最近请求」，所以日志文件是真的，
// 上游表用 mock 注入——它在这里只回答「这个 id 是哪台主机」。

const (
	testHost      = "deb.debian.org"
	testUpstreamI = int64(7)
)

// writeLog 把若干行写成一个日志文件，返回文件路径。
func writeLog(t *testing.T, lines ...string) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "katch.log")
	if err := os.WriteFile(name, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}

// pullLogLine 一条拉取日志，字段名与 metrics 那一侧写出来的一致。
func pullLogLine(at int64, host, object, result string, bytes, durationMS int64) string {
	line, err := json.Marshal(map[string]any{
		"level":       "info",
		"ts":          "2023-11-14T22:13:20.000+0800",
		"caller":      "metrics/metrics.go:1",
		"msg":         metrics.PullLogMessage,
		"at":          at,
		"upstream":    host,
		"object":      object,
		"result":      result,
		"bytes":       bytes,
		"duration_ms": durationMS,
	})
	if err != nil {
		panic(err)
	}
	return string(line)
}

// otherLine 一条与拉取无关的日志，长度和拉取行同量级——它是填充物。
func otherLine(i int) string {
	line, _ := json.Marshal(map[string]any{
		"level": "info",
		"ts":    "2023-11-14T22:13:20.000+0800",
		"msg":   "缓存副本校验失败",
		"key":   fmt.Sprintf("deb.debian.org/pool/filler-%08d.deb", i),
		"err":   "这是一条与拉取无关的日志，用来把文件撑大。",
	})
	return string(line)
}

func setupLog(t *testing.T, filename string, upstream *upstream_entity.Upstream) LogSvc {
	t.Helper()
	ctrl := gomock.NewController(t)
	repo := mock_upstream_repo.NewMockUpstreamRepo(ctrl)
	repo.EXPECT().Find(gomock.Any(), testUpstreamI).Return(upstream, nil).AnyTimes()
	upstream_repo.RegisterUpstream(repo)
	return New(Options{Filename: filename})
}

func testUpstream() *upstream_entity.Upstream {
	return &upstream_entity.Upstream{ID: testUpstreamI, Host: testHost}
}

func TestRecentRequests_ReadsTailOfStructuredLog(t *testing.T) {
	convey.Convey("带密钥读某个上游的最近请求", t, func() {
		name := writeLog(t,
			otherLine(1),
			pullLogLine(1700000000, "proxy.golang.org", "/@v/list", "hit", 12, 3),
			pullLogLine(1700000100, testHost, "/pool/main/a/apt_2.7.deb", "miss", 4096, 120),
			"{这行不是 JSON",
			pullLogLine(1700000200, testHost, "/dists/bookworm/InRelease", "hit", 512, 4),
		)
		svc := setupLog(t, name, testUpstream())

		resp, err := svc.RecentRequests(context.Background(), &admin.RecentRequestsRequest{
			UpstreamID: testUpstreamI,
		})
		convey.So(err, convey.ShouldBeNil)

		convey.Convey("只给这个上游的行，最近的在最前，时间/对象/结果/大小/耗时都在", func() {
			convey.So(len(resp.List), convey.ShouldEqual, 2)
			convey.So(resp.List[0].At, convey.ShouldEqual, 1700000200)
			convey.So(resp.List[0].Object, convey.ShouldEqual, "/dists/bookworm/InRelease")
			convey.So(resp.List[0].Result, convey.ShouldEqual, "hit")
			convey.So(resp.List[0].Bytes, convey.ShouldEqual, 512)
			convey.So(resp.List[0].DurationMS, convey.ShouldEqual, 4)
			convey.So(resp.List[1].Object, convey.ShouldEqual, "/pool/main/a/apt_2.7.deb")
			convey.So(resp.List[1].Result, convey.ShouldEqual, "miss")
			convey.So(resp.List[1].Bytes, convey.ShouldEqual, 4096)
			convey.So(resp.List[1].DurationMS, convey.ShouldEqual, 120)
		})
	})
}

func TestRecentRequests_OnlyReadsTheTail(t *testing.T) {
	convey.Convey("日志远大于响应时只读尾部", t, func() {
		// 文件头上躺着一条**匹配的**拉取行，后面堆几兆填充物，尾部再放两条。
		// 把整份日志读进内存的实现会连头上那条一起给出来——这是唯一能把两种实现
		// 分开的判据，「返回条数对」是分不开的。
		lines := []string{pullLogLine(1600000000, testHost, "/pool/main/古董.deb", "hit", 1, 1)}
		for i := 0; i < 25000; i++ {
			lines = append(lines, otherLine(i))
		}
		lines = append(lines,
			pullLogLine(1700000100, testHost, "/pool/main/a/apt_2.7.deb", "miss", 4096, 120),
			pullLogLine(1700000200, testHost, "/dists/bookworm/InRelease", "hit", 512, 4),
		)
		name := writeLog(t, lines...)
		info, err := os.Stat(name)
		convey.So(err, convey.ShouldBeNil)
		convey.So(info.Size(), convey.ShouldBeGreaterThan, 4<<20)

		svc := setupLog(t, name, testUpstream())
		resp, err := svc.RecentRequests(context.Background(), &admin.RecentRequestsRequest{
			UpstreamID: testUpstreamI,
			// 上限远大于尾部窗口里的匹配行数：条数不构成排除理由。
			Limit: admin.RecentRequestsMaxLimit,
		})
		convey.So(err, convey.ShouldBeNil)

		convey.Convey("窗口之外的那条不在结果里", func() {
			convey.So(len(resp.List), convey.ShouldEqual, 2)
			for _, item := range resp.List {
				convey.So(item.Object, convey.ShouldNotEqual, "/pool/main/古董.deb")
			}
		})
	})
}

func TestRecentRequests_HasHardCeiling(t *testing.T) {
	convey.Convey("条数有硬上限", t, func() {
		lines := make([]string, 0, 400)
		for i := 0; i < 400; i++ {
			lines = append(lines, pullLogLine(1700000000+int64(i), testHost,
				fmt.Sprintf("/pool/main/p-%03d.deb", i), "hit", 10, 2))
		}
		name := writeLog(t, lines...)
		svc := setupLog(t, name, testUpstream())

		resp, err := svc.RecentRequests(context.Background(), &admin.RecentRequestsRequest{
			UpstreamID: testUpstreamI,
			// 接口层的 binding 已经挡过一次，服务层被直接调用时也不能被撬开。
			Limit: 100000,
		})
		convey.So(err, convey.ShouldBeNil)
		convey.So(len(resp.List), convey.ShouldEqual, admin.RecentRequestsMaxLimit)
		convey.So(resp.List[0].Object, convey.ShouldEqual, "/pool/main/p-399.deb")
	})
}

func TestRecentRequests_DegradesToAbsent(t *testing.T) {
	convey.Convey("日志读不到时这一块是空的，而不是一个错误", t, func() {
		convey.Convey("日志文件不存在", func() {
			svc := setupLog(t, filepath.Join(t.TempDir(), "没有这个文件.log"), testUpstream())
			resp, err := svc.RecentRequests(context.Background(), &admin.RecentRequestsRequest{
				UpstreamID: testUpstreamI,
			})
			convey.So(err, convey.ShouldBeNil)
			convey.So(resp.List, convey.ShouldBeEmpty)
		})

		convey.Convey("路径读不出内容（比如配成了一个目录）", func() {
			svc := setupLog(t, t.TempDir(), testUpstream())
			resp, err := svc.RecentRequests(context.Background(), &admin.RecentRequestsRequest{
				UpstreamID: testUpstreamI,
			})
			convey.So(err, convey.ShouldBeNil)
			convey.So(resp.List, convey.ShouldBeEmpty)
		})

		convey.Convey("配置里读不到日志文件路径", func() {
			svc := setupLog(t, "", testUpstream())
			resp, err := svc.RecentRequests(context.Background(), &admin.RecentRequestsRequest{
				UpstreamID: testUpstreamI,
			})
			convey.So(err, convey.ShouldBeNil)
			convey.So(resp.List, convey.ShouldBeEmpty)
		})

		convey.Convey("上游已经被删掉了", func() {
			name := writeLog(t, pullLogLine(1700000200, testHost, "/x", "hit", 1, 1))
			svc := setupLog(t, name, nil)
			resp, err := svc.RecentRequests(context.Background(), &admin.RecentRequestsRequest{
				UpstreamID: testUpstreamI,
			})
			convey.So(err, convey.ShouldBeNil)
			convey.So(resp.List, convey.ShouldBeEmpty)
		})
	})
}
