package cache_svc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/CodFrm/katch/internal/cache"
	"github.com/CodFrm/katch/internal/model/entity/cache_entity"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/dispatch"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
	"github.com/CodFrm/katch/internal/repository/cache_repo"
	mock_cache_repo "github.com/CodFrm/katch/internal/repository/cache_repo/mock"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
	mock_upstream_repo "github.com/CodFrm/katch/internal/repository/upstream_repo/mock"
	"github.com/CodFrm/katch/internal/service/proxy_svc"
	"github.com/CodFrm/katch/internal/service/setting_svc"
)

var errPromote = errors.New("promote failed")

type testProfile struct {
	description    packageprofile.Description
	representation packageprofile.Representation
	transform      func(context.Context, packageprofile.TransformRequest) (*packageprofile.TransformResult, error)
}

func (p testProfile) Describe() packageprofile.Description { return p.description }
func (p testProfile) Classify(packageprofile.Request) packageprofile.Representation {
	return p.representation
}
func (p testProfile) Transform(ctx context.Context, in packageprofile.TransformRequest) (*packageprofile.TransformResult, error) {
	return p.transform(ctx, in)
}
func (testProfile) Companions() []packageprofile.Companion { return nil }
func (testProfile) Guidance() packageprofile.Guidance      { return packageprofile.Guidance{} }

type fixedRewriteSource struct {
	snapshot *proxy_svc.RewriteSnapshot
}

func (s fixedRewriteSource) Snapshot(context.Context) (*proxy_svc.RewriteSnapshot, error) {
	return s.snapshot, nil
}

func transformingOptions(t *testing.T, profile testProfile, generation int64) Options {
	t.Helper()
	profiles := packageprofile.NewRegistry()
	if err := profiles.Register(profile); err != nil {
		t.Fatal(err)
	}
	return Options{
		Profiles: profiles,
		RewriteConfig: fixedRewriteSource{snapshot: &proxy_svc.RewriteSnapshot{
			SiteBaseURL: "https://katch.example.com",
			Generation:  generation,
			Upstreams:   map[string]proxy_svc.RewriteUpstream{},
		}},
	}
}

// fakeRuntime 用例侧的运行时设置。
//
// 基线从 setting_svc 取而不是在这里另抄一份出厂值：两份兜底值一旦分叉，用例会在
// 生产默认值改掉之后继续绿着。仓储没注册时 Runtime 给的就是出厂值，不碰库。
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

// 假源站用 httptest，不打任何真实网络；缓存记录用 mockgen 生成的 mock，
// 但让它背一个内存表——LRU、过期、并发合并这些行为要的是「记录之间的先后」，
// 逐次 EXPECT 断言写不出这种状态。

// fakeRepo 给 mock 背的内存表。
//
// last_access_at 由它自己盖一个单调递增的序号，而不是用真实秒级时间戳：
// 同一秒内写进去的几条记录在真时间戳下分不出先后，LRU 的顺序就成了掷骰子。
type fakeRepo struct {
	mu     sync.Mutex
	seq    int64
	nextID int64
	rows   map[int64]*cache_entity.CacheObject
	// stampAccess 为真时由内存表接管 last_access_at，见上。
	stampAccess   bool
	promoteErr    error
	deleteStarted chan struct{}
	deleteGate    chan struct{}
	// totalSizeGate 非 nil 时，TotalSize 会先等它。
	//
	// TotalSize 只有 enforceQuota 一个调用方，而 enforceQuota 只跑在 pump 那个
	// 后台协程上，所以它是「后台写缓存这件事还没做完」唯一一个不靠睡眠就按得住的缝。
	totalSizeGate chan struct{}
}

