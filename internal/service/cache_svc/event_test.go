package cache_svc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"

	"github.com/CodFrm/katch/internal/model/entity/event_entity"
	"github.com/CodFrm/katch/internal/repository/event_repo"
	mock_event_repo "github.com/CodFrm/katch/internal/repository/event_repo/mock"
)

// errAlwaysFails 事件表写不进去。
var errAlwaysFails = errors.New("events 表写不进去")

// eventSink 事件仓储的内存版。
//
// 判据是「表里多了几条」：逐次 EXPECT 写不出「回收跑过一轮只记一条、没淘汰
// 任何东西时一条都不记」这种否定式。
type eventSink struct {
	mu   sync.Mutex
	rows []*event_entity.Event
}

func newEventSink(t *testing.T, failCreate error) *eventSink {
	t.Helper()
	sink := &eventSink{}
	m := mock_event_repo.NewMockEventRepo(gomock.NewController(t))
	m.EXPECT().Create(gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(_ context.Context, e *event_entity.Event) error {
			if failCreate != nil {
				return failCreate
			}
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

// TestReclaimEvent_OnePerRun 任务目标 (c)：缓存触发回收落一条事件。
//
// 「缓存」一节说超过回收水位就按最近最少使用淘汰。那是 katch 自己替站长做的
// 处置——盘上少了几个对象、下一次拉取要重新回源，界面上得看得见它发生过，
// 所以它和人为变更在同一条时间线上（概览：自动告警与人为变更放在一起）。
//
// 一次回收一条，不是一个被淘汰的对象一条：淘汰一次可能带走几百个对象，
// 那会把时间线冲掉，而运维要看的是「这台机器在什么时候开始削缓存了」。
func TestReclaimEvent_OnePerRun(t *testing.T) {
	convey.Convey("缓存回收跑过一轮，落一条事件", t, func() {
		sink := newEventSink(t, nil)
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "0123456789")
		})
		// 配额 25 字节、回收到 80%（20 字节）：写进第三个 10 字节的对象就得腾地方。
		svc, repo, _ := setupSvc(t, o, staticUpstream("deb.debian.org"),
			Options{Runtime: newFakeRuntime(t, quotaOf(25, 80))})

		putObject(t, svc, "/pool/a.deb", "aaaaaaaaaa", true)
		putObject(t, svc, "/pool/b.deb", "bbbbbbbbbb", true)
		convey.Convey("还没超配额时一条都不记", func() {
			convey.So(len(sink.all()), convey.ShouldEqual, 0)
		})

		convey.Convey("超了配额，回收跑一轮记一条", func() {
			putObject(t, svc, "/pool/c.deb", "cccccccccc", true)
			// 确认回收真的发生了，而不是用例在数一条凭空冒出来的事件。
			convey.So(repo.byKey("/pool/a.deb"), convey.ShouldBeNil)

			rows := sink.all()
			convey.So(len(rows), convey.ShouldEqual, 1)
			convey.So(rows[0].Kind, convey.ShouldEqual, event_entity.KindCacheReclaimed)
			// 没有人按下任何按钮。
			convey.So(rows[0].Actor, convey.ShouldEqual, event_entity.ActorSystem)

			convey.Convey("削掉了多少记在结构化字段里", func() {
				detail := map[string]any{}
				convey.So(json.Unmarshal([]byte(rows[0].Detail), &detail), convey.ShouldBeNil)
				convey.So(detail["removed"], convey.ShouldEqual, 1)
				convey.So(detail["freed_bytes"], convey.ShouldEqual, 10)
				convey.So(detail["quota_bytes"], convey.ShouldEqual, 25)
			})

			convey.Convey("命中不进时间线：那是每请求都会发生的事，不是状态变化", func() {
				r, _, err := svc.Get(context.Background(), target("deb.debian.org", "/pool/c.deb"))
				convey.So(err, convey.ShouldBeNil)
				_, _ = io.ReadAll(r)
				convey.So(r.Close(), convey.ShouldBeNil)
				convey.So(len(sink.all()), convey.ShouldEqual, 1)
			})
		})
	})
}

// TestReclaimEvent_FailedRecordStillReclaims 事件写不进去时，回收本身照常完成。
//
// 反过来的实现最坏：缓存超了配额而事件表满了，于是回收一次也跑不起来，
// 磁盘一路涨到写不下任何东西。
func TestReclaimEvent_FailedRecordStillReclaims(t *testing.T) {
	convey.Convey("事件记不上不影响回收", t, func() {
		newEventSink(t, errAlwaysFails)
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "0123456789")
		})
		svc, repo, _ := setupSvc(t, o, staticUpstream("deb.debian.org"),
			Options{Runtime: newFakeRuntime(t, quotaOf(25, 80))})

		putObject(t, svc, "/pool/a.deb", "aaaaaaaaaa", true)
		putObject(t, svc, "/pool/b.deb", "bbbbbbbbbb", true)
		putObject(t, svc, "/pool/c.deb", "cccccccccc", true)

		convey.So(repo.byKey("/pool/a.deb"), convey.ShouldBeNil)
		convey.So(repo.byKey("/pool/c.deb"), convey.ShouldNotBeNil)
	})
}
