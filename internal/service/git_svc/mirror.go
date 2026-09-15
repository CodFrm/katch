// Package git_svc 是 git 本地镜像的业务层：谁被拉过就镜像谁（决策 7）。
//
// 它坐在穿透路径的旁边，不在它上面：一次 clone 该拿到什么，由穿透那一侧决定；
// 这里只负责把「这个仓库被拉过」变成盘上的一份裸仓库，把那份镜像此刻处在哪个
// 状态如实记在库里，以及在它就绪时拿它答一次 upload-pack。镜像没建成之前，
// 拉取一律穿透（决策 5）。
//
// 三件事按能力分成三个接口：MirrorSvc（建镜像，mirror.go）、LocalAnswerer
// （本地应答，answer.go）、Manageable（列出/删除/按配额淘汰，sweep.go）。
// 「pkt-line 怎么拼」不在这一层，那是 internal/proxy/gitsmart 的事。
package git_svc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cago-frame/cago/pkg/logger"
	"go.uber.org/zap"

	"github.com/CodFrm/katch/internal/model/entity/event_entity"
	"github.com/CodFrm/katch/internal/model/entity/git_entity"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/repository/git_repo"
	"github.com/CodFrm/katch/internal/service/event_svc"
	"github.com/CodFrm/katch/internal/service/setting_svc"
	"github.com/CodFrm/katch/internal/service/upstream_svc"
)

// MirrorSvc 本地镜像的业务操作。
type MirrorSvc interface {
	// Ensure 一次穿透拉取之后登记这个仓库，必要时在后台把镜像建起来。
	//
	// **不返回错误**，理由同 event_svc.Record：调用点在拉取路径的成功路径上，
	// 给一个错误回来只会诱使调用方把它往上抛，于是一次建不了镜像就变成了一次
	// 失败的 clone——而镜像是加速，它建不起来时穿透照常可用（决策 5）。
	//
	// 登记本身是同步的（几毫秒的库操作），建镜像是后台的：ref 广播不能等一次
	// clone 跑完，那正是「未就绪时穿透」这条的由来。
	Ensure(ctx context.Context, host, repo string)
	// Quiesce 等此刻还在跑的后台建镜像收尾为止，ctx 结束时带着 ctx 的错误返回。
	//
	// 同 cache_svc.Quiesce：建镜像脱离请求跑，进程退出或用例拆台之前要有一个
	// 等得到的入口，否则它们会在库和镜像目录被换掉之后继续往里写。
	Quiesce(ctx context.Context) error
}

// Builder 把一个远端仓库建成盘上的一份裸仓库。
//
// 抽成接口是因为状态机与「怎么 clone」是两件事：状态转移的用例不该依赖一次真实
// 的网络 clone，而 go-git 那一侧的用例要的是一个真仓库，两者按不同的方式验。
type Builder interface {
	// Build 建镜像，返回盘上占了多少字节。
	//
	// 超过 MaxBytes 时返回 ErrRepoTooLarge，调用方据此把仓库登记为 rejected；
	// 上游不再送字节返回 ErrBuildStalled、整段跑得太久返回 ErrBuildTimedOut，
	// 这两者连同其余错误一律是「这次没建成」（决策 5）。
	Build(ctx context.Context, req *BuildRequest) (int64, error)
	// Sync 把一份已经建好的镜像增量同步到上游此刻的样子，返回同步之后的体积。
	//
	// 和 Build 分开而不是每次重新 clone：refs 超出 TTL 是常态（决策 8），
	// 每次都把一个仓库整个拉一遍，本地镜像就从加速变成了对上游的放大。
	Sync(ctx context.Context, req *SyncRequest) (int64, error)
}

// SyncRequest 一次增量同步要的东西。
type SyncRequest struct {
	// Dir 盘上那份裸仓库。
	Dir string
	// Remote 上游仓库的完整地址。每次现拼而不是用仓库里记着的那个 remote：
	// 站长在管理界面上改掉回源地址之后，下一次同步就该打到新的地方去。
	Remote string
}

