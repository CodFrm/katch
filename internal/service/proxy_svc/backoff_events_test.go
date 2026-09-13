package proxy_svc_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"

	"github.com/CodFrm/katch/internal/metrics"
	"github.com/CodFrm/katch/internal/model/entity/event_entity"
	"github.com/CodFrm/katch/internal/proxy/backoff"
	"github.com/CodFrm/katch/internal/repository/event_repo"
	mock_event_repo "github.com/CodFrm/katch/internal/repository/event_repo/mock"
	"github.com/CodFrm/katch/internal/service/proxy_svc"
)

// eventSink 事件仓储的内存版。
//
// 判据是「表里多了几条」，逐次 EXPECT 写不出「第 2 到第 5 次失败什么都不该落」
// 这个否定式——那种用例只要实现每次都记，照样绿。
type eventSink struct {
	mu   sync.Mutex
	rows []*event_entity.Event
}

func newEventSink(t *testing.T) *eventSink {
	t.Helper()
	sink := &eventSink{}
	m := mock_event_repo.NewMockEventRepo(gomock.NewController(t))
	m.EXPECT().Create(gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(_ context.Context, e *event_entity.Event) error {
			sink.mu.Lock()
			defer sink.mu.Unlock()
			dup := *e
			sink.rows = append(sink.rows, &dup)
			return nil
		})
	prev := event_repo.Event()
	event_repo.RegisterEvent(m)
	t.Cleanup(func() { event_repo.RegisterEvent(prev) })
	return sink
}

func (s *eventSink) all() []*event_entity.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := make([]*event_entity.Event, 0, len(s.rows))
	for _, row := range s.rows {
		dup := *row
		list = append(list, &dup)
	}
	return list
}

// pullEngine 拉取路径的生产形态：计数中间件在最外层，NoRoute 是拉取处理器。
//
// 用真的中间件而不是直接调闸：退避的成败就是由它喂进去的（feedGate），
// 自己在用例里调一遍 Failure 等于绕开被测的那条路。status 决定这次拉取算什么——
// 502 是回源失败（embed.go 拿不到响应时就发这个）。
func pullEngine(gate metrics.Gate, status *int) *gin.Engine {
	gin.SetMode(gin.TestMode)
	rec := metrics.New(metrics.Options{Registerer: prometheus.NewRegistry()})
	engine := gin.New()
	engine.Use(rec.Middleware(metrics.Hooks{
		Gate: gate,
		Lookup: func(_ context.Context, host string) bool {
			return host == "deb.debian.org"
		},
	}))
	engine.NoRoute(func(c *gin.Context) { c.Status(*status) })
	return engine
}

func pull(engine *gin.Engine) {
	engine.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/deb.debian.org/dists/stable/InRelease", nil))
}

// TestBackoffEvents_DegradeIsRecordedOncePerTransition 任务目标 (b)：上游进入退避
// 落一条事件，而且**只落一条**。
//
// 退避是决策 17 的运行时事实（界面标为限流中/降级），它该在时间线上留一条自动
// 告警——和人为变更同一条线。但它是一次**状态转换**，不是每个请求各自的遭遇：
// 一个挂掉的上游后面还跟着 docker / apt 的自动重试，每次失败都记一条会让这条
// 时间线在最需要看清「什么时候开始坏的」的时候被同一件事刷满几百行。
func TestBackoffEvents_DegradeIsRecordedOncePerTransition(t *testing.T) {
	convey.Convey("上游连续回源失败进入退避", t, func() {
		sink := newEventSink(t)
		clock := time.Unix(1700000000, 0)
		gate := proxy_svc.NewEventGate(backoff.New(backoff.Options{
			Threshold: 3, Base: time.Minute, Max: time.Minute,
			Now: func() time.Time { return clock },
		}))
		status := http.StatusBadGateway
		engine := pullEngine(gate, &status)

		convey.Convey("还没到阈值时不报降级——单次超时更可能是这一个对象的事", func() {
			pull(engine)
			pull(engine)
			convey.So(len(sink.all()), convey.ShouldEqual, 0)
		})

		convey.Convey("到了阈值落一条，之后再失败多少次都不再落", func() {
			for range 8 {
				pull(engine)
			}
			rows := sink.all()
			convey.So(len(rows), convey.ShouldEqual, 1)
			convey.So(rows[0].Kind, convey.ShouldEqual, event_entity.KindUpstreamDegraded)
			// 没有人按下任何按钮：这条得和人为变更分得开。
			convey.So(rows[0].Actor, convey.ShouldEqual, event_entity.ActorSystem)
			detail := map[string]any{}
			convey.So(json.Unmarshal([]byte(rows[0].Detail), &detail), convey.ShouldBeNil)
			convey.So(detail["host"], convey.ShouldEqual, "deb.debian.org")

			convey.Convey("恢复之后再落一条，是另一个 kind", func() {
				// 窗口过去，探测成功：一次真的回源成功才算上游还活着。
				clock = clock.Add(time.Minute)
				status = http.StatusOK
				pull(engine)
				rows := sink.all()
				convey.So(len(rows), convey.ShouldEqual, 2)
				convey.So(rows[1].Kind, convey.ShouldEqual, event_entity.KindUpstreamRecovered)
				convey.So(rows[1].Actor, convey.ShouldEqual, event_entity.ActorSystem)

				convey.Convey("继续成功的拉取不再往时间线上加东西", func() {
					pull(engine)
					pull(engine)
					convey.So(len(sink.all()), convey.ShouldEqual, 2)
				})
			})
		})

		convey.Convey("闸自身的行为没有被包装改掉：退避期间仍然快速失败", func() {
			for range 3 {
				pull(engine)
			}
			convey.So(gate.Allow("deb.debian.org"), convey.ShouldBeFalse)
			convey.So(gate.Degraded("deb.debian.org"), convey.ShouldBeTrue)
			convey.So(gate.Allow("proxy.golang.org"), convey.ShouldBeTrue)
		})
	})
}

// TestBackoffEvents_CacheHitsDoNotTouchTheTimeline 缓存命中不是回源，它既不该
// 把一个降级中的上游报成恢复，也不该往时间线上加任何东西。
func TestBackoffEvents_CacheHitsDoNotTouchTheTimeline(t *testing.T) {
	convey.Convey("命中与被拒绝的请求不产生事件", t, func() {
		sink := newEventSink(t)
		clock := time.Unix(1700000000, 0)
		gate := proxy_svc.NewEventGate(backoff.New(backoff.Options{
			Threshold: 3, Base: time.Minute, Max: time.Minute,
			Now: func() time.Time { return clock },
		}))
		status := http.StatusBadGateway
		engine := pullEngine(gate, &status)
		for range 3 {
			pull(engine)
		}
		convey.So(len(sink.all()), convey.ShouldEqual, 1)

		// 盘上已有的副本在降级期间照常服务（决策 17），但它没碰过上游。
		gin.SetMode(gin.TestMode)
		rec := metrics.New(metrics.Options{Registerer: prometheus.NewRegistry()})
		hitEngine := gin.New()
		hitEngine.Use(rec.Middleware(metrics.Hooks{
			Gate:   gate,
			Lookup: func(_ context.Context, host string) bool { return host == "deb.debian.org" },
		}))
		hitEngine.NoRoute(func(c *gin.Context) {
			c.Header("X-Katch-Cache", "HIT")
			c.Status(http.StatusOK)
		})
		pull(hitEngine)

		convey.So(len(sink.all()), convey.ShouldEqual, 1)
		convey.So(gate.Degraded("deb.debian.org"), convey.ShouldBeTrue)
	})
}
