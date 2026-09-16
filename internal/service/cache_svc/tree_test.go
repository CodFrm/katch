package cache_svc

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/cago-frame/cago/pkg/utils/httputils"
	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"

	"github.com/CodFrm/katch/internal/model/entity/cache_entity"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/pkg/code"
	"github.com/CodFrm/katch/internal/repository/cache_repo"
	mock_cache_repo "github.com/CodFrm/katch/internal/repository/cache_repo/mock"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
	mock_upstream_repo "github.com/CodFrm/katch/internal/repository/upstream_repo/mock"
)

// setupTree 目录树只碰记录表与上游表，不碰盘：两个 repo 都是逐次 EXPECT 的 mock，
// 没 EXPECT 过的调用会让用例当场失败——「非法路径不查库」就靠这一点断言。
func setupTree(t *testing.T) (CacheSvc, *mock_cache_repo.MockCacheObjectRepo, *mock_upstream_repo.MockUpstreamRepo) {
	t.Helper()
	ctrl := gomock.NewController(t)
	cacheRepo := mock_cache_repo.NewMockCacheObjectRepo(ctrl)
	upRepo := mock_upstream_repo.NewMockUpstreamRepo(ctrl)
	prevCache, prevUpstream := cache_repo.CacheObject(), upstream_repo.Upstream()
	cache_repo.RegisterCacheObject(cacheRepo)
	upstream_repo.RegisterUpstream(upRepo)
	t.Cleanup(func() {
		cache_repo.RegisterCacheObject(prevCache)
		upstream_repo.RegisterUpstream(prevUpstream)
	})
	return New(nil, Options{Runtime: newFakeRuntime(t, nil)}), cacheRepo, upRepo
}

func treeUpstreams() []*upstream_entity.Upstream {
	return []*upstream_entity.Upstream{
		{ID: 7, Host: "registry-1.docker.io"},
		{ID: 8, Host: "deb.debian.org"},
	}
}

func shouldBeTreeCode(err error, want int) {
	var e *httputils.Error
	convey.So(errors.As(err, &e), convey.ShouldBeTrue)
	convey.So(e.Status, convey.ShouldEqual, http.StatusBadRequest)
	convey.So(e.Code, convey.ShouldEqual, want)
}

func TestTree_Root(t *testing.T) {
	convey.Convey("树根列出有缓存对象的上游主机，按主机名排序", t, func() {
		svc, cacheRepo, upRepo := setupTree(t)
		cacheRepo.EXPECT().StatByUpstream(gomock.Any()).Return([]*cache_entity.UpstreamTreeStat{
			{UpstreamID: 7, Count: 3, Pinned: 1, Size: 300, LastAccessAt: 50},
			{UpstreamID: 8, Count: 2, Pinned: 0, Size: 20, LastAccessAt: 90},
			// 上游已经删掉的记录在树上没有位置，也不进合计。
			{UpstreamID: 99, Count: 1000, Size: 1 << 30},
		}, nil)
		upRepo.EXPECT().List(gomock.Any()).Return(treeUpstreams(), nil)

		resp, err := svc.Tree(context.Background(), &TreeRequest{})
		convey.So(err, convey.ShouldBeNil)
		convey.So(resp.Path, convey.ShouldEqual, "")
		convey.So(resp.TotalCount, convey.ShouldEqual, 5)
		convey.So(resp.TotalPinned, convey.ShouldEqual, 1)
		convey.So(resp.TotalSize, convey.ShouldEqual, 320)
		convey.So(resp.HasMore, convey.ShouldBeFalse)
		convey.So(resp.NextOffset, convey.ShouldEqual, 2)
		convey.So(len(resp.Children), convey.ShouldEqual, 2)
		convey.So(*resp.Children[0], convey.ShouldResemble, TreeNode{
			Kind: TreeKindDir, Name: "deb.debian.org", Path: "deb.debian.org",
			Count: 2, Size: 20, LastAccessAt: 90})
		convey.So(resp.Children[1].Name, convey.ShouldEqual, "registry-1.docker.io")
		convey.So(resp.Children[1].PinnedCount, convey.ShouldEqual, 1)
	})
}

