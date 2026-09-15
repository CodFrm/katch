package git_svc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/cago-frame/cago/pkg/logger"
	"go.uber.org/zap"

	"github.com/CodFrm/katch/internal/model/entity/git_entity"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/gitsmart"
	"github.com/CodFrm/katch/internal/repository/git_repo"
)

// AnswerRequest 一次可能由本地镜像应答的 upload-pack 请求。
type AnswerRequest struct {
	// Host 上游主机名。
	Host string
	// Repo 仓库在上游侧的路径（dispatch.ClassifyGit 给的那一个）。
	Repo string
	// Advertise true 是 ref 广播（GET info/refs），false 是随后的协商（POST）。
	Advertise bool
	// Body 协商请求的原始请求体，广播时为空。
	Body []byte
}

// Answer 一次本地应答。调用方负责关闭 Body。
type Answer struct {
	// ContentType 这份响应体的 media type，直接发给客户端。
	ContentType string
	// Body 响应体。关掉它同时也放掉这份镜像上的读占用。
	Body io.ReadCloser
}

// DiskRequest 交给 Answerer 的那一份：镜像在盘上的哪里、要广播还是协商。
type DiskRequest struct {
	Dir       string
	Advertise bool
	Body      []byte
}

// Answerer 把一份盘上的镜像变成一次 smart HTTP 的应答。
//
// 抽成接口的理由同 Builder：TTL、降级与并发合并是一回事，「pkt-line 怎么拼」
// 是另一回事，两者按不同的方式验——前者拿假的应答器，后者拿真仓库。
type Answerer interface {
	// Answer 生成响应体。本地答不了时返回一个裹着 gitsmart.ErrNotAnswerable
	// 的错误，调用方据此降级为穿透。
	Answer(ctx context.Context, req *DiskRequest) (*Answer, error)
}

// NewGitSmartAnswerer 构造基于 gitsmart 的实现。
func NewGitSmartAnswerer() Answerer {
	return &gitSmartAnswerer{}
}

type gitSmartAnswerer struct{}

func (g *gitSmartAnswerer) Answer(ctx context.Context, req *DiskRequest) (*Answer, error) {
	if req.Advertise {
		raw, err := gitsmart.Advertise(ctx, req.Dir)
		if err != nil {
			return nil, err
		}
		return &Answer{
			ContentType: gitsmart.AdvertisementContentType,
			Body:        io.NopCloser(bytes.NewReader(raw)),
		}, nil
	}
	body, err := gitsmart.UploadPack(ctx, req.Dir, req.Body)
	if err != nil {
		return nil, err
	}
	return &Answer{ContentType: gitsmart.ResultContentType, Body: body}, nil
}

// LocalAnswerer 能拿本地镜像应答 git 请求的镜像层。
//
// 和 MirrorSvc 分成两个接口，而不是把这两个方法并进去：建镜像与应答镜像是两件
// 可以分开成立的事——一个只登记、只穿透的镜像层（镜像功能整个关掉时就是这样）
// 照样是一个合法的 MirrorSvc。按能力分接口，调用方问的就是「你答得了吗」，
// 而不是「你是不是那个实现」。
type LocalAnswerer interface {
	// Answer 尝试用本地镜像应答一次 upload-pack 请求，nil 表示这一次走穿透。
	Answer(ctx context.Context, req *AnswerRequest) *Answer
	// InUse 这个仓库的镜像此刻是不是正被读，配额淘汰据此跳过它。
	InUse(host, repo string) bool
}

// LocalAnswer 让进程级的镜像层试着本地应答一次，nil 表示这一次走穿透。
//
// 镜像层不具备本地应答能力时同样是 nil：那和「这个仓库还没镜像」在拉取路径上
// 是同一件事——穿透，而不是失败。
func LocalAnswer(ctx context.Context, req *AnswerRequest) *Answer {
	answerer, ok := Mirror().(LocalAnswerer)
	if !ok {
		return nil
	}
	return answerer.Answer(ctx, req)
}

