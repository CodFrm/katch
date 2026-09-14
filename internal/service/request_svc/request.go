// Package request_svc 是「最近请求」明细的业务层。
//
// 写路径：把 metrics 中间件攒在进程内环形缓冲里的拉取记录每秒取走一批，
// 主机名翻成 upstream_id 后落进 recent_request，并按设置里的保留期裁剪。
// 热路径上一次拉取只付一次内存写入，库写不进去只是面板少几秒的量
// （记录与落库、失败与降级两节）。
//
// 读路径：上游详情那块面板按 upstream_id 取最近若干条（recent_requests.go），
// 与日志文件的大小、轮转、是否落盘全部解耦（决策 2/11）。
package request_svc

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/cago-frame/cago"
	"github.com/cago-frame/cago/configs"
	"github.com/cago-frame/cago/pkg/logger"
	"go.uber.org/zap"

	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/metrics"
	"github.com/CodFrm/katch/internal/model/entity/request_log_entity"
	"github.com/CodFrm/katch/internal/repository/request_log_repo"
	"github.com/CodFrm/katch/internal/service/setting_svc"
	"github.com/CodFrm/katch/internal/service/upstream_svc"
)

const (
	// defaultFlushInterval 多久把缓冲区取走一批落库。
	//
	// 1 秒是面板滞后上限（已知代价），也是热路径只做一次内存 append 能换到的
	// 最小滞后：落得更疏，「刚刚发生了什么」会变成「几秒前发生了什么」。
	defaultFlushInterval = time.Second
	// defaultPruneInterval 多久裁一次保留期外的行。保留期以小时计，
	// 每小时裁一次已经远远够用。
	defaultPruneInterval = time.Hour
	// errorLogInterval 连续失败时那条 error 的节流间隔（决策 9）。
	//
	// 库短暂不可用是可接受的事件，但一个不可用的库不该顺带把日志刷爆。
	errorLogInterval = time.Minute
	// flushErrorMessage 落库失败那一条 error 的 msg。用例按它数日志条数。
	flushErrorMessage = "最近请求落库失败"
	// defaultExitFlushTimeout 退出时落最后一批的上限。
	//
	// 和 cmd/katch/main.go 里缓存那一趟收尾（cacheDrainTimeout）同一个理由：一个
	// 卡住的库不该把退出拖到天荒地老。正常收尾只差一条 insert，是毫秒级的事；
	// 等满这个上限，说明库真的不响应了，那就记一条日志然后走人。
	defaultExitFlushTimeout = 30 * time.Second
	// noUpstreamID 标记「这个主机名在本批里查过、没有归属」。
	//
	// 用 -1 而不是 0：自增主键从 1 开始，0 会和「查到了但是零值」混在一起。
	noUpstreamID int64 = -1
)

// Drainer 取走进程内缓冲的最近请求。取走即清空。
type Drainer interface {
	DrainRecent() []metrics.RecentRequest
}

// Options 落库服务的运行参数。
type Options struct {
	// Now 取当前时间，用例注入假时钟用。
	Now func() time.Time
	// Drainer 缓冲来源，nil 表示进程级的那一个。
	Drainer Drainer
	// Settings 保留期的来源，nil 表示进程级设置服务。每次裁剪现读，
	// 界面上改完，下一次裁剪（含重启时的启动裁剪）就按新值走（决策 4/6）。
	Settings setting_svc.RuntimeSource
	// FlushInterval、PruneInterval 两个定时任务的周期，0 按默认值。
	FlushInterval time.Duration
	PruneInterval time.Duration
	// ExitFlushTimeout 退出时等循环收尾、以及落最后一批的上限，0 按默认值。
	//
	// 上限是必须的（见 CloseHandle）：一个不响应的库不该把退出拖到只剩 SIGKILL。
	ExitFlushTimeout time.Duration
}

// RequestSvc 「最近请求」的业务操作。
//
// 它是一个 cago.Component：Start 起落库循环，CloseHandle 同步落退出那批。
// 这样它排在了数据库的 CloseHandle 之前（框架按注册逆序关组件），
// 「退出前落一次」不再和框架关库赛跑。
type RequestSvc interface {
	cago.Component
	// Flush 取走缓冲区里的全部记录并批量落库。失败时这一批就没了，不重试。
	Flush(ctx context.Context) error
	// Prune 按当下的保留期裁掉超期的行，返回裁掉多少行。
	Prune(ctx context.Context) (int64, error)
	// Run 跑定时的落库与裁剪，直到 ctx 结束。
	Run(ctx context.Context)
	// RecentRequests 某个上游最近的若干次拉取，最近的在最前。
	//
	// 库读不出来时把错误交给调用方：管理接口那层会翻成 503，界面据此让
	// 这块面板整块消失（失败与降级一节）。
	RecentRequests(ctx context.Context, req *admin.RecentRequestsRequest) (
		*admin.RecentRequestsResponse, error)
}