func TestTree_Level(t *testing.T) {
	convey.Convey("一层：目录在前、对象在后；查询串不参与分段，变体段剥离并标记", t, func() {
		svc, cacheRepo, upRepo := setupTree(t)
		upRepo.EXPECT().FindByHost(gomock.Any(), "deb.debian.org").Return(treeUpstreams()[1], nil)
		cacheRepo.EXPECT().StatByPrefix(gomock.Any(), int64(8), "/pool/").Return(&cache_entity.PrefixTreeStat{
			Count: 5, Pinned: 1, Size: 500, LastAccessAt: 9, Dirs: 1, Objects: 2}, nil)
		cacheRepo.EXPECT().ListTreeDirs(gomock.Any(), &cache_entity.TreeOption{
			UpstreamID: 8, Prefix: "/pool/", Offset: 0, Limit: 200}).
			Return([]*cache_entity.TreeDir{{Name: "main", Count: 3, Pinned: 1, Size: 300, LastAccessAt: 9}}, nil)
		variant := &cache_entity.CacheObject{ID: 2, UpstreamID: 8, Key: "/pool/y.deb\x1faccept=abc"}
		query := &cache_entity.CacheObject{ID: 1, UpstreamID: 8, Key: "/pool/x.deb?q=/a/b"}
		cacheRepo.EXPECT().ListTreeObjects(gomock.Any(), &cache_entity.TreeOption{
			UpstreamID: 8, Prefix: "/pool/", Offset: 0, Limit: 199}).
			Return([]*cache_entity.CacheObject{query, variant}, nil)

		resp, err := svc.Tree(context.Background(), &TreeRequest{Path: "deb.debian.org/pool"})
		convey.So(err, convey.ShouldBeNil)
		convey.So(resp.Path, convey.ShouldEqual, "deb.debian.org/pool")
		convey.So(resp.TotalCount, convey.ShouldEqual, 5)
		convey.So(resp.TotalPinned, convey.ShouldEqual, 1)
		convey.So(resp.TotalSize, convey.ShouldEqual, 500)
		convey.So(resp.HasMore, convey.ShouldBeFalse)
		convey.So(resp.NextOffset, convey.ShouldEqual, 3)
		convey.So(len(resp.Children), convey.ShouldEqual, 3)
		convey.So(*resp.Children[0], convey.ShouldResemble, TreeNode{
			Kind: TreeKindDir, Name: "main", Path: "deb.debian.org/pool/main",
			Count: 3, PinnedCount: 1, Size: 300, LastAccessAt: 9})
		convey.So(*resp.Children[1], convey.ShouldResemble, TreeNode{
			Kind: TreeKindObject, Name: "x.deb?q=/a/b", Path: "deb.debian.org/pool/x.deb?q=/a/b",
			Host: "deb.debian.org", Object: query})
		convey.So(*resp.Children[2], convey.ShouldResemble, TreeNode{
			Kind: TreeKindObject, Name: "y.deb", Path: "deb.debian.org/pool/y.deb",
			Variant: true, Host: "deb.debian.org", Object: variant})
	})

	convey.Convey("上游根用 / 作前缀", t, func() {
		svc, cacheRepo, upRepo := setupTree(t)
		upRepo.EXPECT().FindByHost(gomock.Any(), "deb.debian.org").Return(treeUpstreams()[1], nil)
		cacheRepo.EXPECT().StatByPrefix(gomock.Any(), int64(8), "/").Return(&cache_entity.PrefixTreeStat{
			Count: 1, Objects: 1}, nil)
		cacheRepo.EXPECT().ListTreeObjects(gomock.Any(), &cache_entity.TreeOption{
			UpstreamID: 8, Prefix: "/", Offset: 0, Limit: 200}).
			Return([]*cache_entity.CacheObject{{ID: 1, UpstreamID: 8, Key: "/InRelease"}}, nil)

		resp, err := svc.Tree(context.Background(), &TreeRequest{Path: "deb.debian.org"})
		convey.So(err, convey.ShouldBeNil)
		convey.So(resp.Children[0].Path, convey.ShouldEqual, "deb.debian.org/InRelease")
	})
}