// BuildRequest 一次建镜像要的东西。
type BuildRequest struct {
	// Dir 镜像在盘上的位置，建成之后这里是一个裸仓库。
	Dir string
	// Remote 上游仓库的完整地址（上游的回源地址加上仓库路径）。
	Remote string
	// MaxBytes 单仓体积上限，每次现读（决策 3/4）。
	MaxBytes int64
	// StallTimeout fetch 阶段内多久收不到上游的字节算停滞，0 表示不限。
	StallTimeout time.Duration
	// TotalTimeout 这一趟建镜像整段墙钟还剩多少，0 表示不限。
	//
	// 是「还剩多少」而不是设置里那个值：总时限盖的是从开始建镜像到镜像可用
	// 为止的整段（决策 3），排队等名额那一段已经花掉的时间要算在里面。
	TotalTimeout time.Duration
}

// ErrRepoTooLarge 仓库大过单仓上限，不镜像它。
//
// 单独一个错误而不是一个普通失败：超限的仓库要登记成 rejected 并从此永久穿透，
// 而一次失败是可以再试的。两者混在一起，一个 3 GB 的仓库会被反复拉上一整天。
var ErrRepoTooLarge = errors.New("仓库体积超过单仓上限")

// ErrBuildStalled 上游不再送字节了，这次建镜像等不下去。
//
// 和 ErrBuildTimedOut 分成两个而不是一个「超时」：站长看到 failed 时要知道该
// 换上游还是该把总时限调大，两种中止外观相同的话，他无从判断（决策 6）。
// 两者都是失败而不是拒绝——上游抖一下，下一次拉取照样可以再试（决策 5）。
var ErrBuildStalled = errors.New("建镜像中止：上游停止发送数据")

// ErrBuildTimedOut 这次建镜像整体跑得太久了。
var ErrBuildTimedOut = errors.New("建镜像中止：超过建镜像总时限")

// Options 构造参数。
type Options struct {
	// Dir 镜像根目录。空表示不建镜像——拉取照常穿透，只是没有本地副本。
	//
	// 它和对象缓存目录分开：两者的淘汰依据不同（对象按 LRU + 摘要，镜像按最后
	// 访问时间 + 仓库粒度），混在一处会让任一侧的配额失去意义。
	Dir string
	// Runtime 运行时设置的来源，nil 表示进程级那一个。
	Runtime setting_svc.RuntimeSource
	// Builder 怎么建镜像，nil 表示用 go-git 那个。
	Builder Builder
	// Answerer 怎么把一份镜像变成一次应答，nil 表示用 gitsmart 那个。
	Answerer Answerer
}

type mirrorSvc struct {
	dir      string
	runtime  setting_svc.RuntimeSource
	builder  Builder
	answerer Answerer
	// slots 建镜像的并发闸，上限每次现读。
	slots syncSlots
	// inflight 正在被本进程处理的仓库。它同时是两件事的闸：并发的建镜像合并成
	// 一次（决策 9 的同理），以及同一个仓库的记录读写被串起来——没有它，两个
	// 同时到达的请求会各自读到「还没有这条记录」，然后双双插入。
	inflight inflightSet
	// pending 还没跑完的后台建镜像，供 Quiesce 等。
	pending sync.WaitGroup
	// readers 此刻正在被本地应答读着的镜像，配额淘汰据此跳过它们。
	readers readerSet
	// evictMu 把配额淘汰串起来：一次建镜像刚收尾、一次增量同步刚收尾，两者都会
	// 触发淘汰，不串起来会把总量削过头（同 cache_svc.evictMu）。
	evictMu sync.Mutex
}

// New 构造镜像层。
func New(opt Options) MirrorSvc {
	if opt.Runtime == nil {
		opt.Runtime = setting_svc.Setting()
	}
	if opt.Builder == nil {
		opt.Builder = NewGitBuilder()
	}
	if opt.Answerer == nil {
		opt.Answerer = NewGitSmartAnswerer()
	}
	return &mirrorSvc{
		dir:      opt.Dir,
		runtime:  opt.Runtime,
		builder:  opt.Builder,
		answerer: opt.Answerer,
		inflight: newInflightSet(),
	}
}

