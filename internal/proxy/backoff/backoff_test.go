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
		for i := 0; i < 12; i++ {
			tracker.Failure("deb.debian.org")
		}
		clock = clock.Add(8 * time.Second)
		// 上限之外再长的退避只会让一个已经恢复的上游迟迟不被重试。
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
