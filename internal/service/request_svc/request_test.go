package request_svc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cago-frame/cago/pkg/logger"
	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/CodFrm/katch/internal/metrics"
	"github.com/CodFrm/katch/internal/model/entity/request_log_entity"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/repository/request_log_repo"
	mock_request_log_repo "github.com/CodFrm/katch/internal/repository/request_log_repo/mock"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
	mock_upstream_repo "github.com/CodFrm/katch/internal/repository/upstream_repo/mock"
	"github.com/CodFrm/katch/internal/service/setting_svc"
)

// 这一层要验的是「缓冲里的记录怎么翻成一行行明细、保留期怎么裁、循环怎么跑」，
// 不连库：仓储用 mockgen 注入。

// 固定时刻：2023-11-14 22:13:20 UTC。
const testNow = int64(1700000000)

func fixedNow() time.Time { return time.Unix(testNow, 0) }

// fakeDrainer 扮演进程内缓冲：给什么就交什么，取走清不清由 metrics 那边的用例守。
type fakeDrainer struct{ recs []metrics.RecentRequest }

func (d *fakeDrainer) DrainRecent() []metrics.RecentRequest { return d.recs }

// fakeSettings 按 testNow 给出保留期，用例改一次读一次，验「取自设置」。
type fakeSettings struct {
	seconds int64
	err     error
}

func (f *fakeSettings) Runtime(context.Context) (*setting_svc.RuntimeSettings, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &setting_svc.RuntimeSettings{RecentRequestRetentionSeconds: f.seconds}, nil
}

type testDeps struct {
	repo     *mock_request_log_repo.MockRequestLogRepo
	upstream *mock_upstream_repo.MockUpstreamRepo
	drainer  *fakeDrainer
	settings *fakeSettings
}

func setup(t *testing.T) *testDeps {
	t.Helper()
	ctrl := gomock.NewController(t)
	deps := &testDeps{
		repo:     mock_request_log_repo.NewMockRequestLogRepo(ctrl),
		upstream: mock_upstream_repo.NewMockUpstreamRepo(ctrl),
		drainer:  &fakeDrainer{},
		settings: &fakeSettings{seconds: 86400},
	}
	request_log_repo.RegisterRequestLog(deps.repo)
	upstream_repo.RegisterUpstream(deps.upstream)
	return deps
}

// svc 按用例给的周期构造服务，其余依赖取本次 setup 的假件。
func (d *testDeps) svc(opt Options) RequestSvc {
	if opt.Now == nil {
		opt.Now = fixedNow
	}
	if opt.Drainer == nil {
		opt.Drainer = d.drainer
	}
	if opt.Settings == nil {
		opt.Settings = d.settings
	}
	return New(opt)
}

func knownUpstream(id int64, host string) *upstream_entity.Upstream {
	return &upstream_entity.Upstream{ID: id, Host: host, Enabled: true}
}

