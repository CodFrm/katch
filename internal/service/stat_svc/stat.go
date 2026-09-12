// Package stat_svc 把进程内的拉取计数变成界面上看得见的统计。
//
// 三件事：每分钟把计数器落成 traffic_rollup 的一行、按区间聚合出总览、
// 裁掉超过保留期的行。界面上的请求量和命中率都从这张表来，不依赖外部的
// Prometheus（决策 16）——一个自部署的镜像站不该为了看自己的命中率就要先
// 搭一套监控。
package stat_svc

import (
	"context"
	"errors"
	"time"

	"github.com/cago-frame/cago/pkg/logger"
	"go.uber.org/zap"

	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/api/stat"
	"github.com/CodFrm/katch/internal/metrics"
	"github.com/CodFrm/katch/internal/model/entity/rollup_entity"
	"github.com/CodFrm/katch/internal/proxy/backoff"
	"github.com/CodFrm/katch/internal/repository/rollup_repo"
	"github.com/CodFrm/katch/internal/service/upstream_svc"
)

const (
	// defaultRange 请求没指定区间时按 24 小时算。
	defaultRange = "24h"
	// defaultRetentionDays 分钟桶保留多久（数据模型一节）。
	defaultRetentionDays = 90
	// defaultFlushInterval 多久把进程内计数器落一次库。
	//
	// 和桶的粒度一致：落得更密只会让同一分钟的行被反复读出来改回去，
	// 落得更疏则一次重启会丢掉更多还没落库的量。
	defaultFlushInterval = time.Minute
	// defaultPruneInterval 多久裁一次保留期外的行。保留期是 90 天，
	// 每小时裁一次已经远远够用。
	defaultPruneInterval = time.Hour
)

// rangeSeconds 界面上可选的三个区间。
//
// 只认这三个而不是任意时长：区间是枚举，接口层已经用 oneof 挡过一次，
// 这里的兜底保证服务层被直接调用时也不会算出一个负数区间。
var rangeSeconds = map[string]int64{
	"24h": 24 * 3600,
	"7d":  7 * 24 * 3600,
	"30d": 30 * 24 * 3600,
}

// Drainer 取走进程内计数器攒下的分钟桶。取走即清零。
type Drainer interface {
	Drain() []metrics.Bucket
}

// DegradeReporter 报告哪些上游正处在回源退避里。
//
// 这是内存态（决策 17），不来自 traffic_rollup，也不该被写进去：
// 退避说的是「此刻」，落库只会把一段早已过期的状态带到下一个进程。
type DegradeReporter interface {
	Snapshot() []backoff.Status
}

// Options 统计层的运行参数。
type Options struct {
	// Now 取当前时间，用例注入假时钟用。
	Now func() time.Time
	// Drainer 计数器来源，nil 表示进程级的那一个。
	Drainer Drainer
	// Degraded 退避状态来源，nil 表示不报告降级。
	Degraded DegradeReporter
	// RetentionDays 分钟桶保留天数，0 按 90 天。
	RetentionDays int
	// FlushInterval、PruneInterval 两个定时任务的周期，0 按默认值。
	FlushInterval time.Duration
	PruneInterval time.Duration
}

// StatSvc 统计的业务操作。
type StatSvc interface {
	// Overview 全站区间总览，供公开的首页用。
	Overview(ctx context.Context, req *stat.OverviewRequest) (*stat.OverviewResponse, error)
	// ByUpstream 按上游的区间统计，附带此刻的降级状态。
	ByUpstream(ctx context.Context, req *admin.UpstreamStatsRequest) (*admin.UpstreamStatsResponse, error)
	// Flush 把进程内计数器攒下的分钟桶落库。
	Flush(ctx context.Context) error
	// Prune 裁掉超过保留期的分钟桶，返回裁掉多少行。
	Prune(ctx context.Context) (int64, error)
	// Run 跑定时的落库与裁剪，直到 ctx 结束。
	Run(ctx context.Context)
}

type statSvc struct {
	opt Options
}

// New 构造统计层。
func New(opt Options) StatSvc {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.RetentionDays <= 0 {
		opt.RetentionDays = defaultRetentionDays
	}
	if opt.FlushInterval <= 0 {
		opt.FlushInterval = defaultFlushInterval
	}
	if opt.PruneInterval <= 0 {
		opt.PruneInterval = defaultPruneInterval
	}
	return &statSvc{opt: opt}
}

var defaultStat = New(Options{})

// Stat 返回统计业务层。
func Stat() StatSvc {
	return defaultStat
}

// Register 注册实现，由 main 装配。
func Register(svc StatSvc) {
	defaultStat = svc
}

// drainer 计数器来源。默认延迟到调用时才取，包初始化时就去碰全局
// registry 会让「导入这个包」变成一次注册指标的副作用。
func (s *statSvc) drainer() Drainer {
	if s.opt.Drainer != nil {
		return s.opt.Drainer
	}
	return metrics.Default()
}