// defaultMirror 在 main 装配之前镜像是关的：拉取路径不该依赖装配顺序，
// 没有镜像目录时 Ensure 什么都不做，clone 照常穿透。
var defaultMirror = New(Options{})

// Mirror 返回镜像层。
func Mirror() MirrorSvc {
	return defaultMirror
}

// Register 注册实现，由 main 装配、由测试注入。
func Register(svc MirrorSvc) {
	defaultMirror = svc
}

func (m *mirrorSvc) Ensure(ctx context.Context, host, repo string) {
	if !m.enabled(ctx) {
		return
	}
	// 脱离客户端的取消：登记是几毫秒的库操作，但客户端在 ref 广播中途断开
	// 不该让一条记录只写了一半，更不该让后台那趟活带着一个已经结束的 context。
	ctx = context.WithoutCancel(ctx)
	upstream, err := m.mirrorable(ctx, host)
	if err != nil || upstream == nil {
		return
	}
	key := mirrorKey(host, repo)
	if !m.inflight.claim(key) {
		// 这个仓库已经有人在处理了。第二个请求什么都不用做——它要的那份镜像
		// 正在被建，而它这一次本来就走穿透。
		return
	}
	handed := false
	defer func() {
		if !handed {
			m.inflight.release(key)
		}
	}()

	record, err := git_repo.GitMirror().FindByRepo(ctx, host, repo)
	if err != nil {
		logger.Ctx(ctx).Error("查镜像记录失败", zap.String("host", host),
			zap.String("repo", repo), zap.Error(err))
		return
	}
	now := time.Now().Unix()
	if record != nil {
		// 只更新访问时间：状态那几列归后台那趟活写，整行写回会把它刚写下的
		// 结果覆盖成调用方手上这份旧的。
		if err := git_repo.GitMirror().Touch(ctx, record.ID, now); err != nil {
			logger.Ctx(ctx).Error("更新镜像访问时间失败", zap.Int64("id", record.ID), zap.Error(err))
		}
		record.LastAccessAt = now
		if record.State != git_entity.MirrorPending {
			// ready 不必重建（增量同步是本地应答那一侧的事）；failed 与 rejected
			// 都不再试（目标：二者都不再重复触发）——反复去建一个建不成的仓库，
			// 只会把每一次 clone 都变成一次对上游的重试风暴。
			return
		}
		// pending 但没人在建：只可能是上一个进程留下的（在建的都被 claim 挡在
		// 上面了）。重新驱动它，否则这条记录会永远停在 pending。
	} else {
		record = &git_entity.GitMirror{
			Host: host, Repo: repo, State: git_entity.MirrorPending,
			LastAccessAt: now, Createtime: now, Updatetime: now,
		}
		if err := git_repo.GitMirror().Save(ctx, record); err != nil {
			logger.Ctx(ctx).Error("登记镜像失败", zap.String("host", host),
				zap.String("repo", repo), zap.Error(err))
			return
		}
		m.record(ctx, event_entity.KindGitMirrorPending, upstream, record)
	}

	// 计数要在起协程**之前**加：加在协程里面的话，Quiesce 可能刚好在它还没被
	// 调度到的时候看到一个空计数，于是「等干完」等了个寂寞（同 cache_svc.pump）。
	m.pending.Add(1)
	handed = true
	go m.build(ctx, upstream, record, key)
}