func TestTree_Paging(t *testing.T) {
	stat := &cache_entity.PrefixTreeStat{Count: 300, Dirs: 250, Objects: 10}
	dirs := func(n int) []*cache_entity.TreeDir {
		list := make([]*cache_entity.TreeDir, n)
		for i := range list {
			list[i] = &cache_entity.TreeDir{Name: "d"}
		}
		return list
	}

	convey.Convey("每次最多 200 项，目录还没列完时不查对象", t, func() {
		svc, cacheRepo, upRepo := setupTree(t)
		upRepo.EXPECT().FindByHost(gomock.Any(), "deb.debian.org").Return(treeUpstreams()[1], nil)
		cacheRepo.EXPECT().StatByPrefix(gomock.Any(), int64(8), "/pool/").Return(stat, nil)
		cacheRepo.EXPECT().ListTreeDirs(gomock.Any(), &cache_entity.TreeOption{
			UpstreamID: 8, Prefix: "/pool/", Offset: 0, Limit: 200}).Return(dirs(200), nil)

		resp, err := svc.Tree(context.Background(), &TreeRequest{Path: "deb.debian.org/pool"})
		convey.So(err, convey.ShouldBeNil)
		convey.So(len(resp.Children), convey.ShouldEqual, 200)
		convey.So(resp.HasMore, convey.ShouldBeTrue)
		convey.So(resp.NextOffset, convey.ShouldEqual, 200)
	})

	convey.Convey("下一批接着目录的尾巴列对象", t, func() {
		svc, cacheRepo, upRepo := setupTree(t)
		upRepo.EXPECT().FindByHost(gomock.Any(), "deb.debian.org").Return(treeUpstreams()[1], nil)
		cacheRepo.EXPECT().StatByPrefix(gomock.Any(), int64(8), "/pool/").Return(stat, nil)
		cacheRepo.EXPECT().ListTreeDirs(gomock.Any(), &cache_entity.TreeOption{
			UpstreamID: 8, Prefix: "/pool/", Offset: 200, Limit: 200}).Return(dirs(50), nil)
		cacheRepo.EXPECT().ListTreeObjects(gomock.Any(), &cache_entity.TreeOption{
			UpstreamID: 8, Prefix: "/pool/", Offset: 0, Limit: 150}).
			Return([]*cache_entity.CacheObject{{Key: "/pool/a"}, {Key: "/pool/b"}}, nil)

		resp, err := svc.Tree(context.Background(), &TreeRequest{Path: "deb.debian.org/pool", Offset: 200})
		convey.So(err, convey.ShouldBeNil)
		convey.So(len(resp.Children), convey.ShouldEqual, 52)
		convey.So(resp.HasMore, convey.ShouldBeTrue)
		convey.So(resp.NextOffset, convey.ShouldEqual, 252)
	})

	convey.Convey("偏移已经越过目录时只按对象的偏移查", t, func() {
		svc, cacheRepo, upRepo := setupTree(t)
		upRepo.EXPECT().FindByHost(gomock.Any(), "deb.debian.org").Return(treeUpstreams()[1], nil)
		cacheRepo.EXPECT().StatByPrefix(gomock.Any(), int64(8), "/pool/").Return(stat, nil)
		cacheRepo.EXPECT().ListTreeObjects(gomock.Any(), &cache_entity.TreeOption{
			UpstreamID: 8, Prefix: "/pool/", Offset: 5, Limit: 200}).
			Return([]*cache_entity.CacheObject{{Key: "/pool/f"}, {Key: "/pool/g"}, {Key: "/pool/h"},
				{Key: "/pool/i"}, {Key: "/pool/j"}}, nil)

		resp, err := svc.Tree(context.Background(), &TreeRequest{Path: "deb.debian.org/pool", Offset: 255})
		convey.So(err, convey.ShouldBeNil)
		convey.So(len(resp.Children), convey.ShouldEqual, 5)
		convey.So(resp.HasMore, convey.ShouldBeFalse)
		convey.So(resp.NextOffset, convey.ShouldEqual, 260)
	})
}