func newFakeRepo(t *testing.T, stampAccess bool) *fakeRepo {
	t.Helper()
	f := &fakeRepo{nextID: 1, rows: map[int64]*cache_entity.CacheObject{}, stampAccess: stampAccess}
	m := mock_cache_repo.NewMockCacheObjectRepo(gomock.NewController(t))
	m.EXPECT().Find(gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(_ any, id int64) (*cache_entity.CacheObject, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return clone(f.rows[id]), nil
		})
	m.EXPECT().FindByKey(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(_ any, upstreamID int64, key string) (*cache_entity.CacheObject, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, row := range f.rows {
				if row.UpstreamID == upstreamID && row.Key == key {
					return clone(row), nil
				}
			}
			return nil, nil
		})
	m.EXPECT().Save(gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(_ any, obj *cache_entity.CacheObject) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if obj.ID == 0 {
				obj.ID = f.nextID
				f.nextID++
			}
			if f.stampAccess {
				f.seq++
				obj.LastAccessAt = f.seq
			}
			f.rows[obj.ID] = clone(obj)
			return nil
		})
	m.EXPECT().Delete(gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(_ any, id int64) error {
			f.waitDelete()
			f.mu.Lock()
			defer f.mu.Unlock()
			delete(f.rows, id)
			return nil
		})
	m.EXPECT().DeleteExpired(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(_ any, id, before int64) (bool, error) {
			f.waitDelete()
			f.mu.Lock()
			defer f.mu.Unlock()
			row, ok := f.rows[id]
			if !ok || row.Immutable || row.ExpiresAt <= 0 || row.ExpiresAt > before || row.Pinned {
				return false, nil
			}
			delete(f.rows, id)
			return true, nil
		})
	m.EXPECT().Touch(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(_ any, id int64, at int64) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			row, ok := f.rows[id]
			if !ok {
				return nil
			}
			row.HitCount++
			row.LastAccessAt = at
			if f.stampAccess {
				f.seq++
				row.LastAccessAt = f.seq
			}
			return nil
		})
	m.EXPECT().PromoteImmutable(gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(_ any, id int64) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.promoteErr != nil {
				return f.promoteErr
			}
			if row, ok := f.rows[id]; ok {
				row.Immutable = true
				row.ExpiresAt = 0
			}
			return nil
		})
	m.EXPECT().SetPinned(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(_ any, id int64, pinned bool) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if row, ok := f.rows[id]; ok {
				row.Pinned = pinned
			}
			return nil
		})
	m.EXPECT().TotalSize(gomock.Any()).AnyTimes().DoAndReturn(func(_ any) (int64, error) {
		f.mu.Lock()
		gate := f.totalSizeGate
		f.mu.Unlock()
		if gate != nil {
			<-gate
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		total := int64(0)
		for _, row := range f.rows {
			total += row.Size
		}
		return total, nil
	})
	m.EXPECT().EvictCandidates(gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(_ any, limit int) ([]*cache_entity.CacheObject, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			list := make([]*cache_entity.CacheObject, 0, limit)
			for _, row := range f.rows {
				if row.Immutable && !row.Pinned {
					list = append(list, clone(row))
				}
			}
			sortByAccess(list)
			if len(list) > limit {
				list = list[:limit]
			}
			return list, nil
		})
	m.EXPECT().ExpiredBefore(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(_ any, before int64, limit int) ([]*cache_entity.CacheObject, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			list := make([]*cache_entity.CacheObject, 0, limit)
			for _, row := range f.rows {
				// 和 SQL 一样：只收已过期、可变且未 pin 的记录。
				if row.ExpiresAt > 0 && row.ExpiresAt <= before && !row.Immutable && !row.Pinned {
					list = append(list, clone(row))
				}
			}
			sort.Slice(list, func(i, j int) bool { return list[i].ExpiresAt < list[j].ExpiresAt })
			if len(list) > limit {
				list = list[:limit]
			}
			return list, nil
		})
	m.EXPECT().CountByUpstream(gomock.Any()).AnyTimes().
		DoAndReturn(func(_ any) (map[int64]int64, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			ret := map[int64]int64{}
			for _, row := range f.rows {
				ret[row.UpstreamID]++
			}
			return ret, nil
		})
	m.EXPECT().CountByDigest(gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(_ any, digest string) (int64, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			n := int64(0)
			for _, row := range f.rows {
				if row.Digest == digest {
					n++
				}
			}
			return n, nil
		})
	m.EXPECT().Search(gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(_ any, opt *cache_entity.SearchOption) ([]*cache_entity.CacheObject, int64, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			list := make([]*cache_entity.CacheObject, 0, len(f.rows))
			for _, row := range f.rows {
				if opt.UpstreamID > 0 && row.UpstreamID != opt.UpstreamID {
					continue
				}
				list = append(list, clone(row))
			}
			sortByAccess(list)
			return list, int64(len(list)), nil
		})
	m.EXPECT().ListByUpstream(gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(_ any, upstreamID int64) ([]*cache_entity.CacheObject, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			list := make([]*cache_entity.CacheObject, 0)
			for _, row := range f.rows {
				if row.UpstreamID == upstreamID {
					list = append(list, clone(row))
				}
			}
			return list, nil
		})
	// 用完还原。cache_repo 的注册是进程级的一份，只写不还的话，用例之间就靠
	// 「谁后跑谁说了算」联系在一起：这个用例留下的后台协程会拿着**下一个**用例的
	// 仓储去读写，而两边的断言各自看起来都还成立。由 harness_test.go 守着。
	prev := cache_repo.CacheObject()
	cache_repo.RegisterCacheObject(m)
	t.Cleanup(func() { cache_repo.RegisterCacheObject(prev) })
	return f
}