func (m *mirrorSvc) Quiesce(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		m.pending.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// build 后台把一个仓库建成镜像，然后按结果落状态。跑在自己的协程上。
func (m *mirrorSvc) build(ctx context.Context, upstream *upstream_entity.Upstream,
	record *git_entity.GitMirror, key string,
) {
	defer func() {
		// 先放掉 inflight 再报干完：反过来的话，Quiesce 返回之后这个仓库还
		// 攥在手里，紧接着的一次 Ensure 会被无声地丢掉。
		m.inflight.release(key)
		m.pending.Done()
	}()

	dir, err := mirrorDir(m.dir, record.Host, record.Repo)
	if err != nil {
		// 仓库路径落不到一个安全的目录上。这是一次失败而不是一次拒绝：
		// 拒绝说的是「这个仓库太大」，而这里说的是「这个路径我们不敢照着建」。
		m.finish(ctx, upstream, record, git_entity.MirrorFailed, 0, err)
		return
	}
	rt := m.settings(ctx)
	// 总时限盖的是从开始建镜像到镜像可用为止的整段墙钟，排队等名额这一段也算
	// 在里面（同 proxy_svc）：连名额都排不到的活，快速失败远好过在队伍里堆着，
	// 而这里的 ctx 已经摘掉了客户端的取消，没有这个时限它就真的永远不会结束。
	//
	// git_sync_timeout_seconds 不再管这一段：它的生效范围收窄到增量同步
	// （决策 3）。两者叠加的话，出厂的 600 秒总是先到，新设置永远不会生效。
	total := time.Duration(rt.GitBuildTimeoutSeconds) * time.Second
	queueCtx, deadline := ctx, time.Time{}
	if total > 0 {
		deadline = time.Now().Add(total)
		var cancel context.CancelFunc
		queueCtx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	release, err := m.slots.acquire(queueCtx, rt.GitSyncConcurrency)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			err = queueTimedOut(rt.GitBuildTimeoutSeconds)
		}
		m.finish(ctx, upstream, record, git_entity.MirrorFailed, 0, err)
		return
	}
	defer release()

	// 建镜像本身**不**带 ctx 时限，它的两道时间闸走的是闸那一侧：go-git 的
	// delta 解析阶段整个不看 ctx（Problem 4），靠 ctx 的时限在那一段上形同
	// 虚设，而实测的卡死正发生在那里。
	var remaining time.Duration
	if !deadline.IsZero() {
		remaining = time.Until(deadline)
		if remaining <= 0 {
			// 名额是在最后一刻才拿到的，建镜像已经没有预算了。
			m.finish(ctx, upstream, record, git_entity.MirrorFailed, 0,
				queueTimedOut(rt.GitBuildTimeoutSeconds))
			return
		}
	}
	size, err := m.builder.Build(ctx, &BuildRequest{
		Dir:          dir,
		Remote:       remoteURL(upstream.Origin, record.Repo),
		MaxBytes:     rt.GitRepoMaxBytes,
		StallTimeout: time.Duration(rt.GitBuildStallSeconds) * time.Second,
		TotalTimeout: remaining,
	})
	switch {
	case errors.Is(err, ErrRepoTooLarge):
		m.finish(ctx, upstream, record, git_entity.MirrorRejected, 0, err)
	case err != nil:
		m.finish(ctx, upstream, record, git_entity.MirrorFailed, 0, err)
	default:
		m.finish(ctx, upstream, record, git_entity.MirrorReady, size, nil)
	}
}

// queueTimedOut 整段墙钟全花在排队等名额上了。
//
// 报总时限闸而不是原样抛一句 context 的话：last_error 要能指明是哪一道闸
// （决策 6），「context deadline exceeded」说不出该调哪一项设置。
func queueTimedOut(seconds int) error {
	return fmt.Errorf("%w：%d 秒的总时限全花在排队等建镜像名额上",
		ErrBuildTimedOut, seconds)
}

// finish 落一次状态变化：写库，然后记进事件流。
//
// 库先于事件：时间线是观察记录，它说「建成了」而库里还是 pending，比晚一拍
// 记上要难查得多。
func (m *mirrorSvc) finish(ctx context.Context, upstream *upstream_entity.Upstream,
	record *git_entity.GitMirror, state string, size int64, cause error,
) {
	now := time.Now().Unix()
	record.State = state
	record.SizeBytes = size
	record.Updatetime = now
	record.LastError = ""
	if cause != nil {
		record.LastError = cause.Error()
	}
	if state == git_entity.MirrorReady {
		record.LastSyncAt = now
	}
	if err := git_repo.GitMirror().Save(ctx, record); err != nil {
		// 写不进去就别记事件：一条说「建成了」而库里查不到的时间线，比没有
		// 这条时间线更糟。
		logger.Ctx(ctx).Error("写镜像状态失败", zap.String("host", record.Host),
			zap.String("repo", record.Repo), zap.String("state", state), zap.Error(err))
		return
	}
	m.record(ctx, mirrorEventKind(state), upstream, record)
	if state == git_entity.MirrorReady {
		// 新镜像刚落盘，此刻正是总量可能超配额的那一刻（同 cache_svc.Put 在
		// 提交之后触发一次回收）。
		_, _ = m.enforceQuota(ctx)
	}
}