func TestRequest_Flush(t *testing.T) {
	convey.Convey("把缓冲里的一批记录翻成明细落库", t, func() {
		deps := setup(t)
		svc := deps.svc(Options{})
		ctx := context.Background()

		convey.Convey("主机名翻成 upstream_id，字段逐个对上", func() {
			deps.drainer.recs = []metrics.RecentRequest{
				{Upstream: "deb.debian.org", At: testNow, Object: "/dists/stable/InRelease",
					Result: metrics.ResultHit, Bytes: 100, DurationMS: 12},
				{Upstream: "proxy.golang.org", At: testNow - 1, Object: "/github.com/x/y/@v/list",
					Result: metrics.ResultMiss, Bytes: 200, DurationMS: 34},
			}
			deps.upstream.EXPECT().FindByHost(gomock.Any(), "deb.debian.org").
				Return(knownUpstream(7, "deb.debian.org"), nil)
			deps.upstream.EXPECT().FindByHost(gomock.Any(), "proxy.golang.org").
				Return(knownUpstream(8, "proxy.golang.org"), nil)
			var saved []*request_log_entity.RecentRequest
			deps.repo.EXPECT().Save(gomock.Any(), gomock.Any()).
				DoAndReturn(func(_ context.Context, rows []*request_log_entity.RecentRequest) error {
					saved = rows
					return nil
				})

			convey.So(svc.Flush(ctx), convey.ShouldBeNil)
			convey.So(saved, convey.ShouldHaveLength, 2)
			convey.So(saved[0].UpstreamID, convey.ShouldEqual, 7)
			convey.So(saved[0].At, convey.ShouldEqual, testNow)
			convey.So(saved[0].Object, convey.ShouldEqual, "/dists/stable/InRelease")
			convey.So(saved[0].Result, convey.ShouldEqual, "hit")
			convey.So(saved[0].Bytes, convey.ShouldEqual, 100)
			convey.So(saved[0].DurationMS, convey.ShouldEqual, 12)
			// 两个时间戳记的是这行什么时候写进来的，只用于排障。
			convey.So(saved[0].Createtime, convey.ShouldEqual, testNow)
			convey.So(saved[0].Updatetime, convey.ShouldEqual, testNow)
			convey.So(saved[1].UpstreamID, convey.ShouldEqual, 8)
			convey.So(saved[1].At, convey.ShouldEqual, testNow-1)
		})

		convey.Convey("翻不出来的一行没有归属，丢弃，其余照落", func() {
			deps.drainer.recs = []metrics.RecentRequest{
				{Upstream: "gone.example.com", At: testNow},
				{Upstream: "deb.debian.org", At: testNow, Object: "/a"},
			}
			deps.upstream.EXPECT().FindByHost(gomock.Any(), "gone.example.com").Return(nil, nil)
			deps.upstream.EXPECT().FindByHost(gomock.Any(), "deb.debian.org").
				Return(knownUpstream(7, "deb.debian.org"), nil)
			var saved []*request_log_entity.RecentRequest
			deps.repo.EXPECT().Save(gomock.Any(), gomock.Any()).
				DoAndReturn(func(_ context.Context, rows []*request_log_entity.RecentRequest) error {
					saved = rows
					return nil
				})

			convey.So(svc.Flush(ctx), convey.ShouldBeNil)
			convey.So(saved, convey.ShouldHaveLength, 1)
			convey.So(saved[0].UpstreamID, convey.ShouldEqual, 7)
		})

		convey.Convey("一个主机只查一次上游表", func() {
			// 一批里同一个上游通常占绝大多数，逐个记录查一次库等于把落库
			// 变成每秒几十次读。FindByHost 默认 Times(1) 就是这条断言。
			deps.drainer.recs = []metrics.RecentRequest{
				{Upstream: "deb.debian.org", At: testNow, Object: "/a"},
				{Upstream: "deb.debian.org", At: testNow, Object: "/b"},
				{Upstream: "deb.debian.org", At: testNow, Object: "/c"},
			}
			deps.upstream.EXPECT().FindByHost(gomock.Any(), "deb.debian.org").
				Return(knownUpstream(7, "deb.debian.org"), nil)
			deps.repo.EXPECT().Save(gomock.Any(), gomock.Any()).Return(nil)

			convey.So(svc.Flush(ctx), convey.ShouldBeNil)
		})

		convey.Convey("全都翻不出来时不写空的一批", func() {
			deps.drainer.recs = []metrics.RecentRequest{{Upstream: "gone.example.com", At: testNow}}
			deps.upstream.EXPECT().FindByHost(gomock.Any(), "gone.example.com").Return(nil, nil)

			convey.So(svc.Flush(ctx), convey.ShouldBeNil)
		})

		convey.Convey("缓冲区空的时候不碰库", func() {
			convey.So(svc.Flush(ctx), convey.ShouldBeNil)
		})
	})
}

// TestRequest_FlushDropsFailedBatch 落库失败丢掉这一批（决策 9）。
//
// 失败之后不重试：这一批已经在 Drain 时离开了缓冲区，下一轮拿到的只会是新的行，
// 一个不可用的库不该顺带把内存变成一个越积越大的重试队列。
func TestRequest_FlushDropsFailedBatch(t *testing.T) {
	convey.Convey("落库失败丢掉这一批", t, func() {
		deps := setup(t)
		svc := deps.svc(Options{})
		ctx := context.Background()

		deps.drainer.recs = []metrics.RecentRequest{{Upstream: "deb.debian.org", At: testNow}}
		deps.upstream.EXPECT().FindByHost(gomock.Any(), "deb.debian.org").
			Return(knownUpstream(7, "deb.debian.org"), nil)
		deps.repo.EXPECT().Save(gomock.Any(), gomock.Any()).Return(errors.New("库不可用"))

		err := svc.Flush(ctx)
		convey.So(err, convey.ShouldNotBeNil)

		convey.Convey("下一轮是空缓冲，不会带着上一批重试", func() {
			deps.drainer.recs = nil
			convey.So(svc.Flush(ctx), convey.ShouldBeNil)
		})
	})
}

