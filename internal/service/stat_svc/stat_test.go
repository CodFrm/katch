package stat_svc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"

	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/api/stat"
	api_upstream "github.com/CodFrm/katch/internal/api/upstream"
	"github.com/CodFrm/katch/internal/metrics"
	"github.com/CodFrm/katch/internal/model/entity/rollup_entity"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/backoff"
	"github.com/CodFrm/katch/internal/repository/cache_repo"
	mock_cache_repo "github.com/CodFrm/katch/internal/repository/cache_repo/mock"
	"github.com/CodFrm/katch/internal/repository/rollup_repo"
	mock_rollup_repo "github.com/CodFrm/katch/internal/repository/rollup_repo/mock"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
	mock_upstream_repo "github.com/CodFrm/katch/internal/repository/upstream_repo/mock"
)

// 服务层用 mock 注入 repo，不连库：这一层要验的是「分钟桶怎么攒成一行、
// 区间怎么算、保留期怎么裁」，而不是 SQL。

// 固定时刻：2023-11-14 22:13:20 UTC。
const testNow = int64(1700000000)

// 今天（UTC）的零点与 14 天序列的左边界，按 testNow 算死在这里：
// 序列的分桶必须是 UTC，跟着进程时区走会让同一批数据在两台部署上落到不同的天。
const (
	testToday    = int64(1699920000)
	testDailyDay = testToday - int64(stat.DailyDays-1)*86400
)

type testDeps struct {
	rollup   *mock_rollup_repo.MockTrafficRollupRepo
	upstream *mock_upstream_repo.MockUpstreamRepo
	cache    *mock_cache_repo.MockCacheObjectRepo
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
		cache:    mock_cache_repo.NewMockCacheObjectRepo(ctrl),
	}
	rollup_repo.RegisterTrafficRollup(deps.rollup)
	upstream_repo.RegisterUpstream(deps.upstream)
	cache_repo.RegisterCacheObject(deps.cache)
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
		// Flush 顺带把缓存占用刷成 /metrics 上的两个 gauge（可观测性一节）。
		// 这一组用例验的是分钟桶，所以这里只把那条路放行。
		deps.cache.EXPECT().SizeByUpstream(gomock.Any()).AnyTimes().Return(map[int64]int64{}, nil)
		deps.cache.EXPECT().CountByUpstream(gomock.Any()).AnyTimes().Return(map[int64]int64{}, nil)
		deps.upstream.EXPECT().List(gomock.Any()).AnyTimes().
			Return([]*upstream_entity.Upstream{}, nil)

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
			deps.cache.EXPECT().TotalSize(gomock.Any()).Return(int64(777), nil)
			deps.rollup.EXPECT().SumBySeries(gomock.Any(), rollup_repo.SeriesQuery{
				From: testDailyDay, To: testToday + 86400, Width: 86400,
			}).Return([]*rollup_repo.SeriesTotals{
				{Bucket: testDailyDay, Requests: 10, Hits: 4, BytesServed: 100, BytesOrigin: 90},
				{Bucket: testToday, Requests: 20, Hits: 20, BytesServed: 200, BytesOrigin: 0},
			}, nil)

			got, err := deps.svc.Overview(ctx, &stat.OverviewRequest{})
			convey.So(err, convey.ShouldBeNil)
			convey.So(got.Range, convey.ShouldEqual, "24h")
			convey.So(got.From, convey.ShouldEqual, testNow-24*3600)
			convey.So(got.To, convey.ShouldEqual, testNow)
			convey.So(got.Requests, convey.ShouldEqual, 100)
			convey.So(got.Hits, convey.ShouldEqual, 60)
			convey.So(got.BytesOrigin, convey.ShouldEqual, 1024)
			convey.So(got.CacheBytes, convey.ShouldEqual, 777)

			convey.Convey("逐日序列固定 14 个点，缺的日子补零", func() {
				// 补零放在服务端：一个刚上线三天的站点，图上应该是 11 个零点
				// 加 3 根柱子，而不是 3 个点被拉满整张图。
				convey.So(len(got.Daily), convey.ShouldEqual, 14)
				convey.So(got.Daily[0].Day, convey.ShouldEqual, testDailyDay)
				convey.So(got.Daily[0].Requests, convey.ShouldEqual, 10)
				convey.So(got.Daily[0].Hits, convey.ShouldEqual, 4)
				convey.So(got.Daily[0].BytesServed, convey.ShouldEqual, 100)
				convey.So(got.Daily[0].BytesOrigin, convey.ShouldEqual, 90)
				// 中间那些没有流量的日子也要占一个点，且相邻两点正好差一天。
				convey.So(got.Daily[1].Day, convey.ShouldEqual, testDailyDay+86400)
				convey.So(got.Daily[1].Requests, convey.ShouldEqual, 0)
				convey.So(got.Daily[13].Day, convey.ShouldEqual, testToday)
				convey.So(got.Daily[13].Requests, convey.ShouldEqual, 20)
				convey.So(got.Daily[13].BytesServed, convey.ShouldEqual, 200)
			})
		})

		convey.Convey("30 天区间", func() {
			deps.rollup.EXPECT().Sum(gomock.Any(), testNow-30*24*3600, testNow).
				Return(&rollup_entity.Totals{Requests: 9}, nil)
			deps.cache.EXPECT().TotalSize(gomock.Any()).Return(int64(0), nil)
			// 趋势始终是近 14 天：区间问的是「看多久的合计」，趋势是首页侧栏
			// 那张固定的 14 天图，改 range 不该把它一起改掉。
			deps.rollup.EXPECT().SumBySeries(gomock.Any(), rollup_repo.SeriesQuery{
				From: testDailyDay, To: testToday + 86400, Width: 86400,
			}).Return(nil, nil)

			got, err := deps.svc.Overview(ctx, &stat.OverviewRequest{Range: "30d"})
			convey.So(err, convey.ShouldBeNil)
			convey.So(got.Range, convey.ShouldEqual, "30d")
			convey.So(got.Requests, convey.ShouldEqual, 9)
			convey.So(len(got.Daily), convey.ShouldEqual, 14)
			convey.So(got.Daily[13].Day, convey.ShouldEqual, testToday)
			convey.So(got.Daily[13].Requests, convey.ShouldEqual, 0)
		})
	})
}

