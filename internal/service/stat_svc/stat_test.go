package stat_svc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"

	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/api/stat"
	"github.com/CodFrm/katch/internal/metrics"
	"github.com/CodFrm/katch/internal/model/entity/rollup_entity"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/backoff"
	"github.com/CodFrm/katch/internal/repository/rollup_repo"
	mock_rollup_repo "github.com/CodFrm/katch/internal/repository/rollup_repo/mock"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
	mock_upstream_repo "github.com/CodFrm/katch/internal/repository/upstream_repo/mock"
)

// 服务层用 mock 注入 repo，不连库：这一层要验的是「分钟桶怎么攒成一行、
// 区间怎么算、保留期怎么裁」，而不是 SQL。

// 固定时刻：2023-11-14 22:13:20 UTC。
const testNow = int64(1700000000)

type testDeps struct {
	rollup   *mock_rollup_repo.MockTrafficRollupRepo
	upstream *mock_upstream_repo.MockUpstreamRepo
	drained  []metrics.Bucket
	svc      StatSvc
}

// drain 扮演进程内计数器：取走一次就清空，和 metrics.Recorder 的约定一致。
func (d *testDeps) Drain() []metrics.Bucket {
	got := d.drained
	d.drained = nil
	return got
}

func setup(t *testing.T, degraded DegradeReporter) *testDeps {
	t.Helper()
	ctrl := gomock.NewController(t)
	deps := &testDeps{
		rollup:   mock_rollup_repo.NewMockTrafficRollupRepo(ctrl),
		upstream: mock_upstream_repo.NewMockUpstreamRepo(ctrl),
	}
	rollup_repo.RegisterTrafficRollup(deps.rollup)
	upstream_repo.RegisterUpstream(deps.upstream)
	deps.svc = New(Options{
		Now:      func() time.Time { return time.Unix(testNow, 0) },
		Drainer:  deps,
		Degraded: degraded,
	})
	return deps
}

func TestStat_Flush(t *testing.T) {
	convey.Convey("每分钟把进程内计数器落成一行分钟桶", t, func() {
		deps := setup(t, nil)
		ctx := context.Background()

		convey.Convey("这一分钟还没有行就新建一行", func() {
			deps.drained = []metrics.Bucket{{
				Host: "deb.debian.org", Bucket: 1700000040,
				Requests: 10, Hits: 6, Denied: 1, OriginErrors: 1,
				BytesServed: 4096, BytesOrigin: 1024,
			}}
			deps.upstream.EXPECT().FindByHost(gomock.Any(), "deb.debian.org").
				Return(&upstream_entity.Upstream{ID: 7, Host: "deb.debian.org", Enabled: true}, nil)
			deps.rollup.EXPECT().FindByBucket(gomock.Any(), int64(7), int64(1700000040)).Return(nil, nil)
			var saved *rollup_entity.TrafficRollup
			deps.rollup.EXPECT().Save(gomock.Any(), gomock.Any()).
				DoAndReturn(func(_ context.Context, row *rollup_entity.TrafficRollup) error {
					saved = row
					return nil
				})

			convey.So(deps.svc.Flush(ctx), convey.ShouldBeNil)
			convey.So(saved.UpstreamID, convey.ShouldEqual, 7)
			convey.So(saved.Bucket, convey.ShouldEqual, 1700000040)
			convey.So(saved.Requests, convey.ShouldEqual, 10)
			convey.So(saved.Hits, convey.ShouldEqual, 6)
			convey.So(saved.Denied, convey.ShouldEqual, 1)
			convey.So(saved.OriginErrors, convey.ShouldEqual, 1)
			convey.So(saved.BytesServed, convey.ShouldEqual, 4096)
			convey.So(saved.BytesOrigin, convey.ShouldEqual, 1024)
			convey.So(saved.Createtime, convey.ShouldEqual, testNow)
		})

		convey.Convey("同一分钟再落一次是累加，不是覆盖", func() {
			// 一分钟内可能落库两次（重启、或上一次落库慢了半拍），
			// 覆盖会把前半分钟的量整个抹掉。
			deps.drained = []metrics.Bucket{{Host: "deb.debian.org", Bucket: 1700000040, Requests: 3, Hits: 2}}
			deps.upstream.EXPECT().FindByHost(gomock.Any(), "deb.debian.org").
				Return(&upstream_entity.Upstream{ID: 7, Enabled: true}, nil)
			deps.rollup.EXPECT().FindByBucket(gomock.Any(), int64(7), int64(1700000040)).
				Return(&rollup_entity.TrafficRollup{ID: 5, UpstreamID: 7, Bucket: 1700000040, Requests: 10, Hits: 6}, nil)
			var saved *rollup_entity.TrafficRollup
			deps.rollup.EXPECT().Save(gomock.Any(), gomock.Any()).
				DoAndReturn(func(_ context.Context, row *rollup_entity.TrafficRollup) error {
					saved = row
					return nil
				})

			convey.So(deps.svc.Flush(ctx), convey.ShouldBeNil)
			convey.So(saved.ID, convey.ShouldEqual, 5)
			convey.So(saved.Requests, convey.ShouldEqual, 13)
			convey.So(saved.Hits, convey.ShouldEqual, 8)
		})

		convey.Convey("上游已经被删掉，这一分钟的量没有归属，丢掉即可", func() {
			deps.drained = []metrics.Bucket{{Host: "gone.example.com", Bucket: 1700000040, Requests: 3}}
			deps.upstream.EXPECT().FindByHost(gomock.Any(), "gone.example.com").Return(nil, nil)

			convey.So(deps.svc.Flush(ctx), convey.ShouldBeNil)
		})

		convey.Convey("一个上游落库失败不影响其余上游", func() {
			// 落库失败只丢这一分钟的统计，不能让整轮 flush 停在第一个错误上——
			// 统计坏掉不该连累别的上游的统计。
			deps.drained = []metrics.Bucket{
				{Host: "a.example.com", Bucket: 1700000040, Requests: 1},
				{Host: "b.example.com", Bucket: 1700000040, Requests: 2},
			}
			deps.upstream.EXPECT().FindByHost(gomock.Any(), "a.example.com").
				Return(nil, errors.New("库不可用"))
			deps.upstream.EXPECT().FindByHost(gomock.Any(), "b.example.com").
				Return(&upstream_entity.Upstream{ID: 8, Enabled: true}, nil)
			deps.rollup.EXPECT().FindByBucket(gomock.Any(), int64(8), int64(1700000040)).Return(nil, nil)
			deps.rollup.EXPECT().Save(gomock.Any(), gomock.Any()).Return(nil)

			convey.So(deps.svc.Flush(ctx), convey.ShouldNotBeNil)
		})
	})
}