// Answer 尝试用本地镜像应答一次 upload-pack 请求，nil 表示这一次走穿透。
//
// **不返回错误**，理由同 Ensure：本地应答是加速，它不成立时穿透照常可用（决策 5）。
// 给一个错误回来只会诱使调用方把它往上抛，于是一次读不到镜像记录就变成了一次
// 失败的 clone。所有「为什么没本地答」都落在日志里。
//
// 决定必须在写出任何字节之前做完：ref 广播一旦开了头就没法再改主意穿透，
// 所以同步、能力判定与对象遍历全部发生在这里，返回之后只剩把字节发出去。
func (m *mirrorSvc) Answer(ctx context.Context, req *AnswerRequest) *Answer {
	if !m.enabled(ctx) {
		return nil
	}
	upstream, err := m.mirrorable(ctx, req.Host)
	if err != nil || upstream == nil {
		// 不在白名单、已停用、没开 git：三者都不发出去，盘上有副本也一样
		// （镜像生命周期一节）。
		return nil
	}
	record, err := git_repo.GitMirror().FindByRepo(ctx, req.Host, req.Repo)
	if err != nil {
		logger.Ctx(ctx).Error("查镜像记录失败，本次穿透", zap.String("host", req.Host),
			zap.String("repo", req.Repo), zap.Error(err))
		return nil
	}
	if record == nil || record.State != git_entity.MirrorReady {
		// pending 还没建完，failed / rejected 盘上根本没有东西：一律穿透。
		return nil
	}
	dir, err := mirrorDir(m.dir, record.Host, record.Repo)
	if err != nil {
		logger.Ctx(ctx).Error("镜像目录算不出来，本次穿透",
			zap.String("host", req.Host), zap.String("repo", req.Repo), zap.Error(err))
		return nil
	}
	if !m.freshen(ctx, upstream, record) {
		return nil
	}
	m.touch(ctx, record)
	// 读占用要在生成响应体**之前**就拿住：pack 是边发边打的，而配额淘汰删的是
	// 整个仓库目录，删在半途会让这一次 clone 散架（镜像生命周期一节）。
	release := m.readers.acquire(mirrorKey(record.Host, record.Repo))
	answer, err := m.answerer.Answer(ctx, &DiskRequest{
		Dir: dir, Advertise: req.Advertise, Body: req.Body,
	})
	if err != nil || answer == nil {
		release()
		if !errors.Is(err, gitsmart.ErrNotAnswerable) {
			// 答不了是设计里的常态（shallow / filter），不必每次都喊一声；
			// 其余的失败是真出了事，那份镜像可能已经坏了。
			logger.Ctx(ctx).Warn("本地应答失败，本次穿透", zap.String("host", req.Host),
				zap.String("repo", req.Repo), zap.Error(err))
		}
		return nil
	}
	answer.Body = &leasedBody{ReadCloser: answer.Body, release: release}
	return answer
}

// InUse 这个仓库的镜像此刻是不是正被读。
//
// 配额淘汰据此跳过它：整仓删掉一份正在被打包的镜像，会让那一次 clone 散架。
func (m *mirrorSvc) InUse(host, repo string) bool {
	return m.readers.busy(mirrorKey(host, repo))
}

// freshen 让这份镜像满足新鲜度要求，返回这一次能不能继续本地应答。
//
// refs 的新鲜度复用上游记录上的 mutable_ttl_seconds（决策 8）：TTL 内直接广播；
// 超了先做一次增量同步再广播；同步失败或超时就这一次降级为穿透，**镜像状态不变**
// ——一次上游抖动不该把一个可用的镜像作废。
func (m *mirrorSvc) freshen(ctx context.Context, upstream *upstream_entity.Upstream,
	record *git_entity.GitMirror,
) bool {
	rt := m.settings(ctx)
	ttl := int64(upstream.MutableTTLSeconds)
	if ttl <= 0 {
		// 上游没单独配时用设置里的默认值，现读现用（同 cache_svc 的可变对象）。
		ttl = rt.MutableTTLSeconds
	}
	if freshEnough(record, ttl) {
		return true
	}
	key := mirrorKey(record.Host, record.Repo)
	if !m.inflight.claim(key) {
		// 这个仓库已经有人在同步（或者正在被重建）。并发的同步合并成一次：
		// 冷启动时几十个客户端同时 clone 同一个仓库，不合并就是把并发原样
		// 放大到上游。落单的这几个这一次走穿透，答案一样是对的。
		return false
	}
	defer m.inflight.release(key)
	// 领到之后再读一遍记录：刚刚排在前面的那个持有者可能已经把这个仓库同步
	// 完了，而我们手上这份是它开工之前读到的。少了这一眼，一批同时到达的
	// 请求会一个接一个地各同步一次——合并就只剩个名字。
	latest, err := git_repo.GitMirror().FindByRepo(ctx, record.Host, record.Repo)
	if err != nil {
		logger.Ctx(ctx).Error("重读镜像记录失败，本次穿透", zap.String("host", record.Host),
			zap.String("repo", record.Repo), zap.Error(err))
		return false
	}
	if latest == nil || latest.State != git_entity.MirrorReady {
		// 在我们排队的这会儿它被删了或者重新开始建了。
		return false
	}
	*record = *latest
	if freshEnough(record, ttl) {
		return true
	}

	// 摘掉客户端的取消：同步到一半被掐断会留下半套 ref，而这趟活对下一个
	// 请求同样有用。时限由同步超时给，它既盖同步本身也盖排队等名额那一段。
	syncCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx),
		time.Duration(rt.GitSyncTimeoutSeconds)*time.Second)
	defer cancel()
	release, err := m.slots.acquire(syncCtx, rt.GitSyncConcurrency)
	if err != nil {
		logger.Ctx(ctx).Warn("排不到同步名额，本次穿透", zap.String("host", record.Host),
			zap.String("repo", record.Repo), zap.Error(err))
		return false
	}
	defer release()

	size, err := m.builder.Sync(syncCtx, &SyncRequest{
		Dir: mustMirrorDir(m.dir, record), Remote: remoteURL(upstream.Origin, record.Repo),
	})
	if err != nil {
		logger.Ctx(ctx).Warn("增量同步失败，本次穿透且镜像状态不变",
			zap.String("host", record.Host), zap.String("repo", record.Repo), zap.Error(err))
		return false
	}
	now := time.Now().Unix()
	record.LastSyncAt = now
	record.SizeBytes = size
	record.Updatetime = now
	if err := git_repo.GitMirror().Save(ctx, record); err != nil {
		// 写不进去不影响这一次应答：盘上那份已经是新的了，只是下一次会再同步
		// 一遍。为一条写不进去的时间戳把一次能本地答的请求推去穿透不划算。
		logger.Ctx(ctx).Error("写同步时刻失败", zap.String("host", record.Host),
			zap.String("repo", record.Repo), zap.Error(err))
	}
	// 配额淘汰不在这里现触发：这份记录的 last_access_at 这一刻还没被 touch()
	// 推到现在——增量同步恰恰发生在一份镜像已经沉寂到 TTL 过期之后，它此刻很
	// 可能正是全局最旧的那个候选，而调用方紧接着就要读它（touch/readers.acquire
	// 都还没轮到）。在这里淘汰会把自己刚同步完、正要发出去的那份镜像删掉。
	// 配额由 finish() 在建成新镜像之后触发，以及 main.go 的定期 Sweep 兜底。
	return true
}