func (s *statSvc) Flush(ctx context.Context) error {
	now := s.opt.Now().Unix()
	var failures []error
	for _, bucket := range s.drainer().Drain() {
		if err := s.saveBucket(ctx, now, bucket); err != nil {
			// 记下来继续跑下一个上游：统计坏掉不该连累别的上游的统计，
			// 更不该让整轮 flush 停在第一个错误上。
			logger.Ctx(ctx).Error("落分钟桶失败", zap.String("host", bucket.Host),
				zap.Int64("bucket", bucket.Bucket), zap.Error(err))
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (s *statSvc) saveBucket(ctx context.Context, now int64, bucket metrics.Bucket) error {
	upstream, err := upstream_svc.Upstream().FindByHost(ctx, bucket.Host)
	if err != nil {
		return err
	}
	if upstream == nil {
		// 上游被删了或被停用了：这一分钟的量没有归属，丢掉即可。
		// 按 upstream_id 存的表里没有它的位置，硬塞一个 0 只会污染聚合。
		return nil
	}
	row, err := rollup_repo.TrafficRollup().FindByBucket(ctx, upstream.ID, bucket.Bucket)
	if err != nil {
		return err
	}
	if row == nil {
		row = &rollup_entity.TrafficRollup{
			UpstreamID: upstream.ID,
			Bucket:     bucket.Bucket,
			Createtime: now,
		}
	}
	// 累加而不是覆盖：一分钟内可能落库两次（进程重启、或上一次落库慢了半拍），
	// 覆盖会把前半分钟的量整个抹掉。
	row.Requests += bucket.Requests
	row.Hits += bucket.Hits
	row.Denied += bucket.Denied
	row.OriginErrors += bucket.OriginErrors
	row.BytesServed += bucket.BytesServed
	row.BytesOrigin += bucket.BytesOrigin
	row.Updatetime = now
	return rollup_repo.TrafficRollup().Save(ctx, row)
}

func (s *statSvc) Prune(ctx context.Context) (int64, error) {
	before := s.opt.Now().Unix() - int64(s.opt.RetentionDays)*24*3600
	return rollup_repo.TrafficRollup().Prune(ctx, before)
}

func (s *statSvc) Overview(ctx context.Context, req *stat.OverviewRequest) (*stat.OverviewResponse, error) {
	name, from, to := s.window(req.Range)
	totals, err := rollup_repo.TrafficRollup().Sum(ctx, from, to)
	if err != nil {
		return nil, err
	}
	return &stat.OverviewResponse{
		Range:        name,
		From:         from,
		To:           to,
		Requests:     totals.Requests,
		Hits:         totals.Hits,
		Denied:       totals.Denied,
		OriginErrors: totals.OriginErrors,
		BytesServed:  totals.BytesServed,
		BytesOrigin:  totals.BytesOrigin,
	}, nil
}

func (s *statSvc) ByUpstream(ctx context.Context, req *admin.UpstreamStatsRequest) (*admin.UpstreamStatsResponse, error) {
	name, from, to := s.window(req.Range)
	list, err := upstream_svc.Upstream().List(ctx, &admin.ListUpstreamsRequest{})
	if err != nil {
		return nil, err
	}
	totals, err := rollup_repo.TrafficRollup().SumByUpstream(ctx, from, to)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]*rollup_entity.Totals, len(totals))
	for _, t := range totals {
		byID[t.UpstreamID] = t
	}
	degraded := s.degraded()
	resp := &admin.UpstreamStatsResponse{
		Range: name, From: from, To: to,
		List: make([]*admin.UpstreamStatItem, 0, len(list.List)),
	}
	// 以上游表为准列出每一行：只回有流量的行，会让刚加的上游在健康矩阵里
	// 凭空消失，而「一次请求都没有」本身就是要看的信息。
	for _, upstream := range list.List {
		item := &admin.UpstreamStatItem{UpstreamID: upstream.ID, Host: upstream.Host}
		if t, ok := byID[upstream.ID]; ok {
			item.Requests = t.Requests
			item.Hits = t.Hits
			item.Denied = t.Denied
			item.OriginErrors = t.OriginErrors
			item.BytesServed = t.BytesServed
			item.BytesOrigin = t.BytesOrigin
		}
		if status, ok := degraded[upstream.Host]; ok {
			item.Degraded = true
			item.RetryAt = status.RetryAt
		}
		resp.List = append(resp.List, item)
	}
	return resp, nil
}

// degraded 此刻降级中的上游，按主机名索引。
func (s *statSvc) degraded() map[string]backoff.Status {
	if s.opt.Degraded == nil {
		return nil
	}
	snapshot := s.opt.Degraded.Snapshot()
	ret := make(map[string]backoff.Status, len(snapshot))
	for _, status := range snapshot {
		ret[status.Host] = status
	}
	return ret
}

// window 求出这次聚合的区间名与左闭右开边界。
func (s *statSvc) window(name string) (string, int64, int64) {
	seconds, ok := rangeSeconds[name]
	if !ok {
		name = defaultRange
		seconds = rangeSeconds[defaultRange]
	}
	to := s.opt.Now().Unix()
	return name, to - seconds, to
}

// Run 跑定时的落库与裁剪。
//
// 用 ticker 而不是 cron 组件：cron 要配置文件里有对应的段，而落库周期是
// 「进程跑起来就该有」的东西，不该多一个配不好就不统计的开关。
func (s *statSvc) Run(ctx context.Context) {
	flush := time.NewTicker(s.opt.FlushInterval)
	defer flush.Stop()
	prune := time.NewTicker(s.opt.PruneInterval)
	defer prune.Stop()
	for {
		select {
		case <-ctx.Done():
			// 退出前再落一次：否则最后不到一分钟的量会随进程一起消失。
			// 这里不能再用已经取消的 ctx，落库会当场失败。
			if err := s.Flush(context.WithoutCancel(ctx)); err != nil {
				logger.Ctx(ctx).Error("退出前落分钟桶失败", zap.Error(err))
			}
			return
		case <-flush.C:
			if err := s.Flush(ctx); err != nil {
				logger.Ctx(ctx).Error("落分钟桶失败", zap.Error(err))
			}
		case <-prune.C:
			removed, err := s.Prune(ctx)
			if err != nil {
				logger.Ctx(ctx).Error("裁剪分钟桶失败", zap.Error(err))
				continue
			}
			if removed > 0 {
				logger.Ctx(ctx).Info("裁掉超保留期的分钟桶", zap.Int64("removed", removed))
			}
		}
	}
}