func sortByAccess(list []*cache_entity.CacheObject) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].LastAccessAt < list[j-1].LastAccessAt; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}

func clone(src *cache_entity.CacheObject) *cache_entity.CacheObject {
	if src == nil {
		return nil
	}
	dst := *src
	return &dst
}

func (f *fakeRepo) all() []*cache_entity.CacheObject {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := make([]*cache_entity.CacheObject, 0, len(f.rows))
	for _, row := range f.rows {
		list = append(list, clone(row))
	}
	sortByAccess(list)
	return list
}

// expire 把一条可变记录的过期时刻拨到过去，免得用例真的去睡一个 TTL。
func (f *fakeRepo) waitDelete() {
	f.mu.Lock()
	started, gate := f.deleteStarted, f.deleteGate
	f.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if gate != nil {
		<-gate
	}
}

func (f *fakeRepo) setPromoteError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.promoteErr = err
}

func (f *fakeRepo) expire(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, row := range f.rows {
		if row.Key == key {
			row.ExpiresAt = time.Now().Unix() - 1
		}
	}
}

func (f *fakeRepo) byKey(key string) *cache_entity.CacheObject {
	for _, row := range f.all() {
		if row.Key == key {
			return row
		}
	}
	return nil
}

// originStub 假源站，记下被打了几次。
type originStub struct {
	srv  *httptest.Server
	hits atomic.Int64
}

type localOriginProxy struct {
	baseURL string
}

func (p localOriginProxy) Fetch(ctx context.Context, target *proxy_svc.Target) (io.ReadCloser, *proxy_svc.Meta, error) {
	request, err := http.NewRequestWithContext(ctx, target.Method, p.baseURL+target.Path, target.Body)
	if err != nil {
		return nil, nil, err
	}
	request.URL.RawQuery = target.RawQuery
	request.Header = target.Header.Clone()
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, nil, err
	}
	var sourceURL *url.URL
	if response.Request != nil && response.Request.URL != nil {
		cloned := *response.Request.URL
		sourceURL = &cloned
	}
	return response.Body, &proxy_svc.Meta{
		StatusCode: response.StatusCode, Header: response.Header.Clone(), ContentLength: response.ContentLength,
		SourceURL: sourceURL,
	}, nil
}

func newOrigin(t *testing.T, handler http.HandlerFunc) *originStub {
	t.Helper()
	o := &originStub{}
	o.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(o.srv.Close)
	return o
}