// mirrorEventKind 一个状态对应事件流上的哪一类。
func mirrorEventKind(state string) string {
	switch state {
	case git_entity.MirrorPending:
		return event_entity.KindGitMirrorPending
	case git_entity.MirrorReady:
		return event_entity.KindGitMirrorReady
	case git_entity.MirrorRejected:
		return event_entity.KindGitMirrorRejected
	default:
		return event_entity.KindGitMirrorFailed
	}
}

// mirrorDetail 事件里带的字段。
//
// 存字段而不是一句拼好的话：句子既翻译不了也筛选不了，而界面本来就要按自己的
// 语言组织文案（event_entity 的 Detail）。
type mirrorDetail struct {
	Host      string `json:"host"`
	Repo      string `json:"repo"`
	State     string `json:"state"`
	SizeBytes int64  `json:"size_bytes"`
	Error     string `json:"error,omitempty"`
}

// record 把一次状态变化记进事件流。操作人是 system：没有人按下这个按钮。
func (m *mirrorSvc) record(ctx context.Context, kind string,
	upstream *upstream_entity.Upstream, mirror *git_entity.GitMirror,
) {
	event_svc.Event().Record(ctx, &event_svc.RecordInput{
		Kind:       kind,
		Actor:      event_entity.ActorSystem,
		UpstreamID: upstream.ID,
		Detail: &mirrorDetail{
			Host: mirror.Host, Repo: mirror.Repo, State: mirror.State,
			SizeBytes: mirror.SizeBytes, Error: mirror.LastError,
		},
	})
}

// enabled 这台 katch 现在建不建镜像。
func (m *mirrorSvc) enabled(ctx context.Context) bool {
	if m.dir == "" {
		// 没配镜像目录：功能整个是关的，拉取照常穿透。
		return false
	}
	if git_repo.GitMirror() == nil {
		// 仓储没装配（用例里、或者 main 漏了一行）。不能只是安静地什么都不做：
		// 那样镜像永远建不起来，而没人知道为什么。
		logger.Ctx(ctx).Warn("镜像仓储未装配，跳过建镜像")
		return false
	}
	return true
}

// mirrorable 这台主机的仓库该不该由我们镜像。
//
// 判据只有上游表：不在表里、已停用、没开 git 协议，三者都是「不该」。停用之所以
// 在这里就被折叠掉，是因为 FindByHost 本来就把它当成不存在——白名单的判定全仓
// 只有那一个出处。
func (m *mirrorSvc) mirrorable(ctx context.Context, host string) (*upstream_entity.Upstream, error) {
	upstream, err := upstream_svc.Upstream().FindByHost(ctx, host)
	if err != nil {
		logger.Ctx(ctx).Error("查上游失败", zap.String("host", host), zap.Error(err))
		return nil, err
	}
	if upstream == nil || !upstream.Protocols.Has(upstream_entity.ProtocolGit) {
		return nil, nil
	}
	return upstream, nil
}

// settings 读一次运行时设置。
//
// 读不出来不让建镜像这件事停摆：返回的快照在出错时是出厂值（同 proxy_svc）。
func (m *mirrorSvc) settings(ctx context.Context) *setting_svc.RuntimeSettings {
	rt, err := m.runtime.Runtime(ctx)
	if err != nil {
		logger.Ctx(ctx).Error("读取运行时设置失败，本次建镜像按默认值处理", zap.Error(err))
	}
	return rt
}
