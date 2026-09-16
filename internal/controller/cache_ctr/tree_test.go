package cache_ctr_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"

	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/model/entity/cache_entity"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
	mock_upstream_repo "github.com/CodFrm/katch/internal/repository/upstream_repo/mock"
)

func registerUpstreams(t *testing.T) *mock_upstream_repo.MockUpstreamRepo {
	t.Helper()
	upRepo := mock_upstream_repo.NewMockUpstreamRepo(gomock.NewController(t))
	prev := upstream_repo.Upstream()
	upstream_repo.RegisterUpstream(upRepo)
	t.Cleanup(func() { upstream_repo.RegisterUpstream(prev) })
	return upRepo
}

// TestCacheTree 目录树一层的对外形态：目录与对象按 kind 区分，对象带上主机名。
func TestCacheTree(t *testing.T) {
	cacheRepo, testMux, engine := setupCacheTest(t)
	upRepo := registerUpstreams(t)
	convey.Convey("列出一层目录与对象", t, func() {
		upRepo.EXPECT().FindByHost(gomock.Any(), "deb.debian.org").
			Return(&upstream_entity.Upstream{ID: 8, Host: "deb.debian.org"}, nil)
		cacheRepo.EXPECT().StatByPrefix(gomock.Any(), int64(8), "/pool/").
			Return(&cache_entity.PrefixTreeStat{Count: 4, Pinned: 1, Size: 400, Dirs: 1, Objects: 1}, nil)
		cacheRepo.EXPECT().ListTreeDirs(gomock.Any(), gomock.Any()).
			Return([]*cache_entity.TreeDir{{Name: "main", Count: 3, Pinned: 1, Size: 300, LastAccessAt: 9}}, nil)
		cacheRepo.EXPECT().ListTreeObjects(gomock.Any(), gomock.Any()).
			Return([]*cache_entity.CacheObject{{ID: 5, UpstreamID: 8, Key: "/pool/a.deb\x1faccept=1",
				Digest: "sha256:aa", Size: 100, Pinned: true}}, nil)

		httpReq, err := testMux.Request(context.Background(), &admin.CacheTreeRequest{Path: "deb.debian.org/pool"})
		convey.So(err, convey.ShouldBeNil)
		httpReq.Header.Set("Authorization", "Bearer "+adminKey)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httpReq)
		convey.So(w.Code, convey.ShouldEqual, http.StatusOK)

		var body struct {
			Data map[string]any `json:"data"`
		}
		convey.So(json.Unmarshal(w.Body.Bytes(), &body), convey.ShouldBeNil)
		data := body.Data
		convey.So(data["path"], convey.ShouldEqual, "deb.debian.org/pool")
		convey.So(data["total_count"], convey.ShouldEqual, 4)
		convey.So(data["total_pinned"], convey.ShouldEqual, 1)
		convey.So(data["total_size"], convey.ShouldEqual, 400)
		convey.So(data["has_more"], convey.ShouldEqual, false)
		convey.So(data["next_offset"], convey.ShouldEqual, 2)
		children := data["children"].([]any)
		convey.So(len(children), convey.ShouldEqual, 2)
		dir := children[0].(map[string]any)
		convey.So(dir["kind"], convey.ShouldEqual, "dir")
		convey.So(dir["name"], convey.ShouldEqual, "main")
		convey.So(dir["path"], convey.ShouldEqual, "deb.debian.org/pool/main")
		convey.So(dir["count"], convey.ShouldEqual, 3)
		convey.So(dir["pinned_count"], convey.ShouldEqual, 1)
		convey.So(dir["size"], convey.ShouldEqual, 300)
		convey.So(dir["last_access_at"], convey.ShouldEqual, 9)
		object := children[1].(map[string]any)
		convey.So(object["kind"], convey.ShouldEqual, "object")
		convey.So(object["name"], convey.ShouldEqual, "a.deb")
		convey.So(object["path"], convey.ShouldEqual, "deb.debian.org/pool/a.deb")
		convey.So(object["variant"], convey.ShouldEqual, true)
		item := object["object"].(map[string]any)
		convey.So(item["id"], convey.ShouldEqual, 5)
		convey.So(item["host"], convey.ShouldEqual, "deb.debian.org")
		convey.So(item["digest"], convey.ShouldEqual, "sha256:aa")
		convey.So(item["pinned"], convey.ShouldEqual, true)
	})
}