// freshEnough 这份镜像还在 TTL 之内吗。
//
// ttl <= 0 当成「不过期」：设置读不出来时按出厂值走，而出厂值不为零；真要是
// 拿到了个 0，宁可用一份可能旧的镜像，也好过每一次请求都去同步一遍。
func freshEnough(record *git_entity.GitMirror, ttl int64) bool {
	return ttl <= 0 || time.Now().Unix()-record.LastSyncAt < ttl
}

// mustMirrorDir 取镜像目录，算不出来时给空串。
//
// 调用点在 Answer 里已经算过一次并且挡掉了失败，这里只是不想把它顺着几层参数
// 传下去；真给出空串时 Sync 会当场失败，而那一次照样降级为穿透。
func mustMirrorDir(root string, record *git_entity.GitMirror) string {
	dir, err := mirrorDir(root, record.Host, record.Repo)
	if err != nil {
		return ""
	}
	return dir
}

// touch 把最后访问时间推到现在。
//
// 本地应答这一条路不经过 Ensure，没有它，一个一直由本地服务的仓库在配额淘汰
// 眼里会显得越来越冷，最后被当成没人要的那一个删掉。
func (m *mirrorSvc) touch(ctx context.Context, record *git_entity.GitMirror) {
	now := time.Now().Unix()
	if err := git_repo.GitMirror().Touch(ctx, record.ID, now); err != nil {
		logger.Ctx(ctx).Error("更新镜像访问时间失败", zap.Int64("id", record.ID), zap.Error(err))
		return
	}
	record.LastAccessAt = now
}

// leasedBody 一份带读占用的响应体：关掉它的同时把占用放掉。
//
// 占用跟着响应体走而不是跟着函数调用走，是因为 pack 是边发边打的——Answer
// 返回时这次读才刚开始。
type leasedBody struct {
	io.ReadCloser
	release func()
}

// Close 先关流再放占用，而不是反过来：占用要一直盖到最后一个字节读完为止，
// 先放掉它等于在打包还没停下来的时候就允许配额淘汰删这个目录。defer 而不是
// 顺序两句，是为了底下那个 Close 出什么意外时占用也不会漏掉——漏一次，这份
// 镜像就永远删不掉了。
func (b *leasedBody) Close() error {
	defer b.release()
	return b.ReadCloser.Close()
}

// readerSet 每个仓库此刻有几个读者。
//
// 计数而不是布尔：同一个仓库同时被几个客户端拉是常态，任何一个还在读，
// 这份镜像就不能删。
type readerSet struct {
	mu     sync.Mutex
	counts map[string]int
}

// acquire 记一个读者，返回放掉它的动作。这个动作可以被调用多次，只有第一次算数。
func (s *readerSet) acquire(key string) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.counts == nil {
		s.counts = map[string]int{}
	}
	s.counts[key]++
	return sync.OnceFunc(func() { s.release(key) })
}

func (s *readerSet) release(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts[key]--
	if s.counts[key] <= 0 {
		// 删掉而不是留一个 0：仓库数没有上限，留着就是一张只涨不消的表。
		delete(s.counts, key)
	}
}

// busy 这个仓库此刻有没有人在读。
func (s *readerSet) busy(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[key] > 0
}