func TestTree_PagingStopsOnEmptyPage(t *testing.T) {
	convey.Convey("合计与列表之间对象被并发清掉时，空的一批不再报还有更多", t, func() {
		svc, cacheRepo, upRepo := setupTree(t)
		upRepo.EXPECT().FindByHost(gomock.Any(), "deb.debian.org").Return(treeUpstreams()[1], nil)
		cacheRepo.EXPECT().StatByPrefix(gomock.Any(), int64(8), "/pool/").
			Return(&cache_entity.PrefixTreeStat{Count: 10, Objects: 10}, nil)
		cacheRepo.EXPECT().ListTreeObjects(gomock.Any(), gomock.Any()).Return([]*cache_entity.CacheObject{}, nil)

		resp, err := svc.Tree(context.Background(), &TreeRequest{Path: "deb.debian.org/pool", Offset: 3})
		convey.So(err, convey.ShouldBeNil)
		convey.So(resp.HasMore, convey.ShouldBeFalse)
		convey.So(resp.NextOffset, convey.ShouldEqual, 3)
	})
}

func TestTree_EmptyAndInvalid(t *testing.T) {
	convey.Convey("不存在的主机或目录是空层，而不是错误", t, func() {
		svc, cacheRepo, upRepo := setupTree(t)
		upRepo.EXPECT().FindByHost(gomock.Any(), "gone.example").Return(nil, nil)
		upRepo.EXPECT().FindByHost(gomock.Any(), "deb.debian.org").Return(treeUpstreams()[1], nil)
		cacheRepo.EXPECT().StatByPrefix(gomock.Any(), int64(8), "/nope/").Return(&cache_entity.PrefixTreeStat{}, nil)

		for _, path := range []string{"gone.example/x", "deb.debian.org/nope"} {
			resp, err := svc.Tree(context.Background(), &TreeRequest{Path: path})
			convey.So(err, convey.ShouldBeNil)
			convey.So(resp.Path, convey.ShouldEqual, path)
			convey.So(resp.Children, convey.ShouldNotBeNil)
			convey.So(len(resp.Children), convey.ShouldEqual, 0)
			convey.So(resp.HasMore, convey.ShouldBeFalse)
		}
	})

	convey.Convey("..、空段与超长路径按参数错误拒绝，不查库", t, func() {
		svc, _, _ := setupTree(t)
		for _, path := range []string{"..", "deb.debian.org/../x", "/deb.debian.org", "deb.debian.org/",
			"deb.debian.org//pool", strings.Repeat("a", maxTreePathLen+1)} {
			_, err := svc.Tree(context.Background(), &TreeRequest{Path: path})
			shouldBeTreeCode(err, code.CacheTreePathInvalid)
			_, err = svc.TreeSearch(context.Background(), &TreeSearchRequest{Path: path, Keyword: "x"})
			shouldBeTreeCode(err, code.CacheTreePathInvalid)
		}
	})

	convey.Convey("搜索词为空或超长时拒绝", t, func() {
		svc, _, _ := setupTree(t)
		for _, keyword := range []string{"", strings.Repeat("k", maxTreeKeywordLen+1)} {
			_, err := svc.TreeSearch(context.Background(), &TreeSearchRequest{Keyword: keyword})
			shouldBeTreeCode(err, code.CacheTreeKeywordInvalid)
		}
	})
}

// TestTreePurge_RejectsBadPath 根路径与非法路径一律拒绝，且不碰任何仓储——
// mock 对没 EXPECT 过的调用会当场失败，这就是「不删任何东西」的断言。
func TestTreePurge_RejectsBadPath(t *testing.T) {
	convey.Convey("根路径不是一个可以按目录清除的目标", t, func() {
		svc, _, _ := setupTree(t)
		_, err := svc.TreePurge(context.Background(), &TreePurgeRequest{Path: ""})
		shouldBeTreeCode(err, code.CacheTreePathInvalid)
	})

	convey.Convey("..、空段与超长路径按参数错误拒绝，不查库", t, func() {
		svc, _, _ := setupTree(t)
		for _, path := range []string{"..", "deb.debian.org/../x", "/deb.debian.org", "deb.debian.org/",
			"deb.debian.org//pool", strings.Repeat("a", maxTreePathLen+1)} {
			_, err := svc.TreePurge(context.Background(), &TreePurgeRequest{Path: path})
			shouldBeTreeCode(err, code.CacheTreePathInvalid)
		}
	})
}