// TestCacheTreeSearch 搜索结果的对外形态。
func TestCacheTreeSearch(t *testing.T) {
	cacheRepo, testMux, _ := setupCacheTest(t)
	upRepo := registerUpstreams(t)
	convey.Convey("在目录下搜索", t, func() {
		upRepo.EXPECT().FindByHost(gomock.Any(), "deb.debian.org").
			Return(&upstream_entity.Upstream{ID: 8, Host: "deb.debian.org"}, nil)
		cacheRepo.EXPECT().SearchTree(gomock.Any(), gomock.Any()).
			Return([]*cache_entity.CacheObject{{ID: 5, UpstreamID: 8, Key: "/pool/main/redis.deb"}}, int64(201), nil)
		cacheRepo.EXPECT().StatTreeMatch(gomock.Any(), gomock.Any()).
			Return(&cache_entity.TreeMatchStat{Count: 9, Size: 90, LastAccessAt: 3, MatchedCount: 2, MatchedSize: 20}, nil)

		resp := &admin.CacheTreeSearchResponse{}
		convey.So(testMux.Do(context.Background(), &admin.CacheTreeSearchRequest{
			Path: "deb.debian.org/pool", Keyword: "redis"}, resp, adminHeader(adminKey)), convey.ShouldBeNil)
		convey.So(resp.Path, convey.ShouldEqual, "deb.debian.org/pool")
		convey.So(resp.Matched, convey.ShouldEqual, 201)
		convey.So(resp.Truncated, convey.ShouldBeTrue)
		convey.So(len(resp.Objects), convey.ShouldEqual, 1)
		convey.So(resp.Objects[0].Name, convey.ShouldEqual, "redis.deb")
		convey.So(resp.Objects[0].Path, convey.ShouldEqual, "deb.debian.org/pool/main/redis.deb")
		convey.So(resp.Objects[0].Object.ID, convey.ShouldEqual, 5)
		convey.So(resp.Objects[0].Object.Host, convey.ShouldEqual, "deb.debian.org")
		convey.So(len(resp.Dirs), convey.ShouldEqual, 1)
		convey.So(*resp.Dirs[0], convey.ShouldResemble, admin.CacheTreeSearchDir{
			Path: "deb.debian.org/pool/main", NameMatch: false, Count: 9, Size: 90,
			MatchedCount: 2, MatchedSize: 20, LastAccessAt: 3})
	})
}

// TestCacheTreeRejectsBadInput ..、空段、超长与缺参按参数错误拒绝，且不碰任何仓储。
func TestCacheTreeRejectsBadInput(t *testing.T) {
	// 两个 repo 上都没有 EXPECT：校验一旦漏放行，mock 会当场失败。
	_, testMux, _ := setupCacheTest(t)
	registerUpstreams(t)
	convey.Convey("非法参数一律 400", t, func() {
		for _, path := range []string{"deb.debian.org/../etc", "..", "deb.debian.org//pool",
			"/deb.debian.org", "deb.debian.org/", strings.Repeat("a", 1025)} {
			err := testMux.Do(context.Background(), &admin.CacheTreeRequest{Path: path},
				&admin.CacheTreeResponse{}, adminHeader(adminKey))
			convey.So(err, convey.ShouldNotBeNil)
			convey.So(statusOf(t, err), convey.ShouldEqual, http.StatusBadRequest)

			err = testMux.Do(context.Background(), &admin.CacheTreeSearchRequest{Path: path, Keyword: "x"},
				&admin.CacheTreeSearchResponse{}, adminHeader(adminKey))
			convey.So(err, convey.ShouldNotBeNil)
			convey.So(statusOf(t, err), convey.ShouldEqual, http.StatusBadRequest)
		}
		for _, keyword := range []string{"", strings.Repeat("k", 257)} {
			err := testMux.Do(context.Background(), &admin.CacheTreeSearchRequest{Keyword: keyword},
				&admin.CacheTreeSearchResponse{}, adminHeader(adminKey))
			convey.So(err, convey.ShouldNotBeNil)
			convey.So(statusOf(t, err), convey.ShouldEqual, http.StatusBadRequest)
		}
		err := testMux.Do(context.Background(), &admin.CacheTreeRequest{Offset: -1},
			&admin.CacheTreeResponse{}, adminHeader(adminKey))
		convey.So(err, convey.ShouldNotBeNil)
		convey.So(statusOf(t, err), convey.ShouldEqual, http.StatusBadRequest)
	})
}