func TestStat_Overview(t *testing.T) {
	convey.Convey("区间总览从分钟桶聚合", t, func() {
		deps := setup(t, nil)
		ctx := context.Background()

		convey.Convey("默认 24 小时", func() {
			deps.rollup.EXPECT().Sum(gomock.Any(), testNow-24*3600, testNow).
				Return(&rollup_entity.Totals{Requests: 100, Hits: 60, BytesServed: 4096, BytesOrigin: 1024}, nil)

			got, err := deps.svc.Overview(ctx, &stat.OverviewRequest{})
			convey.So(err, convey.ShouldBeNil)
			convey.So(got.Range, convey.ShouldEqual, "24h")
			convey.So(got.From, convey.ShouldEqual, testNow-24*3600)
			convey.So(got.To, convey.ShouldEqual, testNow)
			convey.So(got.Requests, convey.ShouldEqual, 100)
			convey.So(got.Hits, convey.ShouldEqual, 60)
			convey.So(got.BytesOrigin, convey.ShouldEqual, 1024)
		})

		convey.Convey("30 天区间", func() {
			deps.rollup.EXPECT().Sum(gomock.Any(), testNow-30*24*3600, testNow).
				Return(&rollup_entity.Totals{Requests: 9}, nil)

			got, err := deps.svc.Overview(ctx, &stat.OverviewRequest{Range: "30d"})
			convey.So(err, convey.ShouldBeNil)
			convey.So(got.Range, convey.ShouldEqual, "30d")
			convey.So(got.Requests, convey.ShouldEqual, 9)
		})
	})
}

func TestStat_ByUpstream(t *testing.T) {
	convey.Convey("按上游聚合，并标出正在降级的上游", t, func() {
		clock := time.Unix(testNow, 0)
		tracker := backoff.New(backoff.Options{
			Threshold: 1, Base: time.Minute, Now: func() time.Time { return clock },
		})
		tracker.Failure("deb.debian.org")
		deps := setup(t, tracker)

		deps.upstream.EXPECT().List(gomock.Any()).Return([]*upstream_entity.Upstream{
			{ID: 7, Host: "deb.debian.org", Enabled: true},
			{ID: 8, Host: "proxy.golang.org", Enabled: true},
		}, nil)
		deps.rollup.EXPECT().SumByUpstream(gomock.Any(), testNow-24*3600, testNow).
			Return([]*rollup_entity.Totals{{UpstreamID: 7, Requests: 100, Hits: 60}}, nil)

		got, err := deps.svc.ByUpstream(context.Background(), &admin.UpstreamStatsRequest{})
		convey.So(err, convey.ShouldBeNil)
		convey.So(len(got.List), convey.ShouldEqual, 2)
		convey.So(got.List[0].Host, convey.ShouldEqual, "deb.debian.org")
		convey.So(got.List[0].Requests, convey.ShouldEqual, 100)
		convey.So(got.List[0].Degraded, convey.ShouldBeTrue)
		convey.So(got.List[0].RetryAt, convey.ShouldEqual, testNow+60)
		// 一次请求都没有的上游也要在矩阵里占一行，否则刚加的上游会凭空消失。
		convey.So(got.List[1].Host, convey.ShouldEqual, "proxy.golang.org")
		convey.So(got.List[1].Requests, convey.ShouldEqual, 0)
		convey.So(got.List[1].Degraded, convey.ShouldBeFalse)
	})
}

func TestStat_Prune(t *testing.T) {
	convey.Convey("裁掉超过保留期的分钟桶", t, func() {
		deps := setup(t, nil)
		// 保留 90 天（数据模型一节）：再往前的分钟桶没人看，留着只会让这张表
		// 无限长下去，而它是每分钟一行乘以上游数。
		deps.rollup.EXPECT().Prune(gomock.Any(), testNow-90*24*3600).Return(int64(42), nil)

		removed, err := deps.svc.Prune(context.Background())
		convey.So(err, convey.ShouldBeNil)
		convey.So(removed, convey.ShouldEqual, 42)
	})
}
