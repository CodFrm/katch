package cache_svc

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"

	"github.com/CodFrm/katch/internal/model/entity/cache_entity"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/pkg/code"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
	mock_upstream_repo "github.com/CodFrm/katch/internal/repository/upstream_repo/mock"
)

func newRegistryUpstream(id int64, host string, completion bool) *upstream_entity.Upstream {
	return &upstream_entity.Upstream{ID: id, Host: host, LibraryCompletion: completion,
		Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolRegistry}}
}

func imageUpstreams() []*upstream_entity.Upstream {
	return []*upstream_entity.Upstream{
		newRegistryUpstream(7, "docker.io", true),
		newRegistryUpstream(8, "ghcr.io", false),
		// 不含 registry 协议的上游不参与镜像视图，也不该被扫。
		{ID: 9, Host: "deb.debian.org", Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic}},
	}
}

func object(id int64, key string, size, hits, last int64) *cache_entity.CacheObject {
	return &cache_entity.CacheObject{ID: id, UpstreamID: 7, Key: key, Size: size, HitCount: hits,
		LastAccessAt: last, Digest: "sha256:" + strconv.FormatInt(id, 10)}
}

func TestImages_MergesRepositories(t *testing.T) {
	convey.Convey("按最后一个动词段切仓库，开了补全的上游把 x 归并到 library/x", t, func() {
		svc, cacheRepo, upRepo := setupTree(t)
		upRepo.EXPECT().List(gomock.Any()).Return(imageUpstreams(), nil)
		pinnedManifest := object(2, "/library/redis/manifests/7\x1faccept=b", 11, 3, 200)
		pinnedManifest.Pinned = true
		cacheRepo.EXPECT().ScanByUpstream(gomock.Any(), int64(7), int64(0), imageScanBatch).Return(
			[]*cache_entity.CacheObject{
				object(1, "/redis/manifests/7\x1faccept=a", 10, 2, 100),
				pinnedManifest,
				object(3, "/library/redis/blobs/sha256:l1", 1000, 1, 50),
				object(4, "/redis/manifests/sha256:m2", 11, 0, 150),
				object(5, "/redis/tags/list?n=10&last=a/manifests/b", 5, 1, 20),
				// 仓库名本身叫 blobs：按最后一个动词段断句。
				object(6, "/blobs/manifests/latest", 7, 1, 10),
				// 只有层的仓库也是一个镜像，Tag 数为 0。
				object(10, "/only/layers/blobs/sha256:x", 100, 4, 300),
				// 拿不出仓库名的键不算任何镜像。
				object(11, "/_catalog", 1, 1, 999),
			}, nil)
		cacheRepo.EXPECT().ScanByUpstream(gomock.Any(), int64(8), int64(0), imageScanBatch).Return(
			[]*cache_entity.CacheObject{
				{ID: 20, UpstreamID: 8, Key: "/redis/manifests/1", Size: 1, LastAccessAt: 60},
				{ID: 21, UpstreamID: 8, Key: "/library/redis/manifests/1", Size: 2, LastAccessAt: 40},
			}, nil)

		resp, err := svc.Images(context.Background(), &ImagesRequest{})
		convey.So(err, convey.ShouldBeNil)
		convey.So(resp.Total, convey.ShouldEqual, 5)
		convey.So(resp.HasMore, convey.ShouldBeFalse)
		convey.So(resp.NextOffset, convey.ShouldEqual, 5)
		names := make([]string, 0, len(resp.List))
		for _, image := range resp.List {
			names = append(names, image.Host+"/"+image.Repository)
		}
		// 按最后访问倒序。
		convey.So(names, convey.ShouldResemble, []string{
			"docker.io/only/layers", "docker.io/library/redis", "ghcr.io/redis",
			"ghcr.io/library/redis", "docker.io/library/blobs"})
		convey.So(*resp.List[1], convey.ShouldResemble, Image{
			UpstreamID: 7, Host: "docker.io", Repository: "library/redis",
			TagCount: 2, ObjectCount: 5, PinnedCount: 1, Size: 1037, HitCount: 7, LastAccessAt: 200,
			Tags: []*ImageTag{}})
		convey.So(resp.List[0].TagCount, convey.ShouldEqual, 0)
		convey.So(resp.List[0].Size, convey.ShouldEqual, 100)
		// 未开补全的上游不归并。
		convey.So(resp.List[2].ObjectCount, convey.ShouldEqual, 1)
		convey.So(resp.List[3].ObjectCount, convey.ShouldEqual, 1)
	})

	convey.Convey("记录按批扫，一批满了才接着扫下一批", t, func() {
		svc, cacheRepo, upRepo := setupTree(t)
		upRepo.EXPECT().Find(gomock.Any(), int64(7)).Return(newRegistryUpstream(7, "docker.io", true), nil)
		full := make([]*cache_entity.CacheObject, 0, imageScanBatch)
		for i := 1; i <= imageScanBatch; i++ {
			full = append(full, object(int64(i), "/big/blobs/sha256:"+strconv.Itoa(i), 1, 0, 1))
		}
		gomock.InOrder(
			cacheRepo.EXPECT().ScanByUpstream(gomock.Any(), int64(7), int64(0), imageScanBatch).Return(full, nil),
			cacheRepo.EXPECT().ScanByUpstream(gomock.Any(), int64(7), int64(imageScanBatch), imageScanBatch).
				Return([]*cache_entity.CacheObject{object(5000, "/big/manifests/1", 1, 0, 2)}, nil),
		)
		resp, err := svc.Images(context.Background(), &ImagesRequest{UpstreamID: 7})
		convey.So(err, convey.ShouldBeNil)
		convey.So(len(resp.List), convey.ShouldEqual, 1)
		convey.So(resp.List[0].ObjectCount, convey.ShouldEqual, imageScanBatch+1)
		convey.So(resp.List[0].TagCount, convey.ShouldEqual, 1)
	})
}