// TestCacheTreePurge 按目录清除的对外形态：未 pin 对象被清、pin 的跳过并报数。
func TestCacheTreePurge(t *testing.T) {
	cacheRepo, testMux, _ := setupCacheTest(t)
	upRepo := registerUpstreams(t)
	convey.Convey("按目录清除，跳过 pin 的对象并报数", t, func() {
		upRepo.EXPECT().FindByHost(gomock.Any(), "deb.debian.org").
			Return(&upstream_entity.Upstream{ID: 8, Host: "deb.debian.org"}, nil)
		cacheRepo.EXPECT().ListByPrefix(gomock.Any(), int64(8), "/pool/").Return(
			[]*cache_entity.CacheObject{
				{ID: 1, UpstreamID: 8, Key: "/pool/keep.deb", Digest: "sha256:keep", Pinned: true},
				{ID: 2, UpstreamID: 8, Key: "/pool/drop.deb", Digest: "sha256:drop"},
			}, nil)
		// 只有未 pin 的那条会被删；Delete(1) 会让用例当场失败。
		cacheRepo.EXPECT().Delete(gomock.Any(), int64(2)).Return(nil)

		resp := &admin.CacheTreePurgeResponse{}
		convey.So(testMux.Do(context.Background(), &admin.CacheTreePurgeRequest{Path: "deb.debian.org/pool"},
			resp, adminHeader(adminKey)), convey.ShouldBeNil)
		convey.So(resp.Removed, convey.ShouldEqual, 1)
		convey.So(resp.Skipped, convey.ShouldEqual, 1)
	})
}

// TestCacheTreePurgeRejectsBadInput 根路径与非法路径一律 400，且不碰任何仓储。
func TestCacheTreePurgeRejectsBadInput(t *testing.T) {
	// cacheRepo 上没有 EXPECT：校验一旦漏放行，mock 会当场失败。
	_, testMux, _ := setupCacheTest(t)
	registerUpstreams(t)
	convey.Convey("非法参数一律 400，不删任何东西", t, func() {
		for _, path := range []string{"", "deb.debian.org/../etc", "..", "deb.debian.org//pool",
			"/deb.debian.org", "deb.debian.org/", strings.Repeat("a", 1025)} {
			err := testMux.Do(context.Background(), &admin.CacheTreePurgeRequest{Path: path},
				&admin.CacheTreePurgeResponse{}, adminHeader(adminKey))
			convey.So(err, convey.ShouldNotBeNil)
			convey.So(statusOf(t, err), convey.ShouldEqual, http.StatusBadRequest)
		}
	})
}

// TestCacheTreeAuth 目录树与搜索和其余管理接口同一道密钥闸。
func TestCacheTreeAuth(t *testing.T) {
	_, testMux, engine := setupCacheTest(t)
	registerUpstreams(t)
	convey.Convey("没有密钥时 401", t, func() {
		for _, req := range []any{
			&admin.CacheTreeRequest{Path: "deb.debian.org"},
			&admin.CacheTreeSearchRequest{Keyword: "redis"},
			&admin.CacheTreePurgeRequest{Path: "deb.debian.org"},
		} {
			httpReq, err := testMux.Request(context.Background(), req)
			convey.So(err, convey.ShouldBeNil)
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, httpReq)
			convey.So(w.Code, convey.ShouldEqual, http.StatusUnauthorized)
		}
	})
}
