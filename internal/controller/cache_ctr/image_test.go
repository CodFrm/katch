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
)

func dockerHub() *upstream_entity.Upstream {
	return &upstream_entity.Upstream{ID: 7, Host: "docker.io", LibraryCompletion: true,
		Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolRegistry}}
}

func serveJSON(t *testing.T, engine http.Handler, httpReq *http.Request) map[string]any {
	t.Helper()
	httpReq.Header.Set("Authorization", "Bearer "+adminKey)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httpReq)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d：%s", w.Code, w.Body.String())
	}
	var body struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Data
}

// TestCacheImages 镜像列表的对外形态。
func TestCacheImages(t *testing.T) {
	cacheRepo, testMux, engine := setupCacheTest(t)
	upRepo := registerUpstreams(t)
	convey.Convey("按仓库列出镜像，关键字命中 tag 时带上命中的 tag", t, func() {
		upRepo.EXPECT().List(gomock.Any()).Return([]*upstream_entity.Upstream{dockerHub()}, nil)
		cacheRepo.EXPECT().ScanByUpstream(gomock.Any(), int64(7), int64(0), gomock.Any()).Return(
			[]*cache_entity.CacheObject{
				{ID: 1, UpstreamID: 7, Key: "/redis/manifests/7-alpine", Digest: "sha256:m", Size: 10,
					HitCount: 2, LastAccessAt: 100, ExpiresAt: 1, Pinned: true},
				{ID: 2, UpstreamID: 7, Key: "/library/redis/blobs/sha256:l", Digest: "sha256:l", Size: 90,
					HitCount: 1, LastAccessAt: 50, Immutable: true},
			}, nil)

		httpReq, err := testMux.Request(context.Background(), &admin.ListCacheImagesRequest{Keyword: "alpine"})
		convey.So(err, convey.ShouldBeNil)
		data := serveJSON(t, engine, httpReq)
		convey.So(data["total"], convey.ShouldEqual, 1)
		convey.So(data["has_more"], convey.ShouldEqual, false)
		convey.So(data["next_offset"], convey.ShouldEqual, 1)
		list := data["list"].([]any)
		convey.So(len(list), convey.ShouldEqual, 1)
		image := list[0].(map[string]any)
		convey.So(image, convey.ShouldResemble, map[string]any{
			"upstream_id": float64(7), "host": "docker.io", "repository": "library/redis",
			"tag_count": float64(1), "object_count": float64(2), "pinned_count": float64(1),
			"size": float64(100), "hit_count": float64(3), "last_access_at": float64(100),
			"tags": []any{map[string]any{
				"reference": "7-alpine", "by_digest": false, "digest": "sha256:m", "variants": float64(1),
				"object_count": float64(1), "pinned": true, "expired": true,
				"hit_count": float64(2), "last_access_at": float64(100),
			}},
		})
	})
}

// TestCacheImageTags tag 列表的对外形态。
func TestCacheImageTags(t *testing.T) {
	cacheRepo, testMux, engine := setupCacheTest(t)
	upRepo := registerUpstreams(t)
	convey.Convey("列出一个镜像的 tag", t, func() {
		upRepo.EXPECT().Find(gomock.Any(), int64(7)).Return(dockerHub(), nil)
		cacheRepo.EXPECT().ListByPrefix(gomock.Any(), int64(7), "/library/redis/manifests/").
			Return([]*cache_entity.CacheObject{}, nil)
		cacheRepo.EXPECT().ListByPrefix(gomock.Any(), int64(7), "/redis/manifests/").
			Return([]*cache_entity.CacheObject{{ID: 3, UpstreamID: 7, Key: "/redis/manifests/sha256:abc",
				Digest: "sha256:abc", Immutable: true, LastAccessAt: 9}}, nil)

		httpReq, err := testMux.Request(context.Background(), &admin.ListCacheImageTagsRequest{
			UpstreamID: 7, Repository: "library/redis"})
		convey.So(err, convey.ShouldBeNil)
		data := serveJSON(t, engine, httpReq)
		convey.So(data["list"], convey.ShouldResemble, []any{map[string]any{
			"reference": "sha256:abc", "by_digest": true, "digest": "sha256:abc", "variants": float64(1),
			"object_count": float64(1), "pinned": false, "expired": false,
			"hit_count": float64(0), "last_access_at": float64(9),
		}})
	})
}