// TestTreePurge_UnknownHostIsNoop 指向不存在的上游（例如已经被并发清空）不算错误，
// 只是清不掉任何东西，与 Tree 对空目录的处理一致。
func TestTreePurge_UnknownHostIsNoop(t *testing.T) {
	convey.Convey("主机不存在时是空操作", t, func() {
		svc, _, upRepo := setupTree(t)
		upRepo.EXPECT().FindByHost(gomock.Any(), "gone.example").Return(nil, nil)

		resp, err := svc.TreePurge(context.Background(), &TreePurgeRequest{Path: "gone.example/x"})
		convey.So(err, convey.ShouldBeNil)
		convey.So(resp.Removed, convey.ShouldEqual, 0)
		convey.So(resp.Skipped, convey.ShouldEqual, 0)
	})
}

// matchRecorder 记下 StatTreeMatch 被问过哪些目录，按前缀给出合计。
type matchRecorder struct {
	mu   sync.Mutex
	seen map[string]*cache_entity.TreeMatchOption
}

func (m *matchRecorder) expect(cacheRepo *mock_cache_repo.MockCacheObjectRepo) {
	m.seen = map[string]*cache_entity.TreeMatchOption{}
	cacheRepo.EXPECT().StatTreeMatch(gomock.Any(), gomock.Any()).AnyTimes().DoAndReturn(
		func(_ context.Context, opt *cache_entity.TreeMatchOption) (*cache_entity.TreeMatchStat, error) {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.seen[strconv.FormatInt(opt.UpstreamID, 10)+opt.Prefix] = opt
			return &cache_entity.TreeMatchStat{Count: 10, Size: 100, LastAccessAt: 7,
				MatchedCount: int64(len(opt.Prefix)), MatchedSize: 1}, nil
		})
}

func TestTreeSearch_UnderPath(t *testing.T) {
	convey.Convey("在目录下搜索：最多 200 个对象、报总数与截断、给出上级目录的汇总", t, func() {
		svc, cacheRepo, upRepo := setupTree(t)
		upRepo.EXPECT().FindByHost(gomock.Any(), "deb.debian.org").Return(treeUpstreams()[1], nil)
		a := &cache_entity.CacheObject{ID: 1, UpstreamID: 8, Key: "/pool/main/r/redis.deb"}
		b := &cache_entity.CacheObject{ID: 2, UpstreamID: 8, Key: "/pool/main/a.deb?x=/y"}
		cacheRepo.EXPECT().SearchTree(gomock.Any(), &cache_entity.TreeSearchOption{
			UpstreamID: 8, Prefix: "/pool/", Keyword: "Main", Limit: 200}).
			Return([]*cache_entity.CacheObject{a, b}, int64(250), nil)
		rec := &matchRecorder{}
		rec.expect(cacheRepo)

		resp, err := svc.TreeSearch(context.Background(), &TreeSearchRequest{
			Path: "deb.debian.org/pool", Keyword: "Main"})
		convey.So(err, convey.ShouldBeNil)
		convey.So(resp.Path, convey.ShouldEqual, "deb.debian.org/pool")
		convey.So(resp.Matched, convey.ShouldEqual, 250)
		convey.So(resp.Truncated, convey.ShouldBeTrue)
		convey.So(len(resp.Objects), convey.ShouldEqual, 2)
		convey.So(*resp.Objects[0], convey.ShouldResemble, TreeSearchObject{
			Name: "redis.deb", Path: "deb.debian.org/pool/main/r/redis.deb", Host: "deb.debian.org", Object: a})
		convey.So(resp.Objects[1].Name, convey.ShouldEqual, "a.deb?x=/y")

		// 只给当前目录以下的上级目录，按路径排序、不重复。
		convey.So(len(resp.Dirs), convey.ShouldEqual, 2)
		convey.So(*resp.Dirs[0], convey.ShouldResemble, TreeSearchDir{
			Path: "deb.debian.org/pool/main", NameMatch: true,
			Count: 10, Size: 100, LastAccessAt: 7, MatchedCount: int64(len("/pool/main/")), MatchedSize: 1})
		convey.So(resp.Dirs[1].Path, convey.ShouldEqual, "deb.debian.org/pool/main/r")
		convey.So(resp.Dirs[1].NameMatch, convey.ShouldBeFalse)
		convey.So(len(rec.seen), convey.ShouldEqual, 2)
		convey.So(*rec.seen["8/pool/main/r/"], convey.ShouldResemble, cache_entity.TreeMatchOption{
			UpstreamID: 8, Prefix: "/pool/main/r/", SearchPrefix: "/pool/", Keyword: "Main"})
	})

	convey.Convey("没到 200 条时不算截断；主机不存在时是空结果", t, func() {
		svc, cacheRepo, upRepo := setupTree(t)
		upRepo.EXPECT().FindByHost(gomock.Any(), "deb.debian.org").Return(treeUpstreams()[1], nil)
		upRepo.EXPECT().FindByHost(gomock.Any(), "gone.example").Return(nil, nil)
		cacheRepo.EXPECT().SearchTree(gomock.Any(), gomock.Any()).
			Return([]*cache_entity.CacheObject{}, int64(0), nil)

		resp, err := svc.TreeSearch(context.Background(), &TreeSearchRequest{Path: "deb.debian.org", Keyword: "zzz"})
		convey.So(err, convey.ShouldBeNil)
		convey.So(resp.Truncated, convey.ShouldBeFalse)
		convey.So(resp.Objects, convey.ShouldNotBeNil)
		convey.So(resp.Dirs, convey.ShouldNotBeNil)

		resp, err = svc.TreeSearch(context.Background(), &TreeSearchRequest{Path: "gone.example", Keyword: "zzz"})
		convey.So(err, convey.ShouldBeNil)
		convey.So(resp.Matched, convey.ShouldEqual, 0)
		convey.So(len(resp.Objects), convey.ShouldEqual, 0)
	})
}

