package backoff

import (
	"testing"
	"time"

	"github.com/smartystreets/goconvey/convey"
)

// 退避是内存态（决策 17），所以这一层没有库可以问：全部用注入的时钟来验，
// 不用 sleep——真等一个退避窗口过去会让整个测试套慢上几秒。

// newTestTracker 返回一个跟着 clock 走的 tracker，clock 由调用方推进。
func newTestTracker(clock *time.Time) *Tracker {
	return New(Options{
		Threshold: 3,
		Base:      time.Second,
		Max:       8 * time.Second,
		Now:       func() time.Time { return *clock },
	})
}

func TestTracker_EnterAndLeaveBackoff(t *testing.T) {
	convey.Convey("上游连续失败达阈值后进入退避", t, func() {
		clock := time.Unix(1700000000, 0)
		tracker := newTestTracker(&clock)

		convey.Convey("没到阈值之前照常放行", func() {
			tracker.Failure("deb.debian.org")
			tracker.Failure("deb.debian.org")
			// 偶发的两次失败不该把一个上游判成降级：网络抖一下就停服，
			// 比抖动本身更糟。
			convey.So(tracker.Allow("deb.debian.org"), convey.ShouldBeTrue)
			convey.So(tracker.Degraded("deb.debian.org"), convey.ShouldBeFalse)
		})

		convey.Convey("达到阈值后快速失败并标为降级", func() {
			for i := 0; i < 3; i++ {
				tracker.Failure("deb.debian.org")
			}
			convey.So(tracker.Allow("deb.debian.org"), convey.ShouldBeFalse)
			convey.So(tracker.Degraded("deb.debian.org"), convey.ShouldBeTrue)

			convey.Convey("别的上游不受影响", func() {
				convey.So(tracker.Allow("proxy.golang.org"), convey.ShouldBeTrue)
				convey.So(tracker.Degraded("proxy.golang.org"), convey.ShouldBeFalse)
			})

			convey.Convey("退避窗口过去后放行探测", func() {
				clock = clock.Add(time.Second)
				convey.So(tracker.Allow("deb.debian.org"), convey.ShouldBeTrue)
				// 探测还没有结果，上游仍然算降级——界面上不能因为「可以试一下了」
				// 就把降级标记摘掉。
				convey.So(tracker.Degraded("deb.debian.org"), convey.ShouldBeTrue)

				convey.Convey("探测再失败，退避时间翻倍", func() {
					tracker.Failure("deb.debian.org")
					clock = clock.Add(time.Second)
					convey.So(tracker.Allow("deb.debian.org"), convey.ShouldBeFalse)
					clock = clock.Add(time.Second)
					convey.So(tracker.Allow("deb.debian.org"), convey.ShouldBeTrue)
				})

				convey.Convey("探测成功就退出退避", func() {
					tracker.Success("deb.debian.org")
					convey.So(tracker.Degraded("deb.debian.org"), convey.ShouldBeFalse)
					convey.So(tracker.Snapshot(), convey.ShouldBeEmpty)
					// 计数要归零：不归零的话，恢复之后再失败一次就又进退避。
					tracker.Failure("deb.debian.org")
					convey.So(tracker.Allow("deb.debian.org"), convey.ShouldBeTrue)
				})
			})
		})
	})
}

func TestTracker_DelayCappedAtMax(t *testing.T) {
	convey.Convey("退避时间指数增长但不超过上限", t, func() {
		clock := time.Unix(1700000000, 0)
		tracker := newTestTracker(&clock)
		// 阈值 3，base 1s，上限 8s：第 3 次失败退 1s，之后 2、4、8、8……
		//
		// 每记一次失败就把时钟推到这一次的窗口末尾：只有被放行、真的打到上游的
		// 失败才计数（见 TestTracker_BlockedRequestsDoNotExtendWindow），挤在同
		// 一个窗口里连打 12 次的话，量到的根本不是第 12 次的退避时长。
		window := time.Duration(0)
		for i := 0; i < 12; i++ {
			at := clock
			tracker.Failure("deb.debian.org")
			snapshot := tracker.Snapshot()
			if len(snapshot) == 0 {
				// 还没到阈值，这一次失败没有窗口。
				continue
			}
			retryAt := time.Unix(snapshot[0].RetryAt, 0)
			window = retryAt.Sub(at)
			clock = retryAt
		}
		// 上限之外再长的退避只会让一个已经恢复的上游迟迟不被重试：没有上限的话，
		// 第 12 次失败要退 512s。
		convey.So(window, convey.ShouldEqual, 8*time.Second)
		convey.So(tracker.Allow("deb.debian.org"), convey.ShouldBeTrue)
	})
}

func TestTracker_Snapshot(t *testing.T) {
	convey.Convey("快照只列出降级中的上游，供界面标注", t, func() {
		clock := time.Unix(1700000000, 0)
		tracker := newTestTracker(&clock)
		tracker.Failure("proxy.golang.org")
		for i := 0; i < 3; i++ {
			tracker.Failure("deb.debian.org")
		}

		got := tracker.Snapshot()
		convey.So(len(got), convey.ShouldEqual, 1)
		convey.So(got[0].Host, convey.ShouldEqual, "deb.debian.org")
		convey.So(got[0].Failures, convey.ShouldEqual, 3)
		convey.So(got[0].RetryAt, convey.ShouldEqual, clock.Add(time.Second).Unix())
	})
}

func TestTracker_Defaults(t *testing.T) {
	convey.Convey("不给参数时用默认阈值与退避时长", t, func() {
		tracker := New(Options{})
		for i := 0; i < defaultThreshold; i++ {
			tracker.Failure("deb.debian.org")
		}
		convey.So(tracker.Allow("deb.debian.org"), convey.ShouldBeFalse)
		convey.So(tracker.Degraded("deb.debian.org"), convey.ShouldBeTrue)
	})
}

// TestTracker_BlockedRequestsDoNotExtendWindow 退避窗口里的失败不是新证据。
//
// 闸接上之后，窗口期内的请求会被挡在回源之前直接失败，而计数中间件看到的仍旧
// 是一个失败的响应，于是原样喂回来。若把它当成一次新的回源失败记下来，docker /
// apt 这些会自己重试的客户端只要还在问，退避就会被一路顶到上限、并且每来一个
// 请求就再顺延一次——上游早恢复了也永远等不到那次探测。
func TestTracker_BlockedRequestsDoNotExtendWindow(t *testing.T) {
	convey.Convey("被闸挡住的请求不延长退避窗口", t, func() {
		clock := time.Unix(1700000000, 0)
		tracker := newTestTracker(&clock)
		for i := 0; i < 3; i++ {
			tracker.Failure("deb.debian.org")
		}

		// 窗口里来了一批请求：它们一个都没打到上游，只是各自快速失败了一次。
		for i := 0; i < 10; i++ {
			convey.So(tracker.Allow("deb.debian.org"), convey.ShouldBeFalse)
			tracker.Failure("deb.debian.org")
		}

		// 窗口该还是原来那 1 秒。
		clock = clock.Add(time.Second)
		convey.So(tracker.Allow("deb.debian.org"), convey.ShouldBeTrue)
		convey.So(tracker.Snapshot()[0].Failures, convey.ShouldEqual, 3)

		convey.Convey("窗口过去之后真的探测过一次再失败，才翻倍", func() {
			tracker.Failure("deb.debian.org")
			convey.So(tracker.Allow("deb.debian.org"), convey.ShouldBeFalse)
			clock = clock.Add(2 * time.Second)
			convey.So(tracker.Allow("deb.debian.org"), convey.ShouldBeTrue)
		})
	})
}
