package git_svc

import (
	"context"
	"errors"
	"os"

	"github.com/cago-frame/cago/pkg/logger"
	"go.uber.org/zap"

	"github.com/CodFrm/katch/internal/model/entity/event_entity"
	"github.com/CodFrm/katch/internal/model/entity/git_entity"
	"github.com/CodFrm/katch/internal/repository/git_repo"
	"github.com/CodFrm/katch/internal/service/event_svc"
)

// ErrMirrorNotFound 要删的那条镜像记录已经不在了。
//
// 界面重复点两下删除、或者两个管理员同时删同一条，都会撞上这个——它不是一次
// 系统错误，不必在日志里留一条 error。
var ErrMirrorNotFound = errors.New("镜像不存在")

// ErrMirrorInUse 这份镜像此刻正被人用着，不能删。
//
// 两种「用着」：正被本地应答读着，或者后台正在建它/同步它。前者是因为 pack 是
// 边发边打的（answer.go），半路抽掉目录会让正在进行的那次 clone 散架；后者是
// 因为 go-git 正往这个目录里写，而删掉库记录之后那趟活收尾时的 Save 会按原主键
// 把它重新插回来（gorm 的 Save 在更新影响 0 行时回落到 Create），留下一条 ready、
// 占着配额、盘上却什么都没有的记录（镜像生命周期一节）。
//
// 配额淘汰遇到这种情况会跳过，手动删除则直接拒绝——人没有理由替一个正在服务
// 的请求、或者一趟马上就要写完的活做这个决定。
var ErrMirrorInUse = errors.New("镜像正在被读取或同步，暂时无法删除")

// evictBatch 配额淘汰一趟最多查多少条候选，同 cache_svc 的那个：给候选一个
// 上限，一次超大规模的超配额不至于把整张表一口气捞回内存。
const evictBatch = 64

// Manageable 镜像层里能被管理界面直接操作的那一半：列出、删除、按配额淘汰。
//
// 和 MirrorSvc 分成两个接口，而不是把这三个方法并进去——理由同 LocalAnswerer
// （answer.go）：建镜像/应答镜像/管理镜像是三件可以分开成立的事，按能力分接口，
// 调用方问的是「你做得了这个操作吗」，而不是「你是不是那个实现」。并进
// MirrorSvc 会打断 internal/service/proxy_svc 与 internal/web 里那些只关心
// Ensure/Lookup/Quiesce 的测试替身。
type Manageable interface {
	// List 列出全部镜像记录，管理界面的镜像列表页用。
	List(ctx context.Context) ([]*git_entity.GitMirror, error)
	// Delete 删除一条镜像：盘上的目录与库记录都会被清掉，下一次拉取因此重新走
	// 穿透并在后台重建（镜像生命周期一节）。id 不存在给 ErrMirrorNotFound；
	// 镜像正被本地应答读着、或者后台正在建它/同步它时给 ErrMirrorInUse——
	// 半路抽掉目录会让那次读散架，也会让那趟写把记录重新插回来。
	Delete(ctx context.Context, id int64) error
	// Sweep 镜像总量超过配额时，按最后访问时间最旧的顺序整仓删除直到配额之下，
	// 返回这一轮删了几个仓库。正被读或正被写的镜像跳过，同 Delete。
	Sweep(ctx context.Context) (int64, error)
}

// List 列出进程级镜像层里的全部镜像记录，管理界面用。镜像层不具备管理能力时
// 给空列表——那和「还没有镜像」在界面上是同一件事。
func List(ctx context.Context) ([]*git_entity.GitMirror, error) {
	manager, ok := Mirror().(Manageable)
	if !ok {
		return nil, nil
	}
	return manager.List(ctx)
}

// Delete 删除进程级镜像层里的一条镜像。
func Delete(ctx context.Context, id int64) error {
	manager, ok := Mirror().(Manageable)
	if !ok {
		return ErrMirrorNotFound
	}
	return manager.Delete(ctx, id)
}

// Sweep 让进程级镜像层按配额淘汰一次。镜像层不具备管理能力时什么都不做——
// 同 MirrorSvc 出厂时「关着」的那份，淘汰无从谈起。
func Sweep(ctx context.Context) (int64, error) {
	manager, ok := Mirror().(Manageable)
	if !ok {
		return 0, nil
	}
	return manager.Sweep(ctx)
}

func (m *mirrorSvc) List(ctx context.Context) ([]*git_entity.GitMirror, error) {
	repo := git_repo.GitMirror()
	if repo == nil {
		// 仓储没装配（用例里、或者 main 漏了一行）：给空列表而不是崩，
		// 界面照常渲染成「还没有镜像」。
		return nil, nil
	}
	return repo.List(ctx)
}

func (m *mirrorSvc) Delete(ctx context.Context, id int64) error {
	repo := git_repo.GitMirror()
	if repo == nil {
		return ErrMirrorNotFound
	}
	record, err := repo.Find(ctx, id)
	if err != nil {
		return err
	}
	if record == nil {
		return ErrMirrorNotFound
	}
	if m.pinned(record.Host, record.Repo) {
		return ErrMirrorInUse
	}
	m.removeDisk(ctx, record)
	return repo.Delete(ctx, id)
}

func (m *mirrorSvc) Sweep(ctx context.Context) (int64, error) {
	repo := git_repo.GitMirror()
	if repo == nil {
		return 0, nil
	}
	return m.enforceQuota(ctx)
}