func TestImages_UpstreamFilter(t *testing.T) {
	convey.Convey("指定上游时只扫它；不是 registry 或不存在的上游给空列表", t, func() {
		svc, cacheRepo, upRepo := setupTree(t)
		upRepo.EXPECT().Find(gomock.Any(), int64(9)).Return(imageUpstreams()[2], nil)
		upRepo.EXPECT().Find(gomock.Any(), int64(99)).Return(nil, nil)
		for _, id := range []int64{9, 99} {
			resp, err := svc.Images(context.Background(), &ImagesRequest{UpstreamID: id})
			convey.So(err, convey.ShouldBeNil)
			convey.So(resp.Total, convey.ShouldEqual, 0)
			convey.So(resp.List, convey.ShouldNotBeNil)
			convey.So(len(resp.List), convey.ShouldEqual, 0)
		}
		_ = cacheRepo
	})
}

func imageRows() []*cache_entity.CacheObject {
	rows := []*cache_entity.CacheObject{}
	for i, repo := range []string{"alpha", "beta", "gamma"} {
		id := int64(i*10 + 1)
		rows = append(rows,
			object(id, "/"+repo+"/manifests/v1", 1, 1, 100+id),
			object(id+1, "/"+repo+"/manifests/Stable-"+repo, 1, 1, 100+id),
			object(id+2, "/"+repo+"/blobs/sha256:x", 1, 1, 100+id))
	}
	return rows
}

func TestImages_KeywordAndPaging(t *testing.T) {
	convey.Convey("关键字匹配仓库名或 tag，不区分大小写", t, func() {
		svc, cacheRepo, upRepo := setupTree(t)
		upRepo.EXPECT().Find(gomock.Any(), int64(8)).Return(newRegistryUpstream(8, "ghcr.io", false), nil).AnyTimes()
		cacheRepo.EXPECT().ScanByUpstream(gomock.Any(), int64(8), int64(0), imageScanBatch).
			Return(imageRows(), nil).AnyTimes()

		convey.Convey("命中仓库名时不带 tag（界面展开时取全部）", func() {
			resp, err := svc.Images(context.Background(), &ImagesRequest{UpstreamID: 8, Keyword: "BET"})
			convey.So(err, convey.ShouldBeNil)
			convey.So(resp.Total, convey.ShouldEqual, 1)
			convey.So(resp.List[0].Repository, convey.ShouldEqual, "beta")
			convey.So(resp.List[0].Tags, convey.ShouldResemble, []*ImageTag{})
			convey.So(resp.List[0].TagCount, convey.ShouldEqual, 2)
		})
		convey.Convey("只命中 tag 时只带命中的 tag", func() {
			resp, err := svc.Images(context.Background(), &ImagesRequest{UpstreamID: 8, Keyword: "stable-g"})
			convey.So(err, convey.ShouldBeNil)
			convey.So(resp.Total, convey.ShouldEqual, 1)
			convey.So(resp.List[0].Repository, convey.ShouldEqual, "gamma")
			convey.So(len(resp.List[0].Tags), convey.ShouldEqual, 1)
			convey.So(resp.List[0].Tags[0].Reference, convey.ShouldEqual, "Stable-gamma")
			convey.So(resp.List[0].TagCount, convey.ShouldEqual, 2)
		})
		convey.Convey("分页", func() {
			resp, err := svc.Images(context.Background(), &ImagesRequest{UpstreamID: 8, Offset: 1, Size: 1})
			convey.So(err, convey.ShouldBeNil)
			convey.So(resp.Total, convey.ShouldEqual, 3)
			convey.So(len(resp.List), convey.ShouldEqual, 1)
			convey.So(resp.List[0].Repository, convey.ShouldEqual, "beta")
			convey.So(resp.HasMore, convey.ShouldBeTrue)
			convey.So(resp.NextOffset, convey.ShouldEqual, 2)

			resp, err = svc.Images(context.Background(), &ImagesRequest{UpstreamID: 8, Offset: 2})
			convey.So(err, convey.ShouldBeNil)
			convey.So(len(resp.List), convey.ShouldEqual, 1)
			convey.So(resp.HasMore, convey.ShouldBeFalse)
			convey.So(resp.NextOffset, convey.ShouldEqual, 3)
		})
	})
}

