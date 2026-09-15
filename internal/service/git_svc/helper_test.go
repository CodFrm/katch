package git_svc

import (
	"context"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/model/entity/event_entity"
	"github.com/CodFrm/katch/internal/model/entity/git_entity"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/gitsmart"
	"github.com/CodFrm/katch/internal/repository/git_repo"
	mock_git_repo "github.com/CodFrm/katch/internal/repository/git_repo/mock"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
	mock_upstream_repo "github.com/CodFrm/katch/internal/repository/upstream_repo/mock"
	"github.com/CodFrm/katch/internal/service/event_svc"
	"github.com/CodFrm/katch/internal/service/setting_svc"
)

// 镜像记录用 mockgen 的 mock 背一张内存表（约定 7，同 cache_svc 的 fakeRepo）：
// 要验的是状态之间的转移与「同一个仓库只建一次」，逐次 EXPECT 写不出这种断言。

// fakeMirrorRepo 背在内存里的镜像表。
type fakeMirrorRepo struct {
	mu     sync.Mutex
	nextID int64
	rows   map[int64]*git_entity.GitMirror
	// saves 每条记录被写了几次，用来数状态转移的次数。
	saves int
}

func newFakeMirrorRepo(t *testing.T) *fakeMirrorRepo {
	t.Helper()
	f := &fakeMirrorRepo{nextID: 1, rows: map[int64]*git_entity.GitMirror{}}
	m := mock_git_repo.NewMockGitMirrorRepo(gomock.NewController(t))
	m.EXPECT().FindByRepo(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(_ context.Context, host, repo string) (*git_entity.GitMirror, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, row := range f.rows {
				if row.Host == host && row.Repo == repo {
					copied := *row
					return &copied, nil
				}
			}
			return nil, nil
		})
	m.EXPECT().Save(gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(_ context.Context, mirror *git_entity.GitMirror) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if mirror.ID == 0 {
				mirror.ID = f.nextID
				f.nextID++
			}
			copied := *mirror
			f.rows[mirror.ID] = &copied
			f.saves++
			return nil
		})
	m.EXPECT().Touch(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(_ context.Context, id int64, at int64) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if row, ok := f.rows[id]; ok {
				row.LastAccessAt = at
			}
			return nil
		})
	m.EXPECT().Find(gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(_ context.Context, id int64) (*git_entity.GitMirror, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			row, ok := f.rows[id]
			if !ok {
				return nil, nil
			}
			copied := *row
			return &copied, nil
		})
	m.EXPECT().Delete(gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(_ context.Context, id int64) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			delete(f.rows, id)
			return nil
		})
	m.EXPECT().List(gomock.Any()).AnyTimes().
		DoAndReturn(func(context.Context) ([]*git_entity.GitMirror, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			list := make([]*git_entity.GitMirror, 0, len(f.rows))
			for _, row := range f.rows {
				copied := *row
				list = append(list, &copied)
			}
			sort.Slice(list, func(i, j int) bool {
				return list[i].LastAccessAt > list[j].LastAccessAt
			})
			return list, nil
		})
	m.EXPECT().TotalSize(gomock.Any()).AnyTimes().
		DoAndReturn(func(context.Context) (int64, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			var total int64
			for _, row := range f.rows {
				if row.State == git_entity.MirrorReady {
					total += row.SizeBytes
				}
			}
			return total, nil
		})
	m.EXPECT().EvictCandidates(gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(_ context.Context, limit int) ([]*git_entity.GitMirror, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			list := make([]*git_entity.GitMirror, 0, len(f.rows))
			for _, row := range f.rows {
				if row.State == git_entity.MirrorReady {
					copied := *row
					list = append(list, &copied)
				}
			}
			sort.Slice(list, func(i, j int) bool {
				return list[i].LastAccessAt < list[j].LastAccessAt
			})
			if len(list) > limit {
				list = list[:limit]
			}
			return list, nil
		})
	before := git_repo.GitMirror()
	git_repo.RegisterGitMirror(m)
	t.Cleanup(func() { git_repo.RegisterGitMirror(before) })
	return f
}

// find 取出一条记录此刻的样子，没有时给 nil。
func (f *fakeMirrorRepo) find(host, repo string) *git_entity.GitMirror {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, row := range f.rows {
		if row.Host == host && row.Repo == repo {
			copied := *row
			return &copied
		}
	}
	return nil
}

// save 直接往这张表里放一条记录，用来摆出「已经有一份建好的镜像」这个起点。
func (f *fakeMirrorRepo) save(row *git_entity.GitMirror) error {
	return git_repo.GitMirror().Save(context.Background(), row)
}

func (f *fakeMirrorRepo) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

// registerUpstream 让白名单里有这么一条上游。enabled 为假时它等同于不存在。
func registerUpstream(t *testing.T, host string, enabled bool, protocols ...string) {
	t.Helper()
	registerUpstreamRow(t, &upstream_entity.Upstream{
		ID: 7, Host: host, Origin: "https://" + host,
		Enabled: enabled, Protocols: upstream_entity.ProtocolSet(protocols),
	})
}