// enforceQuota 镜像总量超过配额时，按最后访问时间最旧的顺序整仓删除，直到配额
// 之下——和 cache_svc.enforceQuota 同一个形状，只是淘汰的单位是整个仓库而不是
// 单个对象（镜像生命周期一节）。
//
// 淘汰到配额本身，不像缓存那样淘汰到一个更低的回收水位：镜像没有「写一个就检查
// 一次」那么高频，恰好卡在配额上不会像对象缓存那样每写一次就再淘汰一次。
func (m *mirrorSvc) enforceQuota(ctx context.Context) (int64, error) {
	m.evictMu.Lock()
	defer m.evictMu.Unlock()

	repo := git_repo.GitMirror()
	if repo == nil {
		return 0, nil
	}
	total, err := repo.TotalSize(ctx)
	if err != nil {
		logger.Ctx(ctx).Error("统计镜像占用失败", zap.Error(err))
		return 0, err
	}
	quota := m.settings(ctx).GitMirrorQuotaBytes
	if total <= quota {
		return 0, nil
	}
	var removed int64
	for total > quota {
		candidates, err := repo.EvictCandidates(ctx, evictBatch)
		if err != nil {
			logger.Ctx(ctx).Error("查询镜像淘汰候选失败", zap.Error(err))
			return removed, err
		}
		if len(candidates) == 0 {
			logger.Ctx(ctx).Error("镜像超配额但没有可淘汰的仓库",
				zap.Int64("total", total), zap.Int64("quota", quota))
			return removed, nil
		}
		progressed := false
		for _, mirror := range candidates {
			if total <= quota {
				break
			}
			if m.pinned(mirror.Host, mirror.Repo) {
				// 正被读或正在被写的仓库跳过；它不算「没有候选」，只是这一个
				// 不该动。
				continue
			}
			m.removeDisk(ctx, mirror)
			if err := repo.Delete(ctx, mirror.ID); err != nil {
				logger.Ctx(ctx).Error("删除镜像记录失败",
					zap.Int64("id", mirror.ID), zap.Error(err))
				return removed, err
			}
			m.evicted(ctx, mirror)
			total -= mirror.SizeBytes
			removed++
			progressed = true
		}
		if !progressed {
			// 这一批全在被读或被写：不是「没有候选」，是候选眼下都动不了。继续
			// 按同一批重试只会原地打转，只能等下一轮 Sweep 再看。
			logger.Ctx(ctx).Warn("镜像超配额但剩余仓库都正被读取或同步，本轮暂不淘汰",
				zap.Int64("total", total), zap.Int64("quota", quota))
			return removed, nil
		}
	}
	return removed, nil
}

// evicted 把一次配额淘汰记进事件流（可观测性一节：建成、失败、超限拒绝、被淘汰
// 都是镜像状态变化，都从 event_svc 这一个出口出去）。
//
// 一条记一个仓库，而不是一轮记一条汇总：镜像是整仓删的，站长下一次发现某个仓库
// 又开始穿透时，要在时间线上查的正是「它是什么时候被收走的」，一个「删了 3 个」
// 答不了这个问题。
//
// 不挂 UpstreamID：淘汰跑在后台，而它恰恰可能发生在上游刚被删掉之后，为一条
// 时间线去反查一条可能已经不存在的记录不划算（cache_reclaimed 同样不挂上游）。
// 主机名在细节里，界面据此组织文案。
//
// 删库之后才记：一条说「被收走了」而记录还在的时间线，比晚一拍记上要难查得多
// （同 finish 里库先于事件那一条）。
func (m *mirrorSvc) evicted(ctx context.Context, mirror *git_entity.GitMirror) {
	event_svc.Event().Record(ctx, &event_svc.RecordInput{
		Kind:  event_entity.KindGitMirrorEvicted,
		Actor: event_entity.ActorSystem,
		Detail: &mirrorDetail{
			Host: mirror.Host, Repo: mirror.Repo, State: mirror.State,
			SizeBytes: mirror.SizeBytes,
		},
	})
}

// pinned 这个仓库此刻能不能被整个删掉。
//
// 两道闸合在一处，因为它们挡的是同一件事——「有人正在用这个目录」：
//
//   - readers：本地应答正在边发边打这份镜像（answer.go 的读占用）；
//   - inflight：后台正在建它，或者一次超出 TTL 的请求正在增量同步它。
//
// 只看 readers 不够：增量同步发生在 readers.acquire **之前**（Answer 先 freshen
// 再拿读占用），所以一份正被 go-git 写着的仓库在 InUse 眼里恰恰是空闲的，而它
// 又因为久未被访问而排在淘汰候选的最前面。
func (m *mirrorSvc) pinned(host, repo string) bool {
	return m.InUse(host, repo) || m.inflight.held(mirrorKey(host, repo))
}

// removeDisk 丢掉一份镜像在盘上的目录。
//
// 算不出目录或者删不掉都只记日志、不中断：调用方接下来还要删库记录，盘上留下
// 的孤儿字节不是「这次操作失败」，只是下一次同一个仓库被建镜像时会被
// Builder.Build 的开局清理带走（builder.go 的 discard 同理）。
func (m *mirrorSvc) removeDisk(ctx context.Context, record *git_entity.GitMirror) {
	dir, err := mirrorDir(m.dir, record.Host, record.Repo)
	if err != nil {
		logger.Ctx(ctx).Error("镜像目录算不出来，跳过删盘", zap.String("host", record.Host),
			zap.String("repo", record.Repo), zap.Error(err))
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		logger.Ctx(ctx).Error("删除镜像目录失败", zap.String("dir", dir), zap.Error(err))
	}
}