func TestStat_PublicUpstreams(t *testing.T) {
	convey.Convey("公开列表要的命中率与状态", t, func() {
		clock := time.Unix(testNow, 0)
		tracker := backoff.New(backoff.Options{
			Threshold: 1, Base: time.Minute, Now: func() time.Time { return clock },
		})
		tracker.Failure("deb.debian.org")
		deps := setup(t, tracker)

		deps.upstream.EXPECT().List(gomock.Any()).Return([]*upstream_entity.Upstream{
			{ID: 7, Host: "deb.debian.org", Enabled: true},
			{ID: 8, Host: "proxy.golang.org", Enabled: true},
			{ID: 9, Host: "quiet.example.com", Enabled: true},
		}, nil)
		deps.rollup.EXPECT().SumByUpstream(gomock.Any(), testNow-24*3600, testNow).
			Return([]*rollup_entity.Totals{
				// 100 次请求里 10 次被规则挡住、10 次上游出错，真正问过缓存的
				// 是 80 次，命中 60 次。
				{UpstreamID: 7, Requests: 100, Hits: 60, Denied: 10, OriginErrors: 10},
				{UpstreamID: 8, Requests: 5, Hits: 0, Denied: 5},
			}, nil)
		// 缓存量按 upstream_id 分组求和，没有缓存的上游干脆不在结果里。
		deps.cache.EXPECT().SizeByUpstream(gomock.Any()).Return(map[int64]int64{7: 4096}, nil)

		got, err := deps.svc.PublicUpstreams(context.Background())
		convey.So(err, convey.ShouldBeNil)
		convey.So(len(got), convey.ShouldEqual, 3)

		convey.Convey("命中率是 hits/(hits+misses)，被拒和回源失败不进分母", func() {
			// 分母算进 denied 与 origin_errors，会让一次上游故障看起来像
			// 缓存变差了，而那两件事根本没走到「缓存里有没有」这个问题上。
			convey.So(got["deb.debian.org"].HitRate, convey.ShouldAlmostEqual, 0.75, 1e-9)
		})

		convey.Convey("分母为零时是 0 而不是除零", func() {
			// 全被规则挡住的上游：真正问过缓存的次数是 0。
			convey.So(got["proxy.golang.org"].HitRate, convey.ShouldEqual, 0)
			// 一次请求都没有的上游也要有一行，否则刚加的上游在页脚表里没有状态。
			convey.So(got["quiet.example.com"].HitRate, convey.ShouldEqual, 0)
		})

		convey.Convey("缓存量按上游给出，没缓存过的是 0", func() {
			convey.So(got["deb.debian.org"].CacheBytes, convey.ShouldEqual, 4096)
			convey.So(got["proxy.golang.org"].CacheBytes, convey.ShouldEqual, 0)
			convey.So(got["quiet.example.com"].CacheBytes, convey.ShouldEqual, 0)
		})

		convey.Convey("状态取的是此刻的退避，与后台那张矩阵同一套", func() {
			convey.So(got["deb.debian.org"].Status, convey.ShouldEqual, api_upstream.StatusDegraded)
			convey.So(got["proxy.golang.org"].Status, convey.ShouldEqual, api_upstream.StatusNormal)
			convey.So(got["quiet.example.com"].Status, convey.ShouldEqual, api_upstream.StatusNormal)
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

// testHour 系列：testNow 落在整点之后的第 800 秒，序列的端点必须被推到桶边界上。
const (
	testHourStart  = int64(1699999200)
	testSeriesTo   = testHourStart + 3600
	testSeriesFrom = testSeriesTo - 24*3600
)

func TestStat_UpstreamSeries(t *testing.T) {
	convey.Convey("单个上游的按小时时序", t, func() {
		deps := setup(t, nil)
		ctx := context.Background()

		convey.Convey("24 小时是 24 个小时桶，缺的小时补零", func() {
			deps.rollup.EXPECT().SumBySeries(gomock.Any(), rollup_repo.SeriesQuery{
				From: testSeriesFrom, To: testSeriesTo, Width: 3600, UpstreamID: 7,
			}).Return([]*rollup_repo.SeriesTotals{
				{Bucket: testSeriesFrom, Requests: 10, Hits: 6, Denied: 1, OriginErrors: 1, BytesServed: 100, BytesOrigin: 40},
				// 中间整整一段没有流量：补零而不是塌成相邻的两根柱子，
				// 否则图上「凌晨三点没人拉」会看起来像「一直有量」。
				{Bucket: testSeriesFrom + 5*3600, Requests: 4, Hits: 4, BytesServed: 50},
				{Bucket: testHourStart, Requests: 2, Hits: 0, OriginErrors: 2},
			}, nil)

			got, err := deps.svc.UpstreamSeries(ctx, &admin.UpstreamSeriesRequest{UpstreamID: 7})
			convey.So(err, convey.ShouldBeNil)
			convey.So(got.Range, convey.ShouldEqual, "24h")
			convey.So(got.BucketSeconds, convey.ShouldEqual, 3600)
			convey.So(len(got.List), convey.ShouldEqual, 24)
			// 六个计数都带上：界面要画命中/回源的堆叠柱，少一个就得再打一次接口。
			convey.So(got.List[0].Bucket, convey.ShouldEqual, testSeriesFrom)
			convey.So(got.List[0].Requests, convey.ShouldEqual, 10)
			convey.So(got.List[0].Hits, convey.ShouldEqual, 6)
			convey.So(got.List[0].Denied, convey.ShouldEqual, 1)
			convey.So(got.List[0].OriginErrors, convey.ShouldEqual, 1)
			convey.So(got.List[0].BytesServed, convey.ShouldEqual, 100)
			convey.So(got.List[0].BytesOrigin, convey.ShouldEqual, 40)
			// 中间的空洞各占一个点，且相邻两点正好差一小时。
			for i := 1; i < 5; i++ {
				convey.So(got.List[i].Bucket, convey.ShouldEqual, testSeriesFrom+int64(i)*3600)
				convey.So(got.List[i].Requests, convey.ShouldEqual, 0)
				convey.So(got.List[i].BytesServed, convey.ShouldEqual, 0)
			}
			convey.So(got.List[5].Requests, convey.ShouldEqual, 4)
			// 由旧到新，最后一个点是此刻所在的那个（还没走完的）小时。
			convey.So(got.List[23].Bucket, convey.ShouldEqual, testHourStart)
			convey.So(got.List[23].OriginErrors, convey.ShouldEqual, 2)
		})

		convey.Convey("区间端点落在小时中间时被推到桶边界上", func() {
			// testNow 是 22:13:20：直接拿 now-24h 当左边界，第一个桶只会盖到
			// 那一小时的后 46 分钟，图上第一根柱子会凭空矮一截。
			deps.rollup.EXPECT().SumBySeries(gomock.Any(), rollup_repo.SeriesQuery{
				From: testSeriesFrom, To: testSeriesTo, Width: 3600, UpstreamID: 7,
			}).Return(nil, nil)

			got, err := deps.svc.UpstreamSeries(ctx, &admin.UpstreamSeriesRequest{UpstreamID: 7})
			convey.So(err, convey.ShouldBeNil)
			convey.So(testNow%3600, convey.ShouldNotEqual, 0)
			convey.So(got.From%3600, convey.ShouldEqual, 0)
			convey.So(got.To%3600, convey.ShouldEqual, 0)
			convey.So(got.From, convey.ShouldEqual, testSeriesFrom)
			// 右边界上取整：此刻所在的这个小时要整个落进来，否则界面上最新的
			// 那根柱子要等到下一个整点才出现。
			convey.So(got.To, convey.ShouldEqual, testSeriesTo)
			convey.So(len(got.List), convey.ShouldEqual, 24)
			convey.So(got.List[0].Requests, convey.ShouldEqual, 0)
		})

		convey.Convey("换 range 只把窗口拉长，桶宽还是一小时", func() {
			deps.rollup.EXPECT().SumBySeries(gomock.Any(), rollup_repo.SeriesQuery{
				From: testSeriesTo - 7*24*3600, To: testSeriesTo, Width: 3600, UpstreamID: 8,
			}).Return([]*rollup_repo.SeriesTotals{{Bucket: testHourStart, Requests: 3}}, nil)

			got, err := deps.svc.UpstreamSeries(ctx, &admin.UpstreamSeriesRequest{UpstreamID: 8, Range: "7d"})
			convey.So(err, convey.ShouldBeNil)
			convey.So(got.Range, convey.ShouldEqual, "7d")
			convey.So(got.BucketSeconds, convey.ShouldEqual, 3600)
			convey.So(len(got.List), convey.ShouldEqual, 7*24)
			convey.So(got.List[1].Bucket-got.List[0].Bucket, convey.ShouldEqual, 3600)
			convey.So(got.List[7*24-1].Bucket, convey.ShouldEqual, testHourStart)
			convey.So(got.List[7*24-1].Requests, convey.ShouldEqual, 3)
		})

		convey.Convey("查库出错原样抛给调用方", func() {
			deps.rollup.EXPECT().SumBySeries(gomock.Any(), gomock.Any()).Return(nil, errors.New("db down"))

			_, err := deps.svc.UpstreamSeries(ctx, &admin.UpstreamSeriesRequest{UpstreamID: 7})
			convey.So(err, convey.ShouldNotBeNil)
		})
	})
}

// TestStat_FlushRefreshesCacheGauges 缓存占用要按**主机名**出现在 /metrics 上。
//
// 标签是主机名而不是 upstream_id：/metrics 是给人和采集方看的，一串数字 id
// 在那里没人认得。没缓存过的上游也要给一行 0——缺行会在图上变成断点，而
// 「这个上游此刻一个对象都没缓存」本身就是要看的事实。
func TestStat_FlushRefreshesCacheGauges(t *testing.T) {
	convey.Convey("落分钟桶时顺带刷新缓存占用指标", t, func() {
		deps := setup(t, nil)
		deps.cache.EXPECT().SizeByUpstream(gomock.Any()).
			Return(map[int64]int64{7: 4096}, nil)
		deps.cache.EXPECT().CountByUpstream(gomock.Any()).
			Return(map[int64]int64{7: 3}, nil)
		deps.upstream.EXPECT().List(gomock.Any()).Return([]*upstream_entity.Upstream{
			{ID: 7, Host: "deb.debian.org", Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic}, Enabled: true},
			{ID: 8, Host: "quiet.example.com", Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic}, Enabled: true},
		}, nil)

		convey.So(deps.svc.Flush(context.Background()), convey.ShouldBeNil)

		got := scrapeDefault()
		convey.So(got, convey.ShouldContainSubstring,
			`katch_cache_objects{upstream="deb.debian.org"} 3`)
		convey.So(got, convey.ShouldContainSubstring,
			`katch_cache_bytes{upstream="deb.debian.org"} 4096`)
		convey.So(got, convey.ShouldContainSubstring,
			`katch_cache_objects{upstream="quiet.example.com"} 0`)
	})
}

// scrapeDefault 按 Prometheus 抓取端点的形态取一次进程级指标。
//
// 走默认 registry 而不是另起一个：stat_svc 刷的就是进程级那一份计数器
// （metrics.SetCacheUsage），而 component.Core() 暴露的 /metrics 正是从那里收集的。
func scrapeDefault() string {
	w := httptest.NewRecorder()
	promhttp.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return w.Body.String()
}

// TestStat_FlushCarriesMissReasons 四个回源原因跟着分钟桶一起落进 traffic_rollup。
//
// 和别的计数一样是累加而不是覆盖：一分钟内可能落库两次（进程重启、或上一次落库
// 慢了半拍），覆盖会把前半分钟的归因整个抹掉，而占比条读的正是这几列。
func TestStat_FlushCarriesMissReasons(t *testing.T) {
	convey.Convey("回源原因跟着分钟桶累加进同一行", t, func() {
		deps := setup(t, nil)
		ctx := context.Background()
		deps.cache.EXPECT().SizeByUpstream(gomock.Any()).AnyTimes().Return(map[int64]int64{}, nil)
		deps.cache.EXPECT().CountByUpstream(gomock.Any()).AnyTimes().Return(map[int64]int64{}, nil)
		deps.upstream.EXPECT().List(gomock.Any()).AnyTimes().Return([]*upstream_entity.Upstream{}, nil)

		deps.drained = []metrics.Bucket{{
			Host: "deb.debian.org", Bucket: 1700000040,
			Requests: 10, Hits: 4, Denied: 1, OriginErrors: 1,
			MissFirst: 2, MissTTL: 1, MissEvicted: 1, MissChanged: 0,
		}}
		deps.upstream.EXPECT().FindByHost(gomock.Any(), "deb.debian.org").
			Return(&upstream_entity.Upstream{ID: 7, Host: "deb.debian.org", Enabled: true}, nil)
		deps.rollup.EXPECT().FindByBucket(gomock.Any(), int64(7), int64(1700000040)).
			Return(&rollup_entity.TrafficRollup{
				ID: 3, UpstreamID: 7, Bucket: 1700000040,
				Requests: 5, MissFirst: 1, MissTTL: 2, MissEvicted: 3, MissChanged: 4,
			}, nil)
		var saved *rollup_entity.TrafficRollup
		deps.rollup.EXPECT().Save(gomock.Any(), gomock.Any()).
			DoAndReturn(func(_ context.Context, row *rollup_entity.TrafficRollup) error {
				saved = row
				return nil
			})

		convey.So(deps.svc.Flush(ctx), convey.ShouldBeNil)
		convey.So(saved.MissFirst, convey.ShouldEqual, 3)
		convey.So(saved.MissTTL, convey.ShouldEqual, 3)
		convey.So(saved.MissEvicted, convey.ShouldEqual, 4)
		convey.So(saved.MissChanged, convey.ShouldEqual, 4)
	})
}

// TestStat_UpstreamSeriesCarriesMissReasons 上游详情的回源原因分解和堆叠图读同一份序列。
//
// 不为占比条另开一个端点：同一屏上的两个数来自两次请求，就会来自两个时刻，
// 然后对不上（UpstreamSeriesPoint 的注释说的就是这件事）。
func TestStat_UpstreamSeriesCarriesMissReasons(t *testing.T) {
	convey.Convey("按小时序列带上四个回源原因", t, func() {
		deps := setup(t, nil)
		deps.rollup.EXPECT().SumBySeries(gomock.Any(), rollup_repo.SeriesQuery{
			From: testSeriesFrom, To: testSeriesTo, Width: 3600, UpstreamID: 7,
		}).Return([]*rollup_repo.SeriesTotals{
			{Bucket: testHourStart, Requests: 10, Hits: 4, Denied: 1, OriginErrors: 1,
				MissFirst: 2, MissTTL: 1, MissEvicted: 1, MissChanged: 0},
		}, nil)

		got, err := deps.svc.UpstreamSeries(context.Background(), &admin.UpstreamSeriesRequest{UpstreamID: 7})
		convey.So(err, convey.ShouldBeNil)
		last := got.List[len(got.List)-1]
		convey.So(last.Bucket, convey.ShouldEqual, testHourStart)
		convey.So(last.MissFirst, convey.ShouldEqual, 2)
		convey.So(last.MissTTL, convey.ShouldEqual, 1)
		convey.So(last.MissEvicted, convey.ShouldEqual, 1)
		convey.So(last.MissChanged, convey.ShouldEqual, 0)
		// 四项之和正好是这一小时的未命中数：界面按这个分母算占比。
		convey.So(last.MissFirst+last.MissTTL+last.MissEvicted+last.MissChanged,
			convey.ShouldEqual, last.Requests-last.Hits-last.Denied-last.OriginErrors)
		// 补零的桶四项都是零，不是「没有这个字段」。
		convey.So(got.List[0].MissFirst, convey.ShouldEqual, 0)
	})
}

// TestStat_CloseHandleFlushesOnExit 退出前落一次：Run 的 ctx.Done 分支只 return，
// 那一批由 CloseHandle 落（失败与降级一节）。周期设成一小时，中途一次 tick 都不会响。
func TestStat_CloseHandleFlushesOnExit(t *testing.T) {
	deps := setup(t, nil)
	svc := New(Options{
		Now:           func() time.Time { return time.Unix(testNow, 0) },
		Drainer:       deps,
		FlushInterval: time.Hour,
		PruneInterval: time.Hour,
	})
	// Flush 顺带刷缓存占用指标，这一组用例验的是退出落库，所以把那条路放行。
	deps.cache.EXPECT().SizeByUpstream(gomock.Any()).AnyTimes().Return(map[int64]int64{}, nil)
	deps.cache.EXPECT().CountByUpstream(gomock.Any()).AnyTimes().Return(map[int64]int64{}, nil)
	deps.upstream.EXPECT().List(gomock.Any()).AnyTimes().
		Return([]*upstream_entity.Upstream{}, nil)

	deps.drained = []metrics.Bucket{{
		Host: "deb.debian.org", Bucket: 1700000040, Requests: 10, Hits: 6,
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := svc.Start(ctx, nil); err != nil {
		t.Fatalf("Start 失败：%v", err)
	}
	cancel()
	// 等循环真的退出：此刻还不该有落库，否则就是退出落库没等循环退出。
	<-svc.(*statSvc).runDone
	if saved != nil {
		t.Fatalf("循环退出时就落库了，退出那批应由 CloseHandle 落：%+v", saved)
	}

	svc.CloseHandle()

	if saved == nil || saved.UpstreamID != 7 || saved.Requests != 10 || saved.Hits != 6 {
		t.Fatalf("CloseHandle 没有把计数器落库：%+v", saved)
	}
}

// TestStat_CloseHandleBoundsTheExitFlush 退出落库有上限（失败与降级一节）。
//
// main 里缓存那一趟收尾挂了 cacheDrainTimeout，这里是同一个理由：一个不响应的库
// 不该把退出拖到只剩 SIGKILL。用例把落库卡在 ctx 上——只有 ctx 带截止时刻才能
// 把它放走，所以 CloseHandle 能在上限内返回就说明它真的给自己留了那条退路。
func TestStat_CloseHandleBoundsTheExitFlush(t *testing.T) {
	deps := setup(t, nil)
	svc := New(Options{
		Now:              func() time.Time { return time.Unix(testNow, 0) },
		Drainer:          deps,
		FlushInterval:    time.Hour,
		PruneInterval:    time.Hour,
		ExitFlushTimeout: 100 * time.Millisecond,
	})
	// Flush 顺带刷缓存占用指标，这一组用例验的是退出落库，所以把那条路放行。
	deps.cache.EXPECT().SizeByUpstream(gomock.Any()).AnyTimes().Return(map[int64]int64{}, nil)
	deps.cache.EXPECT().CountByUpstream(gomock.Any()).AnyTimes().Return(map[int64]int64{}, nil)
	deps.upstream.EXPECT().List(gomock.Any()).AnyTimes().
		Return([]*upstream_entity.Upstream{}, nil)

	deps.drained = []metrics.Bucket{{
		Host: "deb.debian.org", Bucket: 1700000040, Requests: 10, Hits: 6,
	}}
	deps.upstream.EXPECT().FindByHost(gomock.Any(), "deb.debian.org").
		Return(&upstream_entity.Upstream{ID: 7, Host: "deb.debian.org", Enabled: true}, nil)
	deps.rollup.EXPECT().FindByBucket(gomock.Any(), int64(7), int64(1700000040)).Return(nil, nil)
	deps.rollup.EXPECT().Save(gomock.Any(), gomock.Any()).
		DoAndReturn(func(ctx context.Context, _ *rollup_entity.TrafficRollup) error {
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

// stuckDrainer 第一趟 Drain 永远不返回（卡住的库/驱动连取消都不理），
// 之后的趟数照常给空。
type stuckDrainer struct {
	entered chan struct{}
	unblock chan struct{}
	n       atomic.Int32
}

func (d *stuckDrainer) Drain() []metrics.Bucket {
	if d.n.Add(1) == 1 {
		close(d.entered)
		<-d.unblock
	}
	return nil
}

// TestStat_CloseHandleBoundsTheLoopWait 循环卡住时退出也不能被挂住。
//
// 等循环退出同样有上限：这一趟落库卡在 Drain 里，取消也拉不出来，CloseHandle
// 不能跟它一起等下去。
func TestStat_CloseHandleBoundsTheLoopWait(t *testing.T) {
	deps := setup(t, nil)
	stuck := &stuckDrainer{entered: make(chan struct{}), unblock: make(chan struct{})}
	svc := New(Options{
		Now:              func() time.Time { return time.Unix(testNow, 0) },
		Drainer:          stuck,
		FlushInterval:    10 * time.Millisecond,
		PruneInterval:    time.Hour,
		ExitFlushTimeout: 100 * time.Millisecond,
	})
	// Flush 顺带刷缓存占用指标，这一组用例验的是退出，所以把那条路放行。
	deps.cache.EXPECT().SizeByUpstream(gomock.Any()).AnyTimes().Return(map[int64]int64{}, nil)
	deps.cache.EXPECT().CountByUpstream(gomock.Any()).AnyTimes().Return(map[int64]int64{}, nil)
	deps.upstream.EXPECT().List(gomock.Any()).AnyTimes().
		Return([]*upstream_entity.Upstream{}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := svc.Start(ctx, nil); err != nil {
		t.Fatalf("Start 失败：%v", err)
	}
	// 等循环真的进到那一趟落库里再取消：它此刻卡在 Drain 上。
	select {
	case <-stuck.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("循环没有按周期进到落库里")
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

	// 放走卡住的循环再收工：不让它在收尾阶段还停在探针里。
	close(stuck.unblock)
	<-svc.(*statSvc).runDone
}

// TestStat_CloseHandleWithoutStart Start 没跑过时 CloseHandle 是空操作。
//
// 没有循环要等，也不该凭空去碰库：这一组用例一个 mock 预期都没给，真的落了库会
// 因为非预期调用当场失败。
func TestStat_CloseHandleWithoutStart(t *testing.T) {
	deps := setup(t, nil)
	svc := New(Options{
		Now:           func() time.Time { return time.Unix(testNow, 0) },
		Drainer:       deps,
		FlushInterval: time.Hour,
		PruneInterval: time.Hour,
	})
	deps.drained = []metrics.Bucket{{Host: "deb.debian.org", Bucket: 1700000040, Requests: 1}}

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

// TestStat_CloseHandleTwice CloseHandle 被调两次：第二次面对的是空计数器，
// 不重复落库也不阻塞（testDeps.Drain 取走即清空，和 metrics 的约定一致）。
func TestStat_CloseHandleTwice(t *testing.T) {
	deps := setup(t, nil)
	svc := New(Options{
		Now:              func() time.Time { return time.Unix(testNow, 0) },
		Drainer:          deps,
		FlushInterval:    time.Hour,
		PruneInterval:    time.Hour,
		ExitFlushTimeout: time.Second,
	})
	deps.cache.EXPECT().SizeByUpstream(gomock.Any()).AnyTimes().Return(map[int64]int64{}, nil)
	deps.cache.EXPECT().CountByUpstream(gomock.Any()).AnyTimes().Return(map[int64]int64{}, nil)
	deps.upstream.EXPECT().List(gomock.Any()).AnyTimes().
		Return([]*upstream_entity.Upstream{}, nil)

	deps.drained = []metrics.Bucket{{
		Host: "deb.debian.org", Bucket: 1700000040, Requests: 10, Hits: 6,
	}}
	// 各 Times(1)：第二次 CloseHandle 再去查上游或写库都会当场失败。
	deps.upstream.EXPECT().FindByHost(gomock.Any(), "deb.debian.org").
		Return(&upstream_entity.Upstream{ID: 7, Host: "deb.debian.org", Enabled: true}, nil)
	deps.rollup.EXPECT().FindByBucket(gomock.Any(), int64(7), int64(1700000040)).Return(nil, nil)
	deps.rollup.EXPECT().Save(gomock.Any(), gomock.Any()).Return(nil)

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