type requestSvc struct {
	opt Options

	// logMu 护住失败日志的节流时刻。Flush 是导出的，可能被 Run 之外的调用方碰。
	logMu       sync.Mutex
	lastErrorAt time.Time

	// startMu 护住 Start 记下的两个字段：CloseHandle 可能从别的 goroutine 读。
	startMu sync.Mutex
	// startCtx 是 Start 收到的那个 ctx。CloseHandle 去掉它的取消后落退出那批。
	startCtx context.Context
	// runDone 在 Start 起的循环退出后关闭，CloseHandle 先等它。
	runDone chan struct{}
}

// New 构造业务层。
func New(opt Options) RequestSvc {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.FlushInterval <= 0 {
		opt.FlushInterval = defaultFlushInterval
	}
	if opt.PruneInterval <= 0 {
		opt.PruneInterval = defaultPruneInterval
	}
	if opt.ExitFlushTimeout <= 0 {
		opt.ExitFlushTimeout = defaultExitFlushTimeout
	}
	return &requestSvc{opt: opt}
}

var defaultRequest RequestSvc = New(Options{})

// Request 返回「最近请求」业务层。
func Request() RequestSvc {
	return defaultRequest
}

// Register 注册实现，由 main 装配、由测试注入 mock。
func Register(svc RequestSvc) {
	defaultRequest = svc
}

// drainer 缓冲来源。默认延迟到调用时才取，包初始化时就去碰全局
// registry 会让「导入这个包」变成一次注册指标的副作用。
func (s *requestSvc) drainer() Drainer {
	if s.opt.Drainer != nil {
		return s.opt.Drainer
	}
	return metrics.Default()
}

// settings 保留期来源，nil 时用进程级设置服务。
func (s *requestSvc) settings() setting_svc.RuntimeSource {
	if s.opt.Settings != nil {
		return s.opt.Settings
	}
	return setting_svc.Setting()
}

// Flush 把缓冲区里的记录取走并批量落库。
//
// 取走即清空：失败丢掉这一批、不重试（决策 9）。库里已经有的行不受影响，
// 而重试要引入一个需要自己设上限的队列，收益只有一秒的行。
func (s *requestSvc) Flush(ctx context.Context) error {
	rows, err := s.rows(ctx)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		// 安静的一秒不写空的一批：GORM 对空切片报 ErrEmptySlice，
		// 放任它冒出去只会让落库循环在没事的时候每秒多一条 error。
		return nil
	}
	repo := request_log_repo.RecentRequestLog()
	if repo == nil {
		// 没装配就当作写入失败：丢这一批并让调用方记一条被节流的 error，
		// 而不是在这里空指针 panic 掉整个后台循环。
		return errors.New("最近请求仓储未装配")
	}
	return repo.Save(ctx, rows)
}

// rows 把缓冲里的一批记录翻成明细行。
//
// 主机名翻 upstream_id 是这里做而不是热路径上做：热路径不查库。一个主机一批里
// 只查一次上游表——一批里同一个上游通常占绝大多数，逐个记录查一次等于把落库
// 变成每秒几十次读。翻不出 id 的行没有归属，丢弃即可（记录与落库一节）。
func (s *requestSvc) rows(ctx context.Context) ([]*request_log_entity.RecentRequest, error) {
	records := s.drainer().DrainRecent()
	if len(records) == 0 {
		return nil, nil
	}
	now := s.opt.Now().Unix()
	ids := make(map[string]int64, len(records))
	rows := make([]*request_log_entity.RecentRequest, 0, len(records))
	for _, rec := range records {
		id, ok := ids[rec.Upstream]
		if !ok {
			upstream, err := upstream_svc.Upstream().FindByHost(ctx, rec.Upstream)
			if err != nil {
				// 上游表都读不出来，这一批就落不成：整批丢掉，由调用方记一条
				// 被节流的 error。只丢这一个主机的行会让错误更难看见。
				return nil, err
			}
			if upstream == nil {
				id = noUpstreamID
			} else {
				id = upstream.ID
			}
			ids[rec.Upstream] = id
		}
		if id == noUpstreamID {
			continue
		}
		rows = append(rows, &request_log_entity.RecentRequest{
			UpstreamID: id,
			At:         rec.At,
			Object:     rec.Object,
			Result:     string(rec.Result),
			Bytes:      rec.Bytes,
			DurationMS: rec.DurationMS,
			// 两个时间戳记的是这行什么时候写进来的，裁剪看的是 At。
			Createtime: now,
			Updatetime: now,
		})
	}
	return rows, nil
}

// Prune 按当下的保留期裁掉 at 早于「此刻减去保留期」的行。
func (s *requestSvc) Prune(ctx context.Context) (int64, error) {
	retention, err := s.retentionSeconds(ctx)
	if err != nil {
		return 0, err
	}
	repo := request_log_repo.RecentRequestLog()
	if repo == nil {
		// 和 Flush 一样把未装配当成失败：这里是在后台循环里跑的，
		// 空指针 panic 会把整个落库循环带走。
		return 0, errors.New("最近请求仓储未装配")
	}
	return repo.Prune(ctx, s.opt.Now().Unix()-retention)
}