// TestRequest_FlushLookupError 上游表读不出来时这一批也不写。
func TestRequest_FlushLookupError(t *testing.T) {
	convey.Convey("主机名翻不出 id 时丢掉这一批并报错", t, func() {
		deps := setup(t)
		svc := deps.svc(Options{})

		deps.drainer.recs = []metrics.RecentRequest{{Upstream: "deb.debian.org", At: testNow}}
		deps.upstream.EXPECT().FindByHost(gomock.Any(), "deb.debian.org").
			Return(nil, errors.New("库不可用"))

		convey.So(svc.Flush(context.Background()), convey.ShouldNotBeNil)
	})
}

func TestRequest_Prune(t *testing.T) {
	convey.Convey("按保留期裁掉超期的行", t, func() {
		deps := setup(t)
		svc := deps.svc(Options{})

		convey.Convey("保留期取自设置", func() {
			deps.settings.seconds = 3600
			deps.repo.EXPECT().Prune(gomock.Any(), testNow-3600).Return(int64(42), nil)

			removed, err := svc.Prune(context.Background())
			convey.So(err, convey.ShouldBeNil)
			convey.So(removed, convey.ShouldEqual, 42)
		})

		convey.Convey("改小保留期之后立刻按新值裁", func() {
			// 决策 10：不必等下一个整点，设置改完重启就是止血手段。
			deps.settings.seconds = 604800
			deps.repo.EXPECT().Prune(gomock.Any(), testNow-604800).Return(int64(0), nil)

			removed, err := svc.Prune(context.Background())
			convey.So(err, convey.ShouldBeNil)
			convey.So(removed, convey.ShouldEqual, 0)
		})

		convey.Convey("设置读不出来时不碰库", func() {
			deps.settings.err = errors.New("库不可用")
			// 没有 Prune 预期：真的去删了会因为缺 mock 调用当场失败。
			_, err := svc.Prune(context.Background())
			convey.So(err, convey.ShouldNotBeNil)
		})
	})
}

// captureLogs 把全局日志换成一个按级别过滤的内存 core。
//
// 和 metrics 那边的做法一致：级别拦截要真的走 zap 的 level 检查，
// 这样「这条 error 真的被节流了」才和线上是同一件事。
func captureLogs(t *testing.T, level zapcore.Level) *observer.ObservedLogs {
	t.Helper()
	core, logs := observer.New(level)
	logger.SetLogger(zap.New(core))
	t.Cleanup(func() { logger.SetLogger(zap.NewNop()) })
	return logs
}

// TestRequest_RunPrunesOnStart 启动时先裁一次，再进每小时的循环（决策 10）。
//
// 把周期设成一小时，断言这一趟里 Prune 只被调了一次：那一次只能是启动那一次。
func TestRequest_RunPrunesOnStart(t *testing.T) {
	deps := setup(t)
	svc := deps.svc(Options{FlushInterval: time.Hour, PruneInterval: time.Hour})

	pruned := make(chan struct{})
	deps.repo.EXPECT().Prune(gomock.Any(), testNow-deps.settings.seconds).
		DoAndReturn(func(context.Context, int64) (int64, error) {
			close(pruned)
			return 3, nil
		})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.Run(ctx)
	}()

	select {
	case <-pruned:
	case <-time.After(2 * time.Second):
		t.Fatal("Run 没有在启动时先裁一次")
	}
	cancel()
	<-done
}