func TestTreeSearch_Root(t *testing.T) {
	convey.Convey("根上搜索是全站搜索：主机名命中的上游整体算命中", t, func() {
		svc, cacheRepo, upRepo := setupTree(t)
		upRepo.EXPECT().List(gomock.Any()).Return(treeUpstreams(), nil)
		deb := &cache_entity.CacheObject{ID: 1, UpstreamID: 8, Key: "/dists/InRelease"}
		manifest := &cache_entity.CacheObject{ID: 2, UpstreamID: 7, Key: "/library/debian/manifests/12\x1faccept=abc"}
		cacheRepo.EXPECT().SearchTree(gomock.Any(), &cache_entity.TreeSearchOption{
			UpstreamIDs: []int64{7, 8}, HostMatched: []int64{8}, Keyword: "DEBIAN", Limit: 200}).
			Return([]*cache_entity.CacheObject{manifest, deb}, int64(2), nil)
		rec := &matchRecorder{}
		rec.expect(cacheRepo)

		resp, err := svc.TreeSearch(context.Background(), &TreeSearchRequest{Keyword: "DEBIAN"})
		convey.So(err, convey.ShouldBeNil)
		convey.So(resp.Matched, convey.ShouldEqual, 2)
		convey.So(*resp.Objects[0], convey.ShouldResemble, TreeSearchObject{
			Name: "12", Path: "registry-1.docker.io/library/debian/manifests/12", Variant: true,
			Host: "registry-1.docker.io", Object: manifest})

		paths := make([]string, 0, len(resp.Dirs))
		nameMatch := map[string]bool{}
		for _, dir := range resp.Dirs {
			paths = append(paths, dir.Path)
			nameMatch[dir.Path] = dir.NameMatch
		}
		convey.So(paths, convey.ShouldResemble, []string{
			"deb.debian.org", "deb.debian.org/dists",
			"registry-1.docker.io", "registry-1.docker.io/library",
			"registry-1.docker.io/library/debian", "registry-1.docker.io/library/debian/manifests",
		})
		convey.So(nameMatch["deb.debian.org"], convey.ShouldBeTrue)
		convey.So(nameMatch["deb.debian.org/dists"], convey.ShouldBeFalse)
		convey.So(nameMatch["registry-1.docker.io"], convey.ShouldBeFalse)
		convey.So(nameMatch["registry-1.docker.io/library/debian"], convey.ShouldBeTrue)

		convey.So(*rec.seen["8/"], convey.ShouldResemble, cache_entity.TreeMatchOption{
			UpstreamID: 8, Prefix: "/", Keyword: "DEBIAN", AllMatched: true})
		convey.So(rec.seen["8/dists/"].AllMatched, convey.ShouldBeTrue)
		convey.So(*rec.seen["7/library/"], convey.ShouldResemble, cache_entity.TreeMatchOption{
			UpstreamID: 7, Prefix: "/library/", Keyword: "DEBIAN"})
	})
}