// TestCacheImagePurge 删除 tag：只删该引用的记录，pin 的跳过并报数。
func TestCacheImagePurge(t *testing.T) {
	cacheRepo, testMux, _ := setupCacheTest(t)
	upRepo := registerUpstreams(t)
	convey.Convey("删除 tag", t, func() {
		upRepo.EXPECT().Find(gomock.Any(), int64(7)).Return(dockerHub(), nil)
		cacheRepo.EXPECT().ListByPrefix(gomock.Any(), int64(7), "/library/redis/manifests/").Return(
			[]*cache_entity.CacheObject{
				{ID: 1, UpstreamID: 7, Key: "/library/redis/manifests/7", Pinned: true},
				{ID: 2, UpstreamID: 7, Key: "/library/redis/manifests/8"},
			}, nil)
		cacheRepo.EXPECT().ListByPrefix(gomock.Any(), int64(7), "/redis/manifests/").Return(
			[]*cache_entity.CacheObject{{ID: 3, UpstreamID: 7, Key: "/redis/manifests/7\x1faccept=a"}}, nil)
		// 只有 3 会被删：1 被 pin，2 是另一个 tag。
		cacheRepo.EXPECT().Delete(gomock.Any(), int64(3)).Return(nil)

		resp := &admin.PurgeCacheImageResponse{}
		convey.So(testMux.Do(context.Background(), &admin.PurgeCacheImageRequest{
			UpstreamID: 7, Repository: "redis", Reference: "7"}, resp, adminHeader(adminKey)), convey.ShouldBeNil)
		convey.So(*resp, convey.ShouldResemble, admin.PurgeCacheImageResponse{Removed: 1, Skipped: 1})
	})
}

// TestCacheImagesRejectBadInput ..、空段、超长与缺参按参数错误拒绝，且不碰任何仓储。
func TestCacheImagesRejectBadInput(t *testing.T) {
	_, testMux, _ := setupCacheTest(t)
	registerUpstreams(t)
	convey.Convey("非法参数一律 400，不删任何东西", t, func() {
		for _, repository := range []string{"", "..", "a/../b", "a//b", "/a", "a/", strings.Repeat("r", 501)} {
			err := testMux.Do(context.Background(), &admin.ListCacheImageTagsRequest{UpstreamID: 7, Repository: repository},
				&admin.ListCacheImageTagsResponse{}, adminHeader(adminKey))
			convey.So(err, convey.ShouldNotBeNil)
			convey.So(statusOf(t, err), convey.ShouldEqual, http.StatusBadRequest)
			err = testMux.Do(context.Background(), &admin.PurgeCacheImageRequest{UpstreamID: 7, Repository: repository},
				&admin.PurgeCacheImageResponse{}, adminHeader(adminKey))
			convey.So(err, convey.ShouldNotBeNil)
			convey.So(statusOf(t, err), convey.ShouldEqual, http.StatusBadRequest)
		}
		for _, reference := range []string{"..", "a/b", strings.Repeat("t", 257)} {
			err := testMux.Do(context.Background(), &admin.PurgeCacheImageRequest{UpstreamID: 7, Repository: "redis",
				Reference: reference}, &admin.PurgeCacheImageResponse{}, adminHeader(adminKey))
			convey.So(err, convey.ShouldNotBeNil)
			convey.So(statusOf(t, err), convey.ShouldEqual, http.StatusBadRequest)
		}
		// 缺上游。
		err := testMux.Do(context.Background(), &admin.ListCacheImageTagsRequest{Repository: "redis"},
			&admin.ListCacheImageTagsResponse{}, adminHeader(adminKey))
		convey.So(err, convey.ShouldNotBeNil)
		convey.So(statusOf(t, err), convey.ShouldEqual, http.StatusBadRequest)
		err = testMux.Do(context.Background(), &admin.PurgeCacheImageRequest{Repository: "redis"},
			&admin.PurgeCacheImageResponse{}, adminHeader(adminKey))
		convey.So(err, convey.ShouldNotBeNil)
		convey.So(statusOf(t, err), convey.ShouldEqual, http.StatusBadRequest)
		for _, req := range []*admin.ListCacheImagesRequest{
			{Keyword: strings.Repeat("k", 257)}, {Offset: -1}, {Size: 201},
		} {
			err := testMux.Do(context.Background(), req, &admin.ListCacheImagesResponse{}, adminHeader(adminKey))
			convey.So(err, convey.ShouldNotBeNil)
			convey.So(statusOf(t, err), convey.ShouldEqual, http.StatusBadRequest)
		}
	})
}

// TestCacheImagesAuth 镜像接口和其余管理接口同一道密钥闸。
func TestCacheImagesAuth(t *testing.T) {
	_, testMux, engine := setupCacheTest(t)
	registerUpstreams(t)
	convey.Convey("没有密钥时 401", t, func() {
		for _, req := range []any{
			&admin.ListCacheImagesRequest{},
			&admin.ListCacheImageTagsRequest{UpstreamID: 7, Repository: "redis"},
			&admin.PurgeCacheImageRequest{UpstreamID: 7, Repository: "redis"},
		} {
			httpReq, err := testMux.Request(context.Background(), req)
			convey.So(err, convey.ShouldBeNil)
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, httpReq)
			convey.So(w.Code, convey.ShouldEqual, http.StatusUnauthorized)
		}
	})
}