// registerUpstreamWithOrigin 同上，但回源地址由用例自己给。
//
// 跑真 builder 的那几条要把回源地址指到进程内那台假上游上，而不是一个真的
// https 主机——那样一次建镜像就会打到公网去。
func registerUpstreamWithOrigin(t *testing.T, host, origin string, protocols ...string) {
	t.Helper()
	registerUpstreamRow(t, &upstream_entity.Upstream{
		ID: 7, Host: host, Origin: origin,
		Enabled: true, Protocols: upstream_entity.ProtocolSet(protocols),
	})
}

func registerUpstreamRow(t *testing.T, row *upstream_entity.Upstream) {
	t.Helper()
	m := mock_upstream_repo.NewMockUpstreamRepo(gomock.NewController(t))
	m.EXPECT().FindByHost(gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(_ context.Context, want string) (*upstream_entity.Upstream, error) {
			if want != row.Host {
				return nil, nil
			}
			copied := *row
			return &copied, nil
		})
	before := upstream_repo.Upstream()
	upstream_repo.RegisterUpstream(m)
	t.Cleanup(func() { upstream_repo.RegisterUpstream(before) })
}

// fakeEvents 收下事件流上的每一条，用来验「每次状态变化都进了时间线」。
type fakeEvents struct {
	mu   sync.Mutex
	list []*event_svc.RecordInput
}

func captureEvents(t *testing.T) *fakeEvents {
	t.Helper()
	f := &fakeEvents{}
	before := event_svc.Event()
	event_svc.Register(f)
	t.Cleanup(func() { event_svc.Register(before) })
	return f
}

func (f *fakeEvents) Record(_ context.Context, in *event_svc.RecordInput) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.list = append(f.list, in)
}

func (f *fakeEvents) List(context.Context, *admin.ListEventsRequest) (*admin.ListEventsResponse, error) {
	return nil, nil
}

// kinds 事件流上出现过的类别，按发生顺序。
func (f *fakeEvents) kinds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.list))
	for _, in := range f.list {
		out = append(out, in.Kind)
	}
	return out
}

// detailOf 取某一类事件的细节。
func (f *fakeEvents) detailOf(kind string) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, in := range f.list {
		if in.Kind == kind {
			return in.Detail
		}
	}
	return nil
}

// upstreamIDOf 取某一类事件挂在哪个上游上。
func (f *fakeEvents) upstreamIDOf(kind string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, in := range f.list {
		if in.Kind == kind {
			return in.UpstreamID
		}
	}
	return 0
}

// actorOf 取某一类事件的操作人。
func (f *fakeEvents) actorOf(kind string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, in := range f.list {
		if in.Kind == kind {
			return in.Actor
		}
	}
	return event_entity.ActorSystem
}

// fakeBuilder 顶替真正的 go-git：状态机的用例不该依赖一次真实的 clone。
type fakeBuilder struct {
	mu sync.Mutex
	// calls 每个远端被真正建了几次镜像。
	calls map[string]int
	// reqs 收下每一次请求，用来验路径与上限是怎么传下去的。
	reqs []BuildRequest
	// gate 非 nil 时，每次 Build 先等它——「并发合并成一次」要按住第一趟活
	// 不放，才数得出后面那些请求有没有各自又建一次。
	gate chan struct{}
	// started 每开始一趟 Build 就发一下，用例据此知道第一趟已经进门了。
	started chan struct{}
	// result 这一趟的结果，nil 表示成功、体积 1024。
	result func(req *BuildRequest) (int64, error)
	// ctxDeadlines 每一趟拿到的 ctx 上有没有 deadline。
	//
	// 建镜像的时限**不该**挂在 ctx 上：go-git 的 delta 解析阶段整个不看它
	// （Problem 4），那条路在实测卡死的那一段上形同虚设。
	ctxDeadlines []bool

	// 下面四项是增量同步那一侧的同名物件，和建镜像分开数：本地应答要验的
	// 恰恰是「这一次到底同步了没有」，混在一个计数里就分不出来了。
	syncs      int
	syncReqs   []SyncRequest
	syncGate   chan struct{}
	syncResult func(req *SyncRequest) (int64, error)
	// syncStarted 每开始一趟同步就发一下。
	syncStarted chan struct{}
}

func newFakeBuilder() *fakeBuilder {
	return &fakeBuilder{
		calls:       map[string]int{},
		started:     make(chan struct{}, 64),
		syncStarted: make(chan struct{}, 64),
	}
}