// retentionSeconds 读出此刻的保留期。
//
// 每次裁剪现读而不是构造时抄进字段：决策 4/6 让这项设置在界面上改完立刻生效，
// 抄一份意味着改小了要等重启才收手。
func (s *requestSvc) retentionSeconds(ctx context.Context) (int64, error) {
	rt, err := s.settings().Runtime(ctx)
	if err != nil {
		return 0, err
	}
	if rt == nil || rt.RecentRequestRetentionSeconds <= 0 {
		// 保留期非正会当场把整张表删空。设置定义上不可能出现这个值，
		// 真的出现时宁可这一轮不裁，也不做一次不可逆的删除。
		return 0, errors.New("最近请求的保留期非正")
	}
	return rt.RecentRequestRetentionSeconds, nil
}

// logError 记一条落库失败，按分钟节流。
//
// 节流用的是注入的时钟，因此用例能在一个固定时刻上验「二十次失败只写一条」。
func (s *requestSvc) logError(ctx context.Context, msg string, err error) {
	s.logMu.Lock()
	now := s.opt.Now()
	if !s.lastErrorAt.IsZero() && now.Sub(s.lastErrorAt) < errorLogInterval {
		s.logMu.Unlock()
		return
	}
	s.lastErrorAt = now
	s.logMu.Unlock()
	logger.Ctx(ctx).Error(msg, zap.Error(err))
}

// Start 起落库循环，并记下循环结束的信号，由框架在注册组件时调用。
//
// 循环跑在 goroutine 里：Start 必须立刻返回，否则框架的注册会被一个
// 要跑到进程退出的循环卡住。
func (s *requestSvc) Start(ctx context.Context, _ *configs.Config) error {
	done := make(chan struct{})
	s.startMu.Lock()
	s.startCtx = ctx
	s.runDone = done
	s.startMu.Unlock()
	go func() {
		defer close(done)
		s.Run(ctx)
	}()
	return nil
}

// CloseHandle 等循环退出，再用没被取消的 ctx 落最后一次（失败与降级一节）。
//
// 框架停止时先取消所有组件的 ctx，再同步调 CloseHandle，最后才 gogo.Wait()：
// 这里同步落库，排在数据库 CloseHandle 之前，不再和关库赛跑。用
// context.WithoutCancel 而不是已经取消的 ctx，否则这一批必然写不进去。
//
// 等循环与落这一批都带 ExitFlushTimeout：框架调 CloseHandle 没有任何超时，
// 而 WithoutCancel 又把取消那条退路拿掉了，不留上限的话一个不响应的库会把退出
// 挂到只剩 SIGKILL（和 main 里缓存收尾的 cacheDrainTimeout 同一条理由）。
func (s *requestSvc) CloseHandle() {
	s.startMu.Lock()
	done := s.runDone
	ctx := s.startCtx
	s.startMu.Unlock()
	if ctx == nil {
		// Start 没跑过，没有循环要等，也没有退出那批要落。
		return
	}
	if done != nil {
		// 循环自己那一趟落库用的是已经取消的 ctx，正常的库上它是立刻返回的；
		// 卡在库上时，退出不该跟着一起挂住。
		timer := time.NewTimer(s.opt.ExitFlushTimeout)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			logger.Ctx(ctx).Warn("落库循环没能在退出前收尾",
				zap.Duration("timeout", s.opt.ExitFlushTimeout))
		}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.opt.ExitFlushTimeout)
	defer cancel()
	if err := s.Flush(ctx); err != nil {
		s.logError(ctx, flushErrorMessage, err)
	}
}

// Run 跑定时的落库与裁剪。
//
// 用 ticker 而不是 cron 组件：落库周期是「进程跑起来就该有」的东西，
// 不该多一个配不好就丢掉最近请求的开关。
//
// 退出那批不在这里落：CloseHandle 等这个循环退出之后才落，那才是
// 数据库关闭之前的最后一个同步点。
func (s *requestSvc) Run(ctx context.Context) {
	// 启动时先按当下的保留期裁一次（决策 10）：把保留期改小之后，重启
	// 就是一个立刻见效的止血手段，而不必再等最多一小时。
	if removed, err := s.Prune(ctx); err != nil {
		logger.Ctx(ctx).Error("启动时裁剪最近请求失败", zap.Error(err))
	} else if removed > 0 {
		logger.Ctx(ctx).Info("裁掉超保留期的最近请求", zap.Int64("removed", removed))
	}

	flush := time.NewTicker(s.opt.FlushInterval)
	defer flush.Stop()
	prune := time.NewTicker(s.opt.PruneInterval)
	defer prune.Stop()
	for {
		select {
		case <-ctx.Done():
			// 退出那批由 CloseHandle 落：它先等这个循环退出，再在数据库
			// 关闭之前同步落库（失败与降级一节）。
			return
		case <-flush.C:
			if err := s.Flush(ctx); err != nil {
				s.logError(ctx, flushErrorMessage, err)
			}
		case <-prune.C:
			removed, err := s.Prune(ctx)
			if err != nil {
				s.logError(ctx, "裁剪最近请求失败", err)
				continue
			}
			if removed > 0 {
				logger.Ctx(ctx).Info("裁掉超保留期的最近请求", zap.Int64("removed", removed))
			}
		}
	}
}
