package proxy_svc

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/dispatch"
	"github.com/CodFrm/katch/internal/service/setting_svc"
)

// acquired 在后台取一个名额，返回一个「取到了」的信号与还名额的动作。
func acquired(ctx context.Context, slots *originSlots, limit int) (<-chan error, func()) {
	done := make(chan error, 1)
	var release func()
	go func() {
		var err error
		release, err = slots.acquire(ctx, limit)
		done <- err
	}()
	return done, func() {
		if release != nil {
			release()
		}
	}
}

// waitFor 等一个信号，等不到就报错。
func waitFor(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("等不到取名额的结果")
		return nil
	}
}

// TestOriginSlots_LimitIsReadEveryTime 并发上限是每次取名额时现读的。
//
// 这是「改完不必重启」在这道闸上的形态：上限不是构造时定死的信号量容量，
// 站长把它改小之后，下一个来取名额的就得等在途的那些还回来。
func TestOriginSlots_LimitIsReadEveryTime(t *testing.T) {
	convey.Convey("回源并发闸按每次传进来的上限放行", t, func() {
		slots := &originSlots{}
		ctx := context.Background()

		convey.Convey("上限之内不排队", func() {
			releaseFirst := mustAcquire(t, slots, ctx, 2)
			releaseSecond := mustAcquire(t, slots, ctx, 2)
			// 第三个要等：上限是 2，在途已经有 2 个。
			third, releaseThird := acquired(ctx, slots, 2)
			select {
			case <-third:
				t.Fatal("上限已满却还放行了一个")
			case <-time.After(50 * time.Millisecond):
			}
			releaseFirst()
			convey.So(waitFor(t, third), convey.ShouldBeNil)
			releaseSecond()
			releaseThird()
			convey.So(slots.inflight, convey.ShouldEqual, 0)
		})

		convey.Convey("上限改小之后，在途的还回来才轮得到下一个", func() {
			// 按旧的上限 3 取了两个名额。
			releaseFirst := mustAcquire(t, slots, ctx, 3)
			releaseSecond := mustAcquire(t, slots, ctx, 3)
			// 上限此刻被改成 1：在途的两个还回来之前，新的一个取不到名额。
			next, releaseNext := acquired(ctx, slots, 1)
			releaseFirst()
			select {
			case <-next:
				t.Fatal("上限已经改成 1，在途还有 1 个却放行了")
			case <-time.After(50 * time.Millisecond):
			}
			releaseSecond()
			convey.So(waitFor(t, next), convey.ShouldBeNil)
			releaseNext()
			convey.So(slots.inflight, convey.ShouldEqual, 0)
		})

		convey.Convey("上限为 0 表示不限并发", func() {
			for range 5 {
				release := mustAcquire(t, slots, ctx, 0)
				// 当初没取名额，还名额也是空动作，账不会被算成负数。
				release()
			}
			convey.So(slots.inflight, convey.ShouldEqual, 0)
		})
	})
}

// TestOriginSlots_AbandonedWaiterFreesTheQueue 等着等着走掉的那个不能把队伍堵死。
//
// 少了这条，一个等名额时被取消的请求会把那次唤醒带走，后面的人在名额空着的情况下
// 一直等下去——表现为镜像站在一次客户端断开之后整体停止回源。
func TestOriginSlots_AbandonedWaiterFreesTheQueue(t *testing.T) {
	convey.Convey("等待者被取消之后，名额照样轮到下一个", t, func() {
		slots := &originSlots{}
		releaseHolder := mustAcquire(t, slots, context.Background(), 1)

		leavingCtx, cancel := context.WithCancel(context.Background())
		leaving, _ := acquired(leavingCtx, slots, 1)
		staying, releaseStaying := acquired(context.Background(), slots, 1)
		// 让两个都排进队伍再取消，否则取消的是一个还没开始等的人。
		time.Sleep(50 * time.Millisecond)
		cancel()
		convey.So(waitFor(t, leaving), convey.ShouldEqual, context.Canceled)

		releaseHolder()
		convey.So(waitFor(t, staying), convey.ShouldBeNil)
		releaseStaying()
		convey.So(slots.inflight, convey.ShouldEqual, 0)
	})
}

// mustAcquire 取一个当场就该拿得到的名额。
func mustAcquire(t *testing.T, slots *originSlots, ctx context.Context, limit int) func() {
	t.Helper()
	release, err := slots.acquire(ctx, limit)
	if err != nil {
		t.Fatalf("取名额失败：%v", err)
	}
	return release
}

// TestFetch_QueueWaitIsBounded 排队等回源名额必须有上限。
//
// 名额从开始回源一直占到响应体被关闭，所以一个卡在上游那边的大对象会把名额攥住
// 很久。排在它后面的人等多久，取决于传进来那个 context——而拉取路径传进来的恰恰是
// 一个**不会结束**的 context：cache_svc 用 context.WithoutCancel 把客户端的取消摘掉
// （决策 9：客户端断开也要把这趟下载跑完），那种 context 的 Done() 是 nil，
// acquire 里那条 select 因此永远等不到第二个分支。
//
// 于是：上游卡住 + 并发上限用满，后面每一个请求都会永久挂死在取名额这一步，
// 连 gin 的处理器和客户端的连接一起攥着，客户端断开也解不开。超时也救不了——
// 那个计时器装在 fetchWithRetry 里，是取到名额**之后**才开始走的。
func TestFetch_QueueWaitIsBounded(t *testing.T) {
	convey.Convey("上游卡住时，排队的回源会超时退出而不是永久挂死", t, func() {
		stall := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-stall // 响应头发了，响应体一直不结束：名额就这么被攥着。
		}))
		// 先放开卡住的处理器再关服务器：反过来的话 Close 会等在那个还没返回的处理器上。
		defer srv.Close()
		defer close(stall)

		repo := setupRepo(t)
		repo.EXPECT().List(gomock.Any()).Return([]*upstream_entity.Upstream{
			{ID: 1, Host: "deb.debian.org", Kind: upstream_entity.KindStatic,
				Origin: srv.URL, Enabled: true},
		}, nil).AnyTimes()

		svc := New(Options{Runtime: singleSlotRuntime{}})

		pull := func(ctx context.Context) (io.ReadCloser, error) {
			body, _, err := svc.Fetch(ctx, &Target{
				Kind: dispatch.KindStatic, Host: "deb.debian.org",
				Path: "/dists/stable/InRelease", Method: http.MethodGet,
			})
			return body, err
		}

		// 第一个请求拿到名额并把它攥住：响应体一直没关。
		first, err := pull(context.Background())
		convey.So(err, convey.ShouldBeNil)
		defer func() { _ = first.Close() }()

		// 第二个请求传的是一个永远不会结束的 context，正是拉取路径的形态。
		second := make(chan error, 1)
		go func() {
			body, err := pull(context.Background())
			if body != nil {
				_ = body.Close()
			}
			second <- err
		}()
		select {
		case err := <-second:
			// 要的就是「带着错误回来」，而不是一直不回来。
			convey.So(err, convey.ShouldNotBeNil)
		case <-time.After(10 * time.Second):
			t.Fatal("排队的回源永久挂死了：取名额这一步既没有上限也取消不掉")
		}
	})
}

// singleSlotRuntime 并发上限 1、超时 1 秒：用最小的配置把「排队」这件事逼出来。
type singleSlotRuntime struct{}

func (singleSlotRuntime) Runtime(context.Context) (*setting_svc.RuntimeSettings, error) {
	return &setting_svc.RuntimeSettings{
		OriginConcurrency:    1,
		OriginTimeoutSeconds: 1,
		OriginRetries:        0,
	}, nil
}