func (b *fakeBuilder) Build(ctx context.Context, req *BuildRequest) (int64, error) {
	_, hasDeadline := ctx.Deadline()
	b.mu.Lock()
	b.calls[req.Remote]++
	b.reqs = append(b.reqs, *req)
	b.ctxDeadlines = append(b.ctxDeadlines, hasDeadline)
	gate, result := b.gate, b.result
	b.mu.Unlock()
	select {
	case b.started <- struct{}{}:
	default:
	}
	if gate != nil {
		// 认 ctx，同 Sync：调用方要是给了一个带时限的 ctx，这趟活就该被它掐断。
		select {
		case <-gate:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	if result != nil {
		return result(req)
	}
	return 1024, nil
}

// lastBuildHadDeadline 最后一趟建镜像拿到的 ctx 上有没有 deadline。
func (b *fakeBuilder) lastBuildHadDeadline() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.ctxDeadlines) == 0 {
		return false
	}
	return b.ctxDeadlines[len(b.ctxDeadlines)-1]
}

func (b *fakeBuilder) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	total := 0
	for _, n := range b.calls {
		total += n
	}
	return total
}

func (b *fakeBuilder) lastRequest() BuildRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.reqs) == 0 {
		return BuildRequest{}
	}
	return b.reqs[len(b.reqs)-1]
}

// fakeRuntime 用例侧的运行时设置，基线取出厂值（同 cache_svc 的那一个）。
type fakeRuntime struct {
	mu sync.Mutex
	rt *setting_svc.RuntimeSettings
}

func newFakeRuntime(t *testing.T, patch func(rt *setting_svc.RuntimeSettings)) *fakeRuntime {
	t.Helper()
	rt, err := setting_svc.Setting().Runtime(context.Background())
	if err != nil {
		t.Fatalf("取出厂运行时设置失败：%v", err)
	}
	if patch != nil {
		patch(rt)
	}
	return &fakeRuntime{rt: rt}
}

func (f *fakeRuntime) Runtime(context.Context) (*setting_svc.RuntimeSettings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	snapshot := *f.rt
	return &snapshot, nil
}

// set 改一项设置，下一次操作就该按新值走。
func (f *fakeRuntime) set(patch func(rt *setting_svc.RuntimeSettings)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	patch(f.rt)
}

func (b *fakeBuilder) Sync(ctx context.Context, req *SyncRequest) (int64, error) {
	b.mu.Lock()
	b.syncs++
	b.syncReqs = append(b.syncReqs, *req)
	gate, result := b.syncGate, b.syncResult
	b.mu.Unlock()
	select {
	case b.syncStarted <- struct{}{}:
	default:
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	if result != nil {
		// 结果函数可能永远不返回（验同步超时的那一条），超时由 ctx 负责掐断。
		type outcome struct {
			size int64
			err  error
		}
		done := make(chan outcome, 1)
		go func() {
			size, err := result(req)
			done <- outcome{size, err}
		}()
		select {
		case got := <-done:
			return got.size, got.err
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return 2048, nil
}

func (b *fakeBuilder) syncCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.syncs
}

func (b *fakeBuilder) lastSync() SyncRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.syncReqs) == 0 {
		return SyncRequest{}
	}
	return b.syncReqs[len(b.syncReqs)-1]
}

// fakeAnswerer 顶替 gitsmart：TTL 与降级的用例不该依赖一次真的打包。
//
// 它记下被调用时上游已经同步了几次——「先同步再广播」这条要验的是顺序，
// 而顺序只能在应答发生的那一刻回头看。
type fakeAnswerer struct {
	mu      sync.Mutex
	builder *fakeBuilder
	reqs    []DiskRequest
	// syncsAt 每一次应答发生时的同步计数。
	syncsAt []int
	body    string
	err     error
}

func newFakeAnswerer(builder *fakeBuilder, body string) *fakeAnswerer {
	return &fakeAnswerer{builder: builder, body: body}
}

func (a *fakeAnswerer) Answer(_ context.Context, req *DiskRequest) (*Answer, error) {
	a.mu.Lock()
	a.reqs = append(a.reqs, *req)
	a.syncsAt = append(a.syncsAt, a.builder.syncCount())
	err, body := a.err, a.body
	a.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &Answer{
		ContentType: "application/x-git-upload-pack-advertisement",
		Body:        io.NopCloser(strings.NewReader(body)),
	}, nil
}

func (a *fakeAnswerer) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.reqs)
}

func (a *fakeAnswerer) lastRequest() DiskRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.reqs) == 0 {
		return DiskRequest{}
	}
	return a.reqs[len(a.reqs)-1]
}

// syncsBefore 第一次应答发生时，上游已经被同步了几次。
func (a *fakeAnswerer) syncsBefore() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.syncsAt) == 0 {
		return -1
	}
	return a.syncsAt[0]
}

// withAnswerer 把应答那一侧换成用例的替身。
func withAnswerer(svc MirrorSvc, answerer Answerer) {
	svc.(*mirrorSvc).answerer = answerer
}

// gitsmartNotAnswerable 「本地答不了」那个哨兵，由 gitsmart 给出。
func gitsmartNotAnswerable() error {
	return gitsmart.ErrNotAnswerable
}