// setupSvc 装一套完整的缓存层：假源站 + 一条上游 + 内存缓存表 + 真磁盘目录。
func setupSvc(t *testing.T, o *originStub, up *upstream_entity.Upstream, opt Options) (CacheSvc, *fakeRepo, *cache.Store) {
	t.Helper()
	repo := newFakeRepo(t, true)
	upRepo := mock_upstream_repo.NewMockUpstreamRepo(gomock.NewController(t))
	up.Origin = o.srv.URL
	up.Enabled = true
	// 装配形态与 main 一致：读路径走带进程内缓存的那一层。
	// 同样是进程全局，同样要还原，理由见 newFakeRepo 里那段。
	prevUpstream := upstream_repo.Upstream()
	upstream_repo.RegisterUpstream(proxy_svc.NewCachedUpstreamRepo(upRepo))
	t.Cleanup(func() { upstream_repo.RegisterUpstream(prevUpstream) })
	upRepo.EXPECT().List(gomock.Any()).Return([]*upstream_entity.Upstream{up}, nil).AnyTimes()
	if opt.Profiles != nil {
		previousProxy := proxy_svc.Proxy()
		proxy_svc.Register(localOriginProxy{baseURL: o.srv.URL})
		t.Cleanup(func() { proxy_svc.Register(previousProxy) })
	}

	store, err := cache.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("建缓存目录失败：%v", err)
	}
	svc := New(store, opt)
	// 等后台下载收尾再让用例结束。
	//
	// 缓存写入是脱离客户端跑的，所以「客户端收完了」不等于「这趟活干完了」：
	// pump 在 finish 之后还要跑一趟 enforceQuota，而那里读的是进程全局的
	// cache_repo。不等它，这个协程就会活到下一个用例里去读写上面刚刚还原过的
	// 那份全局——CI 上的 DATA RACE 就是这么来的。
	//
	// 这条 Cleanup 注册在两条还原之后，于是 LIFO 下它**先**跑：先把人等回来，
	// 再把全局换回去，顺序反了等于没等。
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), quiesceTimeout)
		defer cancel()
		if err := svc.Quiesce(ctx); err != nil {
			// 不是 Fatal：Cleanup 里 FailNow 不会中断别的收尾，而这条信息本身
			// 就够定位了——有一趟后台下载在这个时限内没能收尾。
			t.Errorf("后台缓存写入没能在 %s 内收尾：%v", quiesceTimeout, err)
		}
	})
	return svc, repo, store
}

// quiesceTimeout 用例结束时留给后台缓存写入的收尾窗口。
//
// 取一个明显大于任何一条用例正常耗时的值：这里等满了只说明有协程卡住了，
// 那是缺陷而不是慢，应该把用例判红而不是接着等。
const quiesceTimeout = 30 * time.Second

func staticUpstream(host string) *upstream_entity.Upstream {
	return &upstream_entity.Upstream{
		ID: 7, Host: host, Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
		ImmutablePatterns: upstream_entity.PatternList{"/pool/"},
		MutableTTLSeconds: 60,
	}
}

func target(host, path string) *proxy_svc.Target {
	return &proxy_svc.Target{
		Kind: dispatch.KindStatic, Host: host, Path: path,
		Method: http.MethodGet, Header: http.Header{},
	}
}

// truncateBlob 把盘上的副本截短，模拟「记录说有 N 字节、文件却只剩几字节」的坏副本。
func truncateBlob(t *testing.T, store *cache.Store, repo *fakeRepo, key string, size int64) {
	t.Helper()
	row := repo.byKey(key)
	if row == nil {
		t.Fatalf("没有 %s 的缓存记录", key)
	}
	f, _, err := store.Open(row.Digest)
	if err != nil {
		t.Fatalf("打开副本失败：%v", err)
	}
	name := f.Name()
	_ = f.Close()
	if err := os.Truncate(name, size); err != nil {
		t.Fatalf("截断副本失败：%v", err)
	}
}

// digestOfString 用例侧算内容摘要，用来直接问磁盘「这份内容还在不在」。
func digestOfString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// corruptBlob 原地改写盘上的副本，但**保持字节数不变**：这类损坏躲得过大小比对，
// 只有按内容摘要校验才认得出来。
func corruptBlob(t *testing.T, store *cache.Store, repo *fakeRepo, key string) {
	t.Helper()
	row := repo.byKey(key)
	if row == nil {
		t.Fatalf("没有 %s 的缓存记录", key)
	}
	f, size, err := store.Open(row.Digest)
	if err != nil {
		t.Fatalf("打开副本失败：%v", err)
	}
	name := f.Name()
	_ = f.Close()
	if err := os.WriteFile(name, []byte(strings.Repeat("X", int(size))), 0o600); err != nil {
		t.Fatalf("改写副本失败：%v", err)
	}
}

// quotaOf 只改配额与回收水位的那种补丁。
func quotaOf(quota int64, percent int) func(rt *setting_svc.RuntimeSettings) {
	return func(rt *setting_svc.RuntimeSettings) {
		rt.CacheQuotaBytes = quota
		rt.CacheReclaimPercent = percent
	}
}
