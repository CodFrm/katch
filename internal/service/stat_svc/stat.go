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
	api_upstream "github.com/CodFrm/katch/internal/api/upstream"
	"github.com/CodFrm/katch/internal/metrics"
	"github.com/CodFrm/katch/internal/model/entity/rollup_entity"
	"github.com/CodFrm/katch/internal/proxy/backoff"
	"github.com/CodFrm/katch/internal/repository/cache_repo"
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
	// secondsPerDay 一天有多少秒，逐日序列分桶用。
	secondsPerDay = 86400
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
	// UpstreamSeries 单个上游的按小时时序，供上游详情的命中/回源堆叠图用。
	UpstreamSeries(ctx context.Context, req *admin.UpstreamSeriesRequest) (*admin.UpstreamSeriesResponse, error)
	// PublicUpstreams 每个上游对外可见的命中率与状态，按主机名索引。
	PublicUpstreams(ctx context.Context) (map[string]*UpstreamPublicStat, error)
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
	// 顺带把缓存占用刷到 /metrics 上：katch_cache_objects 与 katch_cache_bytes
	// 是「此刻有多少」，只能按快照给。搭这趟车而不是另起一个定时器，是因为
	// 这里本来就每分钟醒一次，而这两个数的新鲜度要求和分钟桶完全一样。
	s.refreshCacheGauges(ctx)
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
	row.MissFirst += bucket.MissFirst
	row.MissTTL += bucket.MissTTL
	row.MissEvicted += bucket.MissEvicted
	row.MissChanged += bucket.MissChanged
	row.Updatetime = now
	return rollup_repo.TrafficRollup().Save(ctx, row)
}