func TestImages_DefaultPageSize(t *testing.T) {
	convey.Convey("默认每页 50 个镜像", t, func() {
		svc, cacheRepo, upRepo := setupTree(t)
		upRepo.EXPECT().Find(gomock.Any(), int64(8)).Return(newRegistryUpstream(8, "ghcr.io", false), nil)
		rows := make([]*cache_entity.CacheObject, 0, 60)
		for i := int64(1); i <= 60; i++ {
			rows = append(rows, object(i, "/repo"+strconv.FormatInt(i, 10)+"/blobs/sha256:x", 1, 0, i))
		}
		cacheRepo.EXPECT().ScanByUpstream(gomock.Any(), int64(8), int64(0), imageScanBatch).Return(rows, nil)
		resp, err := svc.Images(context.Background(), &ImagesRequest{UpstreamID: 8})
		convey.So(err, convey.ShouldBeNil)
		convey.So(len(resp.List), convey.ShouldEqual, 50)
		convey.So(resp.HasMore, convey.ShouldBeTrue)
		convey.So(resp.NextOffset, convey.ShouldEqual, 50)
	})
}

func TestImageTags(t *testing.T) {
	convey.Convey("tag 行：变体合并、按摘要拉取成行、pin 与过期标记", t, func() {
		svc, cacheRepo, upRepo := setupTree(t)
		upRepo.EXPECT().Find(gomock.Any(), int64(7)).Return(newRegistryUpstream(7, "docker.io", true), nil).AnyTimes()
		now := time.Now().Unix()
		older := object(1, "/redis/manifests/7\x1faccept=a", 10, 2, 100)
		older.ExpiresAt = now + 3600
		newer := object(2, "/library/redis/manifests/7\x1faccept=b", 11, 3, 200)
		newer.ExpiresAt = now - 10
		plain := object(3, "/library/redis/manifests/7", 12, 1, 150)
		plain.ExpiresAt = now + 3600
		plain.Pinned = true
		byDigest := object(4, "/redis/manifests/sha256:abc", 11, 5, 300)
		byDigest.Immutable = true
		fresh := object(5, "/library/redis/manifests/8", 11, 0, 50)
		fresh.ExpiresAt = now + 3600
		// 前缀之下但属于另一个仓库（library/redis/manifests/sub）的键不算。
		other := object(6, "/library/redis/manifests/sub/manifests/1", 1, 1, 999)
		cacheRepo.EXPECT().ListByPrefix(gomock.Any(), int64(7), "/library/redis/manifests/").
			Return([]*cache_entity.CacheObject{newer, plain, fresh, other}, nil).AnyTimes()
		cacheRepo.EXPECT().ListByPrefix(gomock.Any(), int64(7), "/redis/manifests/").
			Return([]*cache_entity.CacheObject{older, byDigest}, nil).AnyTimes()

		for _, repository := range []string{"redis", "library/redis"} {
			resp, err := svc.ImageTags(context.Background(), &ImageTagsRequest{UpstreamID: 7, Repository: repository})
			convey.So(err, convey.ShouldBeNil)
			convey.So(len(resp.List), convey.ShouldEqual, 3)
			convey.So(*resp.List[0], convey.ShouldResemble, ImageTag{
				Reference: "sha256:abc", ByDigest: true, Digest: "sha256:4", Variants: 1,
				ObjectCount: 1, HitCount: 5, LastAccessAt: 300})
			// 三条记录两种写法、三个变体段（含无变体）合成一行：摘要取最近访问的那份，
			// 过期看的也是它；任意一份被 pin 就算 pin。
			convey.So(*resp.List[1], convey.ShouldResemble, ImageTag{
				Reference: "7", Digest: "sha256:2", Variants: 3, ObjectCount: 3, PinnedCount: 1,
				Pinned: true, Expired: true, HitCount: 6, LastAccessAt: 200})
			convey.So(*resp.List[2], convey.ShouldResemble, ImageTag{
				Reference: "8", Digest: "sha256:5", Variants: 1, ObjectCount: 1, LastAccessAt: 50})
		}

		resp, err := svc.ImageTags(context.Background(), &ImageTagsRequest{UpstreamID: 7, Repository: "redis", Keyword: "SHA256"})
		convey.So(err, convey.ShouldBeNil)
		convey.So(len(resp.List), convey.ShouldEqual, 1)
		convey.So(resp.List[0].Reference, convey.ShouldEqual, "sha256:abc")
	})

	convey.Convey("未开补全的上游只查原样的仓库名；非 registry 上游给空列表", t, func() {
		svc, cacheRepo, upRepo := setupTree(t)
		upRepo.EXPECT().Find(gomock.Any(), int64(8)).Return(newRegistryUpstream(8, "ghcr.io", false), nil)
		upRepo.EXPECT().Find(gomock.Any(), int64(9)).Return(imageUpstreams()[2], nil)
		cacheRepo.EXPECT().ListByPrefix(gomock.Any(), int64(8), "/redis/manifests/").
			Return([]*cache_entity.CacheObject{object(1, "/redis/manifests/1", 1, 0, 1)}, nil)
		resp, err := svc.ImageTags(context.Background(), &ImageTagsRequest{UpstreamID: 8, Repository: "redis"})
		convey.So(err, convey.ShouldBeNil)
		convey.So(len(resp.List), convey.ShouldEqual, 1)

		resp, err = svc.ImageTags(context.Background(), &ImageTagsRequest{UpstreamID: 9, Repository: "redis"})
		convey.So(err, convey.ShouldBeNil)
		convey.So(resp.List, convey.ShouldNotBeNil)
		convey.So(len(resp.List), convey.ShouldEqual, 0)
	})
}