// TestRequest_CloseHandleFlushesOnExit 退出前落一次，最多丢 1 秒的尾部（失败与降级一节）。
//
// 循环退出本身不落库：Run 的 ctx.Done 分支只 return，那一批只能来自 CloseHandle。
// 周期设成一小时，中途一次 tick 都不会响。
func TestRequest_CloseHandleFlushesOnExit(t *testing.T) {
	deps := setup(t)
	svc := deps.svc(Options{FlushInterval: time.Hour, PruneInterval: time.Hour})

	deps.drainer.recs = []metrics.RecentRequest{{Upstream: "deb.debian.org", At: testNow, Object: "/a"}}
	deps.repo.EXPECT().Prune(gomock.Any(), gomock.Any()).Return(int64(0), nil)
	deps.upstream.EXPECT().FindByHost(gomock.Any(), "deb.debian.org").
		Return(knownUpstream(7, "deb.debian.org"), nil)
	var saved []*request_log_entity.RecentRequest
	deps.repo.EXPECT().Save(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, rows []*request_log_entity.RecentRequest) error {
			saved = rows
			return nil
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := svc.Start(ctx, nil); err != nil {
		t.Fatalf("Start 失败：%v", err)
	}
	cancel()
	// 等循环真的退出：此刻还不该有落库。真的落了，就是退出落库没等循环
	// 退出（或还挂在 Run 的 ctx.Done 分支里），会和框架关库赛跑。
	<-svc.(*requestSvc).runDone
	if len(saved) != 0 {
		t.Fatalf("循环退出时就落库了，退出那批应由 CloseHandle 落：%+v", saved)
	}

	svc.CloseHandle()

	if len(saved) != 1 || saved[0].UpstreamID != 7 {
		t.Fatalf("CloseHandle 没有把缓冲落库：%+v", saved)
	}
}

// TestRequest_CloseHandleBoundsTheExitFlush 退出落库有上限（失败与降级一节）。
//
// main 里缓存那一趟收尾挂了 cacheDrainTimeout，这里是同一个理由：一个不响应的库
// 不该把退出拖到只剩 SIGKILL。用例把落库卡在 ctx 上——只有 ctx 带截止时刻才能
// 把它放走，所以 CloseHandle 能在上限内返回就说明它真的给自己留了那条退路。
func TestRequest_CloseHandleBoundsTheExitFlush(t *testing.T) {
	deps := setup(t)
	svc := deps.svc(Options{
		FlushInterval:    time.Hour,
		PruneInterval:    time.Hour,
		ExitFlushTimeout: 100 * time.Millisecond,
	})

	deps.repo.EXPECT().Prune(gomock.Any(), gomock.Any()).Return(int64(0), nil)
	deps.drainer.recs = []metrics.RecentRequest{{Upstream: "deb.debian.org", At: testNow}}
	deps.upstream.EXPECT().FindByHost(gomock.Any(), "deb.debian.org").
		Return(knownUpstream(7, "deb.debian.org"), nil)
	deps.repo.EXPECT().Save(gomock.Any(), gomock.Any()).
		DoAndReturn(func(ctx context.Context, _ []*request_log_entity.RecentRequest) error {
			// 一个不响应的库：谁来都只能等 ctx。
			<-ctx.Done()
			return ctx.Err()
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := svc.Start(ctx, nil); err != nil {
		t.Fatalf("Start 失败：%v", err)
	}
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.CloseHandle()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("库不响应时 CloseHandle 没有上限，把退出挂住了")
	}
}

// TestRequest_CloseHandleBoundsTheLoopWait 循环卡住时退出也不能被挂住。
//
// 等循环退出同样有上限：这里的 Prune 上库不动了，循环永远不会返回，CloseHandle
// 不能跟它一起等下去。
func TestRequest_CloseHandleBoundsTheLoopWait(t *testing.T) {
	deps := setup(t)
	svc := deps.svc(Options{
		FlushInterval:    time.Hour,
		PruneInterval:    time.Hour,
		ExitFlushTimeout: 100 * time.Millisecond,
	})

	stuck := make(chan struct{})
	defer close(stuck)
	deps.repo.EXPECT().Prune(gomock.Any(), gomock.Any()).
		DoAndReturn(func(context.Context, int64) (int64, error) {
			// 一个卡住的库：连取消都不理的驱动。
			<-stuck
			return 0, nil
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := svc.Start(ctx, nil); err != nil {
		t.Fatalf("Start 失败：%v", err)
	}
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.CloseHandle()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("循环卡住时 CloseHandle 没有上限，把退出挂住了")
	}
}

// TestRequest_CloseHandleWithoutStart Start 没跑过时 CloseHandle 是空操作。
//
// 没有循环要等，也不该凭空去碰库：这一组用例一个 mock 预期都没给，真的落了库会
// 因为非预期调用当场失败。
func TestRequest_CloseHandleWithoutStart(t *testing.T) {
	deps := setup(t)
	svc := deps.svc(Options{FlushInterval: time.Hour, PruneInterval: time.Hour})
	deps.drainer.recs = []metrics.RecentRequest{{Upstream: "deb.debian.org", At: testNow}}

	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.CloseHandle()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start 没跑过时 CloseHandle 阻塞了")
	}
}

// oneShotDrainer 和 metrics 一样取走即清空：CloseHandle 调两次时，第二次看到的
// 必须是空缓冲，否则同一批会被落两遍。
type oneShotDrainer struct{ recs []metrics.RecentRequest }

func (d *oneShotDrainer) DrainRecent() []metrics.RecentRequest {
	out := d.recs
	d.recs = nil
	return out
}

// TestRequest_CloseHandleTwice CloseHandle 被调两次：第二次面对的是空缓冲，
// 不重复落库也不阻塞。
func TestRequest_CloseHandleTwice(t *testing.T) {
	deps := setup(t)
	svc := deps.svc(Options{
		Drainer:          &oneShotDrainer{recs: []metrics.RecentRequest{{Upstream: "deb.debian.org", At: testNow}}},
		FlushInterval:    time.Hour,
		PruneInterval:    time.Hour,
		ExitFlushTimeout: time.Second,
	})

	// 各 Times(1)：第二次 CloseHandle 再去查上游或写库都会当场失败。
	deps.repo.EXPECT().Prune(gomock.Any(), gomock.Any()).Return(int64(0), nil)
	deps.upstream.EXPECT().FindByHost(gomock.Any(), "deb.debian.org").
		Return(knownUpstream(7, "deb.debian.org"), nil)
	deps.repo.EXPECT().Save(gomock.Any(), gomock.Any()).Return(nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := svc.Start(ctx, nil); err != nil {
		t.Fatalf("Start 失败：%v", err)
	}
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.CloseHandle()
		svc.CloseHandle()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("CloseHandle 调两次时阻塞了")
	}
}

// TestRequest_RunFlushesEveryTick 后台循环按周期把缓冲区取走一批落库。
func TestRequest_RunFlushesEveryTick(t *testing.T) {
	deps := setup(t)
	svc := deps.svc(Options{
		FlushInterval: 10 * time.Millisecond,
		PruneInterval: time.Hour,
	})

	deps.drainer.recs = []metrics.RecentRequest{{Upstream: "deb.debian.org", At: testNow}}
	deps.repo.EXPECT().Prune(gomock.Any(), gomock.Any()).AnyTimes().Return(int64(0), nil)
	deps.upstream.EXPECT().FindByHost(gomock.Any(), "deb.debian.org").
		AnyTimes().Return(knownUpstream(7, "deb.debian.org"), nil)
	flushes := make(chan struct{}, 64)
	deps.repo.EXPECT().Save(gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(context.Context, []*request_log_entity.RecentRequest) error {
			flushes <- struct{}{}
			return nil
		})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.Run(ctx)
	}()

	// 等两次落库：说明是循环在落，而不是只在退出时补落一次。
	for i := 0; i < 2; i++ {
		select {
		case <-flushes:
		case <-time.After(2 * time.Second):
			t.Fatal("Run 没有按周期把缓冲区取走落库")
		}
	}
	cancel()
	<-done
}

// TestRequest_RunThrottlesFlushErrors 连续失败时那条 error 按分钟节流（决策 9）。
//
// 假时钟钉在 testNow 上：没有节流的话 2ms 一轮会写下上百条，而这里要的是 1 条。
func TestRequest_RunThrottlesFlushErrors(t *testing.T) {
	logs := captureLogs(t, zapcore.ErrorLevel)

	deps := setup(t)
	svc := deps.svc(Options{
		FlushInterval: 2 * time.Millisecond,
		PruneInterval: time.Hour,
	})

	deps.drainer.recs = []metrics.RecentRequest{{Upstream: "deb.debian.org", At: testNow}}
	deps.repo.EXPECT().Prune(gomock.Any(), gomock.Any()).AnyTimes().Return(int64(0), nil)
	deps.upstream.EXPECT().FindByHost(gomock.Any(), "deb.debian.org").
		AnyTimes().Return(knownUpstream(7, "deb.debian.org"), nil)
	attempts := make(chan struct{}, 4096)
	deps.repo.EXPECT().Save(gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(context.Context, []*request_log_entity.RecentRequest) error {
			attempts <- struct{}{}
			return errors.New("库不可用")
		})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.Run(ctx)
	}()

	for i := 0; i < 20; i++ {
		select {
		case <-attempts:
		case <-time.After(2 * time.Second):
			t.Fatal("Run 没有在反复落库")
		}
	}
	cancel()
	<-done

	if got := logs.FilterMessage(flushErrorMessage).Len(); got != 1 {
		t.Fatalf("失败日志写了 %d 条，应按分钟节流成 1 条", got)
	}
}