// refreshCacheGauges 把按上游的缓存对象数与字节数刷成 /metrics 上的两个 gauge。
//
// 查不出来就不刷：留着上一轮的数字，比换成一排 0 更接近事实——0 会让人以为
// 缓存被清空了，而实际上只是这一次没读到。
func (s *statSvc) refreshCacheGauges(ctx context.Context) {
	repo := cache_repo.CacheObject()
	if repo == nil {
		// 缓存目录不可用时 main 根本不装这个仓储，此时没有占用可言。
		return
	}
	sizes, err := repo.SizeByUpstream(ctx)
	if err != nil {
		logger.Ctx(ctx).Error("统计按上游的缓存占用失败", zap.Error(err))
		return
	}
	counts, err := repo.CountByUpstream(ctx)
	if err != nil {
		logger.Ctx(ctx).Error("统计按上游的缓存对象数失败", zap.Error(err))
		return
	}
	list, err := upstream_svc.Upstream().List(ctx, &admin.ListUpstreamsRequest{})
	if err != nil {
		// 指标的标签是主机名，翻不出来就不刷：按 upstream_id 打标签会让
		// /metrics 上出现一串没人认得的数字。
		logger.Ctx(ctx).Error("读取上游列表失败，本轮不刷缓存占用指标", zap.Error(err))
		return
	}
	usage := make([]metrics.CacheUsage, 0, len(list.List))
	for _, item := range list.List {
		// 没缓存过的上游也要给一行 0：它同样是要看的事实，缺行会在图上变成断点。
		usage = append(usage, metrics.CacheUsage{
			Upstream: item.Host,
			Objects:  counts[item.ID],
			Bytes:    sizes[item.ID],
		})
	}
	metrics.SetCacheUsage(usage)
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
	// 缓存量与逐日趋势不随 Range 变：区间问的是「这段时间发生了什么」，
	// 缓存量问的是「现在存着什么」，趋势是侧栏那张固定的 14 天图。
	cacheBytes, err := cache_repo.CacheObject().TotalSize(ctx)
	if err != nil {
		return nil, err
	}
	daily, err := s.daily(ctx)
	if err != nil {
		return nil, err
	}
	return &stat.OverviewResponse{
		Range:        name,
		From:         from,
		To:           to,
		CacheBytes:   cacheBytes,
		Daily:        daily,
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

// UpstreamPublicStat 一个上游对外可见的那部分状态。
//
// 单独一个结构体而不是直接给 admin.UpstreamStatItem：后台那一份带着请求量、
// 回源字节和退避时刻，都是运营数据，整份递给公开列表只要日后谁往里加一个字段
// 就会默认公开出去。
type UpstreamPublicStat struct {
	// HitRate 命中率，0~1。
	HitRate float64
	// CacheBytes 这个上游此刻在缓存里占了多少字节。
	CacheBytes int64
	// Status upstream.StatusNormal 或 upstream.StatusDegraded。
	Status string
}

func (s *statSvc) PublicUpstreams(ctx context.Context) (map[string]*UpstreamPublicStat, error) {
	// 直接走 ByUpstream：后台那张健康矩阵已经把「区间聚合 + 此刻的退避」
	// 算过一遍，这里再算一遍，同一个上游迟早会在首页和后台给出两个说法。
	stats, err := s.ByUpstream(ctx, &admin.UpstreamStatsRequest{})
	if err != nil {
		return nil, err
	}
	// 缓存量一次查回来按 upstream_id 索引：逐个上游问一次，十来个上游就是
	// 十来次查询，而这是匿名就能打的首页。
	sizes, err := cache_repo.CacheObject().SizeByUpstream(ctx)
	if err != nil {
		return nil, err
	}
	ret := make(map[string]*UpstreamPublicStat, len(stats.List))
	for _, item := range stats.List {
		status := api_upstream.StatusNormal
		if item.Degraded {
			status = api_upstream.StatusDegraded
		}
		ret[item.Host] = &UpstreamPublicStat{
			HitRate: hitRate(item.Hits, item.Requests, item.Denied, item.OriginErrors),
			// 没缓存过的上游不在 sizes 里，取零值正是它此刻的缓存量。
			CacheBytes: sizes[item.UpstreamID],
			Status:     status,
		}
	}
	return ret, nil
}

// hitRate 命中率 = hits / (hits + misses)（可观测性一节：比值一律由计数推导）。
//
// 分母里去掉 denied 与 origin_errors：被规则挡住的请求和上游不可达都没走到
// 「缓存里有没有」这个问题上，算进分母会让一次上游故障看起来像缓存变差了。
func hitRate(hits, requests, denied, originErrors int64) float64 {
	lookups := requests - denied - originErrors
	if lookups <= 0 {
		// 一次都没问过缓存时命中率无从谈起，给 0 而不是让它变成 NaN——
		// NaN 序列化成 JSON 会直接让整个响应失败。
		return 0
	}
	return float64(hits) / float64(lookups)
}

// daily 近 DailyDays 天的逐日序列，由旧到新，缺的日子补零。
//
// 从分钟桶聚合而不是另立一张日表：rollup 只有这一张表（决策 16），多一张就
// 多一套要对齐的口径。补零放在服务端：一个刚上线三天的站点，图上应该是 11 个
// 零点加 3 根柱子，而不是 3 个点被拉满整张图。
func (s *statSvc) daily(ctx context.Context) ([]*stat.DailyPoint, error) {
	// 右边界取到今天结束：左闭右开的区间里，今天这一天的桶必须整个落进来。
	to := bucketStart(s.opt.Now().Unix(), secondsPerDay) + secondsPerDay
	rows, err := s.series(ctx, rollup_repo.SeriesQuery{
		From: to - int64(stat.DailyDays)*secondsPerDay, To: to, Width: secondsPerDay,
	})
	if err != nil {
		return nil, err
	}
	points := make([]*stat.DailyPoint, 0, len(rows))
	for _, row := range rows {
		points = append(points, &stat.DailyPoint{
			Day:         row.Bucket,
			Requests:    row.Requests,
			Hits:        row.Hits,
			BytesServed: row.BytesServed,
			BytesOrigin: row.BytesOrigin,
		})
	}
	return points, nil
}

// UpstreamSeries 单个上游的按小时时序。
//
// 只给这一个上游的量，且要密钥：运维的问题是「某个上游怎么了」，而单上游的
// 请求量和回源字节是运营数据，和首页那张站点名片不是一回事。
func (s *statSvc) UpstreamSeries(ctx context.Context, req *admin.UpstreamSeriesRequest) (*admin.UpstreamSeriesResponse, error) {
	name, from, to := s.seriesWindow(req.Range, admin.SeriesBucketSeconds)
	rows, err := s.series(ctx, rollup_repo.SeriesQuery{
		From: from, To: to, Width: admin.SeriesBucketSeconds, UpstreamID: req.UpstreamID,
	})
	if err != nil {
		return nil, err
	}
	points := make([]*admin.UpstreamSeriesPoint, 0, len(rows))
	for _, row := range rows {
		points = append(points, &admin.UpstreamSeriesPoint{
			Bucket:       row.Bucket,
			Requests:     row.Requests,
			Hits:         row.Hits,
			Denied:       row.Denied,
			OriginErrors: row.OriginErrors,
			BytesServed:  row.BytesServed,
			BytesOrigin:  row.BytesOrigin,
			MissFirst:    row.MissFirst,
			MissTTL:      row.MissTTL,
			MissEvicted:  row.MissEvicted,
			MissChanged:  row.MissChanged,
		})
	}
	return &admin.UpstreamSeriesResponse{
		Range: name, From: from, To: to,
		BucketSeconds: admin.SeriesBucketSeconds,
		List:          points,
	}, nil
}

// series 一段等宽时间桶的序列：区间先对齐到桶边界，缺的桶补零，由旧到新。
//
// 逐日和逐小时共用这一段：两者只差一个桶宽和一个上游过滤，各写一遍迟早会在
// 其中一边把补零或者端点对齐写歪，而那种歪只表现为图上少一根柱子。
func (s *statSvc) series(ctx context.Context, q rollup_repo.SeriesQuery) ([]*rollup_repo.SeriesTotals, error) {
	q.From, q.To = alignWindow(q.From, q.To, q.Width)
	rows, err := rollup_repo.TrafficRollup().SumBySeries(ctx, q)
	if err != nil {
		return nil, err
	}
	byBucket := make(map[int64]*rollup_repo.SeriesTotals, len(rows))
	for _, row := range rows {
		byBucket[row.Bucket] = row
	}
	points := make([]*rollup_repo.SeriesTotals, 0, (q.To-q.From)/q.Width)
	for bucket := q.From; bucket < q.To; bucket += q.Width {
		if row, ok := byBucket[bucket]; ok {
			points = append(points, row)
			continue
		}
		// 没有流量的桶也要占一个点：塌掉它会让「这一小时没人拉」在图上
		// 和相邻的有量时段连成一片。
		points = append(points, &rollup_repo.SeriesTotals{Bucket: bucket})
	}
	return points, nil
}

// bucketStart 这一秒所属桶（UTC）的起点。
//
// UTC 而不是进程本地时区：分钟桶本身是 UTC 秒，SQL 那边也是按 UTC 分的组，
// 两边用不同的起点会让序列的边界桶对不上。
func bucketStart(sec, width int64) int64 {
	return sec - sec%width
}

// alignWindow 把区间推到桶边界上：左边界下取整、右边界上取整。
//
// 不对齐的话端点那个桶只盖到一部分——图上第一根柱子会凭空矮一截，而看图的人
// 无从知道那是真的没量还是区间切在了半路。
func alignWindow(from, to, width int64) (int64, int64) {
	if to%width != 0 {
		to = bucketStart(to, width) + width
	}
	return bucketStart(from, width), to
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
	name, seconds := normalizeRange(name)
	to := s.opt.Now().Unix()
	return name, to - seconds, to
}

// seriesWindow 求出时序的区间名与左闭右开边界，两端都落在桶边界上。
//
// 右边界从「此刻所在的那个桶」往后推一格，左边界由它减去区间长度定出来：
// 反过来先取 now-区间 再两端各自对齐，会多出半个桶，24 小时的图上就会冒出
// 第 25 根柱子。换 range 只改这里的区间长度，桶宽是调用方给的常量。
func (s *statSvc) seriesWindow(name string, width int64) (string, int64, int64) {
	name, seconds := normalizeRange(name)
	to := bucketStart(s.opt.Now().Unix(), width) + width
	return name, to - seconds, to
}

// normalizeRange 认区间名，不认得的一律当默认区间。
func normalizeRange(name string) (string, int64) {
	seconds, ok := rangeSeconds[name]
	if !ok {
		return defaultRange, rangeSeconds[defaultRange]
	}
	return name, seconds
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