func TestImage_RejectsBadInput(t *testing.T) {
	convey.Convey("仓库名与引用不合法时按参数错误拒绝，不查库", t, func() {
		svc, _, _ := setupTree(t)
		ctx := context.Background()
		for _, repository := range []string{"", "..", "a/../b", "a//b", "/a", "a/", strings.Repeat("r", 501)} {
			_, err := svc.ImageTags(ctx, &ImageTagsRequest{UpstreamID: 7, Repository: repository})
			shouldBeTreeCode(err, code.CacheImageRepositoryInvalid)
			_, err = svc.ImagePurge(ctx, &ImagePurgeRequest{UpstreamID: 7, Repository: repository})
			shouldBeTreeCode(err, code.CacheImageRepositoryInvalid)
		}
		for _, reference := range []string{"..", "a/b", strings.Repeat("t", 257)} {
			_, err := svc.ImagePurge(ctx, &ImagePurgeRequest{UpstreamID: 7, Repository: "redis", Reference: reference})
			shouldBeTreeCode(err, code.CacheImageReferenceInvalid)
		}
		long := strings.Repeat("k", 257)
		_, err := svc.Images(ctx, &ImagesRequest{Keyword: long})
		shouldBeTreeCode(err, code.CacheTreeKeywordInvalid)
		_, err = svc.ImageTags(ctx, &ImageTagsRequest{UpstreamID: 7, Repository: "redis", Keyword: long})
		shouldBeTreeCode(err, code.CacheTreeKeywordInvalid)
	})
}

// useUpstream 把上游表换成只认这一条的 mock（setupSvc 装的那份不回答 Find）。
func useUpstream(t *testing.T, up *upstream_entity.Upstream) {
	t.Helper()
	upRepo := mock_upstream_repo.NewMockUpstreamRepo(gomock.NewController(t))
	upRepo.EXPECT().Find(gomock.Any(), gomock.Any()).AnyTimes().
		DoAndReturn(func(_ any, id int64) (*upstream_entity.Upstream, error) {
			if id == up.ID {
				return up, nil
			}
			return nil, nil
		})
	upRepo.EXPECT().List(gomock.Any()).Return([]*upstream_entity.Upstream{up}, nil).AnyTimes()
	prev := upstream_repo.Upstream()
	upstream_repo.RegisterUpstream(upRepo)
	t.Cleanup(func() { upstream_repo.RegisterUpstream(prev) })
}

