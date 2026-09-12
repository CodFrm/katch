// 用例放在 _test 包里：它经 api.Router 注册真实路由，而 api 包又依赖本包，
// 同包会构成导入环。走 _test 包换来的是「测的就是生产路由 + 生产中间件」。
package cache_ctr_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cago-frame/cago/pkg/utils/httputils"
	"github.com/cago-frame/cago/server/mux/muxclient"
	"github.com/cago-frame/cago/server/mux/muxtest"
	"github.com/gin-gonic/gin"
	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"
	"golang.org/x/crypto/bcrypt"

	"github.com/CodFrm/katch/internal/api"
	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/model/entity/cache_entity"
	"github.com/CodFrm/katch/internal/model/entity/setting_entity"
	"github.com/CodFrm/katch/internal/repository/cache_repo"
	mock_cache_repo "github.com/CodFrm/katch/internal/repository/cache_repo/mock"
	"github.com/CodFrm/katch/internal/repository/setting_repo"
	mock_setting_repo "github.com/CodFrm/katch/internal/repository/setting_repo/mock"
	"github.com/CodFrm/katch/internal/service/setting_svc"
)

const adminKey = "correct-admin-key"

// setupCacheTest 装配一套「repo 是 mock、路由与中间件是生产那套」的测试环境。
//
// 缓存业务层用的是它自己的默认实例（没有磁盘 store 的那个）：对象的搜索、清除、
// pin 只碰 cache_object 表，不碰盘；盘上那一半由 cache_svc 自己的用例覆盖。
func setupCacheTest(t *testing.T) (*mock_cache_repo.MockCacheObjectRepo, *muxtest.TestMux, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctrl := gomock.NewController(t)
	cacheRepo := mock_cache_repo.NewMockCacheObjectRepo(ctrl)
	cache_repo.RegisterCacheObject(cacheRepo)

	setRepo := mock_setting_repo.NewMockSettingRepo(ctrl)
	setting_repo.RegisterSetting(setRepo)
	// MinCost：用例只关心比对结果，不关心 bcrypt 的计算代价。
	hash, err := bcrypt.GenerateFromPassword([]byte(adminKey), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	setRepo.EXPECT().Find(gomock.Any(), setting_svc.AdminKeyHashSetting).Return(
		&setting_entity.Setting{Key: setting_svc.AdminKeyHashSetting, Value: string(hash)}, nil,
	).AnyTimes()

	testMux := muxtest.NewTestMux()
	if err := api.Router(context.Background(), testMux.Router); err != nil {
		t.Fatal(err)
	}
	engine, ok := testMux.IRouter.(*gin.Engine)
	if !ok {
		t.Fatal("muxtest 的 IRouter 不再是 *gin.Engine")
	}
	return cacheRepo, testMux, engine
}

func adminHeader(key string) muxclient.ClientDoOption {
	return muxclient.WithHeader(http.Header{"Authorization": []string{"Bearer " + key}})
}

// statusOf 把接口返回的错误摊成 HTTP 状态码。
func statusOf(t *testing.T, err error) int {
	t.Helper()
	e, ok := err.(*httputils.Error)
	if !ok {
		t.Fatalf("期望 *httputils.Error，拿到 %T（%v）", err, err)
	}
	return e.Status
}

// TestCacheSearchAndPurge 覆盖任务目标「按关键字搜到一个缓存对象并清除它」。
func TestCacheSearchAndPurge(t *testing.T) {
	cacheRepo, testMux, _ := setupCacheTest(t)
	convey.Convey("按关键字搜到一个对象再清掉它", t, func() {
		object := &cache_entity.CacheObject{
			ID: 42, UpstreamID: 7, Key: "/pool/redis_7.0.deb",
			Digest: "sha256:abc", Size: 1024, ContentType: "application/x-debian-package",
			Immutable: true, LastAccessAt: 1757000000, HitCount: 3,
			Createtime: 1756000000, Updatetime: 1756000000,
		}
		// 关键字必须原样落到查询条件上：在内存里过滤等于把整张表捞回来，
		// 而这张表是拉取路径自己长出来的，几天就几十万条。
		cacheRepo.EXPECT().Search(gomock.Any(), gomock.Any()).DoAndReturn(
			func(_ context.Context, opt *cache_entity.SearchOption) ([]*cache_entity.CacheObject, int64, error) {
				convey.So(opt.Keyword, convey.ShouldEqual, "redis")
				convey.So(opt.UpstreamID, convey.ShouldEqual, 7)
				convey.So(opt.Limit, convey.ShouldEqual, 10)
				convey.So(opt.Offset, convey.ShouldEqual, 0)
				return []*cache_entity.CacheObject{object}, 1, nil
			})

		searchResp := &admin.SearchCacheObjectsResponse{}
		convey.So(testMux.Do(context.Background(), &admin.SearchCacheObjectsRequest{
			UpstreamID: 7, Keyword: "redis", Page: 1, Size: 10,
		}, searchResp, adminHeader(adminKey)), convey.ShouldBeNil)
		convey.So(searchResp.Total, convey.ShouldEqual, 1)
		convey.So(len(searchResp.List), convey.ShouldEqual, 1)
		convey.So(searchResp.Page, convey.ShouldEqual, 1)
		convey.So(searchResp.Size, convey.ShouldEqual, 10)
		item := searchResp.List[0]
		convey.So(item.ID, convey.ShouldEqual, 42)
		convey.So(item.Key, convey.ShouldEqual, "/pool/redis_7.0.deb")
		convey.So(item.Digest, convey.ShouldEqual, "sha256:abc")
		convey.So(item.Size, convey.ShouldEqual, 1024)
		convey.So(item.Immutable, convey.ShouldBeTrue)
		convey.So(item.Pinned, convey.ShouldBeFalse)
		convey.So(item.HitCount, convey.ShouldEqual, 3)

		convey.Convey("搜到的那一条能按 id 清掉", func() {
			cacheRepo.EXPECT().Find(gomock.Any(), int64(42)).Return(object, nil)
			cacheRepo.EXPECT().Delete(gomock.Any(), int64(42)).Return(nil)

			purgeResp := &admin.PurgeCacheResponse{}
			convey.So(testMux.Do(context.Background(), &admin.PurgeCacheRequest{ID: 42},
				purgeResp, adminHeader(adminKey)), convey.ShouldBeNil)
			convey.So(purgeResp.Removed, convey.ShouldEqual, 1)
		})
	})
}

// TestCachePurgeSkipsPinned 覆盖任务目标「pin 过的对象在清除批次里被跳过」。
//
// 断言落在「pin 的那条的 Delete 没被调用过」上：mock 对没 EXPECT 过的调用会当场
// 失败，所以过滤一旦被拿掉，这个用例红的是「多打了一次 Delete」，而不是只有计数对不上。
func TestCachePurgeSkipsPinned(t *testing.T) {
	cacheRepo, testMux, _ := setupCacheTest(t)
	convey.Convey("按上游清缓存时跳过 pin 过的对象", t, func() {
		cacheRepo.EXPECT().ListByUpstream(gomock.Any(), int64(7)).Return(
			[]*cache_entity.CacheObject{
				{ID: 1, UpstreamID: 7, Key: "/pool/keep.deb", Digest: "sha256:keep", Pinned: true},
				{ID: 2, UpstreamID: 7, Key: "/pool/drop.deb", Digest: "sha256:drop"},
			}, nil)
		// 只有未 pin 的那条会被删；Delete(1) 会让用例当场失败。
		cacheRepo.EXPECT().Delete(gomock.Any(), int64(2)).Return(nil)

		resp := &admin.PurgeCacheResponse{}
		convey.So(testMux.Do(context.Background(), &admin.PurgeCacheRequest{UpstreamID: 7},
			resp, adminHeader(adminKey)), convey.ShouldBeNil)
		convey.So(resp.Removed, convey.ShouldEqual, 1)
		convey.So(resp.Skipped, convey.ShouldEqual, 1)
	})
}

// TestCachePurgeRequiresTarget 两个目标都不给时必须报错：不给「清空一切」留一个
// 不写参数就能触发的形态。
func TestCachePurgeRequiresTarget(t *testing.T) {
	// cacheRepo 上一个 EXPECT 都没有：一旦真去列表或删除，mock 会当场失败。
	_, testMux, _ := setupCacheTest(t)
	convey.Convey("既没给对象也没给上游时拒绝清缓存", t, func() {
		err := testMux.Do(context.Background(), &admin.PurgeCacheRequest{},
			&admin.PurgeCacheResponse{}, adminHeader(adminKey))
		convey.So(err, convey.ShouldNotBeNil)
		convey.So(statusOf(t, err), convey.ShouldEqual, http.StatusBadRequest)
	})
}

// TestCachePin pin 是界面上的一个开关：钉住和放开是同一个端点的两个取值。
func TestCachePin(t *testing.T) {
	cacheRepo, testMux, _ := setupCacheTest(t)
	convey.Convey("钉住与放开一个缓存对象", t, func() {
		convey.Convey("钉住存在的对象", func() {
			cacheRepo.EXPECT().Find(gomock.Any(), int64(42)).Return(
				&cache_entity.CacheObject{ID: 42, UpstreamID: 7}, nil)
			cacheRepo.EXPECT().SetPinned(gomock.Any(), int64(42), true).Return(nil)
			convey.So(testMux.Do(context.Background(), &admin.PinCacheObjectRequest{ID: 42, Pinned: true},
				&admin.PinCacheObjectResponse{}, adminHeader(adminKey)), convey.ShouldBeNil)
		})

		convey.Convey("放开走同一个端点", func() {
			cacheRepo.EXPECT().Find(gomock.Any(), int64(42)).Return(
				&cache_entity.CacheObject{ID: 42, UpstreamID: 7, Pinned: true}, nil)
			cacheRepo.EXPECT().SetPinned(gomock.Any(), int64(42), false).Return(nil)
			convey.So(testMux.Do(context.Background(), &admin.PinCacheObjectRequest{ID: 42, Pinned: false},
				&admin.PinCacheObjectResponse{}, adminHeader(adminKey)), convey.ShouldBeNil)
		})

		convey.Convey("对象不存在时是 404，而不是一次静悄悄的空操作", func() {
			cacheRepo.EXPECT().Find(gomock.Any(), int64(404)).Return(nil, nil)
			err := testMux.Do(context.Background(), &admin.PinCacheObjectRequest{ID: 404, Pinned: true},
				&admin.PinCacheObjectResponse{}, adminHeader(adminKey))
			convey.So(err, convey.ShouldNotBeNil)
			convey.So(statusOf(t, err), convey.ShouldEqual, http.StatusNotFound)
		})
	})
}

// TestCacheAdminAuth 缓存管理是破坏性操作（决策 10），三个端点都必须在密钥后面。
func TestCacheAdminAuth(t *testing.T) {
	// cacheRepo 上一个 EXPECT 都没有：鉴权一旦漏放行进 service，mock 会当场失败。
	_, testMux, engine := setupCacheTest(t)
	convey.Convey("缓存管理端点一律要密钥", t, func() {
		for _, req := range []any{
			&admin.SearchCacheObjectsRequest{Keyword: "redis"},
			&admin.PurgeCacheRequest{UpstreamID: 7},
			&admin.PinCacheObjectRequest{ID: 42, Pinned: true},
		} {
			httpReq, err := testMux.Request(context.Background(), req)
			convey.So(err, convey.ShouldBeNil)
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, httpReq)
			convey.So(w.Code, convey.ShouldEqual, http.StatusUnauthorized)
		}
	})
}