func TestImagePurge(t *testing.T) {
	convey.Convey("删除 tag 与删除镜像：跳过 pin 并报数，删 tag 不连带层", t, func() {
		o := newOrigin(t, func(http.ResponseWriter, *http.Request) {})
		up := newRegistryUpstream(7, "docker.io", true)
		svc, repo, store := setupSvc(t, o, up, Options{})
		useUpstream(t, up)
		ctx := context.Background()

		putObject(t, svc, "/redis/manifests/7", "manifest-a", false)
		putObject(t, svc, "/library/redis/manifests/7\x1faccept=b", "manifest-b", false)
		putObject(t, svc, "/library/redis/manifests/8", "manifest-c", false)
		putObject(t, svc, "/library/redis/blobs/sha256:l", "layer", true)
		putObject(t, svc, "/redis/blobs/sha256:p", "pinned layer", true)
		// 同一份层内容被另一个镜像引用：删掉 redis 之后盘上的文件还得在。
		putObject(t, svc, "/nginx/blobs/sha256:l", "layer", true)
		// 另一个仓库，只是名字以 redis 开头或挂在 redis 之下。
		putObject(t, svc, "/library/redis/extra/blobs/sha256:q", "sub", true)
		putObject(t, svc, "/redisx/manifests/1", "redisx", false)
		convey.So(svc.Pin(ctx, &PinRequest{ID: repo.byKey("/redis/blobs/sha256:p").ID, Pinned: true}), convey.ShouldBeNil)

		resp, err := svc.ImagePurge(ctx, &ImagePurgeRequest{UpstreamID: 7, Repository: "library/redis", Reference: "7"})
		convey.So(err, convey.ShouldBeNil)
		convey.So(*resp, convey.ShouldResemble, ImagePurgeResponse{Removed: 2, Skipped: 0})
		convey.So(repo.byKey("/redis/manifests/7"), convey.ShouldBeNil)
		convey.So(repo.byKey("/library/redis/manifests/7\x1faccept=b"), convey.ShouldBeNil)
		convey.So(repo.byKey("/library/redis/manifests/8"), convey.ShouldNotBeNil)
		convey.So(repo.byKey("/library/redis/blobs/sha256:l"), convey.ShouldNotBeNil)
		_, ok := store.Has(digestOfString("manifest-a"))
		convey.So(ok, convey.ShouldBeFalse)

		resp, err = svc.ImagePurge(ctx, &ImagePurgeRequest{UpstreamID: 7, Repository: "redis"})
		convey.So(err, convey.ShouldBeNil)
		convey.So(*resp, convey.ShouldResemble, ImagePurgeResponse{Removed: 2, Skipped: 1})
		convey.So(repo.byKey("/library/redis/manifests/8"), convey.ShouldBeNil)
		convey.So(repo.byKey("/library/redis/blobs/sha256:l"), convey.ShouldBeNil)
		convey.So(repo.byKey("/redis/blobs/sha256:p"), convey.ShouldNotBeNil)
		convey.So(repo.byKey("/nginx/blobs/sha256:l"), convey.ShouldNotBeNil)
		convey.So(repo.byKey("/library/redis/extra/blobs/sha256:q"), convey.ShouldNotBeNil)
		convey.So(repo.byKey("/redisx/manifests/1"), convey.ShouldNotBeNil)
		_, ok = store.Has(digestOfString("layer"))
		convey.So(ok, convey.ShouldBeTrue)

		convey.Convey("pin 过的 tag 变体被跳过并报数", func() {
			putObject(t, svc, "/library/redis/manifests/9", "manifest-d", false)
			convey.So(svc.Pin(ctx, &PinRequest{ID: repo.byKey("/library/redis/manifests/9").ID, Pinned: true}), convey.ShouldBeNil)
			resp, err := svc.ImagePurge(ctx, &ImagePurgeRequest{UpstreamID: 7, Repository: "redis", Reference: "9"})
			convey.So(err, convey.ShouldBeNil)
			convey.So(*resp, convey.ShouldResemble, ImagePurgeResponse{Removed: 0, Skipped: 1})
		})

		convey.Convey("不存在的上游是空操作", func() {
			resp, err := svc.ImagePurge(ctx, &ImagePurgeRequest{UpstreamID: 99, Repository: "redisx"})
			convey.So(err, convey.ShouldBeNil)
			convey.So(*resp, convey.ShouldResemble, ImagePurgeResponse{})
			convey.So(repo.byKey("/redisx/manifests/1"), convey.ShouldNotBeNil)
		})
	})
}
