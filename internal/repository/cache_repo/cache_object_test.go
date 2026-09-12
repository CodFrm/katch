package cache_repo

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/cago-frame/cago/pkg/utils/testutils"
	"github.com/smartystreets/goconvey/convey"

	"github.com/CodFrm/katch/internal/model/entity/cache_entity"
)

// 用 sqlmock 而不是真库：这一层要验的是 SQL 拼得对不对——淘汰顺序、pin 的过滤、
// 同一份内容还有没有别的记录引用，这些写错了在真库上也「跑得通」，只是结果不对。

func TestCacheObjectRepo_FindByKey(t *testing.T) {
	convey.Convey("按上游与 key 查缓存记录", t, func() {
		ctx, _, mock := testutils.Database(t)
		repo := NewCacheObject()

		convey.Convey("查到时带回摘要与大小", func() {
			mock.ExpectQuery("SELECT \\* FROM `cache_objects` WHERE upstream_id=\\? AND `key`=\\?").
				WithArgs(int64(7), "/dists/stable/InRelease", 1).
				WillReturnRows(sqlmock.NewRows([]string{"id", "upstream_id", "key", "digest", "size"}).
					AddRow(3, 7, "/dists/stable/InRelease", "sha256:aa", 15))

			got, err := repo.FindByKey(ctx, 7, "/dists/stable/InRelease")
			convey.So(err, convey.ShouldBeNil)
			convey.So(got.ID, convey.ShouldEqual, 3)
			convey.So(got.Digest, convey.ShouldEqual, "sha256:aa")
			convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
		})

		convey.Convey("没缓存过不是错误", func() {
			// 未命中是拉取路径上的常态，当成错误会让每一次冷拉取都留下一条 error。
			mock.ExpectQuery("SELECT \\* FROM `cache_objects`").
				WillReturnRows(sqlmock.NewRows([]string{"id"}))

			got, err := repo.FindByKey(ctx, 7, "/x")
			convey.So(err, convey.ShouldBeNil)
			convey.So(got, convey.ShouldBeNil)
		})
	})
}

// TestCacheObjectRepo_Touch 命中时只更新访问时间与命中数。
//
// 不整行 Save：命中是热路径上最频繁的写，整行写回既会把并发的另一次写盖掉，
// 又会把一个只读请求变成一次全列更新。
func TestCacheObjectRepo_Touch(t *testing.T) {
	convey.Convey("命中时只更新 last_access_at 与 hit_count", t, func() {
		ctx, _, mock := testutils.Database(t)
		mock.ExpectBegin()
		mock.ExpectExec("UPDATE `cache_objects` SET .*hit_count.*WHERE id=\\?").
			WithArgs(int64(1700000000), int64(3)).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()

		convey.So(NewCacheObject().Touch(ctx, 3, 1700000000), convey.ShouldBeNil)
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}

// TestCacheObjectRepo_EvictCandidates 淘汰只能落在**不可变且未被 pin** 的对象上。
//
// 可变对象由 TTL 自己过期，pin 的对象是人明确要求留下的；把它们卷进 LRU，
// 表现就是「刚 pin 的基础镜像层过两天又没了」。
func TestCacheObjectRepo_EvictCandidates(t *testing.T) {
	convey.Convey("淘汰候选按最久未访问排序，且排除 pin 与可变对象", t, func() {
		ctx, _, mock := testutils.Database(t)
		mock.ExpectQuery("SELECT \\* FROM `cache_objects` WHERE immutable=\\? AND pinned=\\? ORDER BY last_access_at asc LIMIT \\?").
			WithArgs(true, false, 2).
			WillReturnRows(sqlmock.NewRows([]string{"id", "size", "last_access_at"}).
				AddRow(5, 100, 1).AddRow(6, 200, 2))

		got, err := NewCacheObject().EvictCandidates(ctx, 2)
		convey.So(err, convey.ShouldBeNil)
		convey.So(len(got), convey.ShouldEqual, 2)
		convey.So(got[0].ID, convey.ShouldEqual, 5)
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}

func TestCacheObjectRepo_TotalSize(t *testing.T) {
	convey.Convey("统计缓存占用", t, func() {
		ctx, _, mock := testutils.Database(t)

		convey.Convey("一条记录都没有时是 0 而不是错误", func() {
			// SUM 在空表上给的是 NULL，扫进 int64 会报错——空缓存是正常状态。
			mock.ExpectQuery("SELECT COALESCE\\(SUM\\(size\\), 0\\) FROM `cache_objects`").
				WillReturnRows(sqlmock.NewRows([]string{"total"}).AddRow(0))

			got, err := NewCacheObject().TotalSize(ctx)
			convey.So(err, convey.ShouldBeNil)
			convey.So(got, convey.ShouldEqual, 0)
			convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
		})
	})
}

// TestCacheObjectRepo_CountByDigest 内容寻址的直接后果：删一条记录之前必须先问
// 「还有没有别的记录指着同一份内容」，否则删掉文件会把另一条记录变成坏缓存。
func TestCacheObjectRepo_CountByDigest(t *testing.T) {
	convey.Convey("按摘要数还有多少条记录引用同一份内容", t, func() {
		ctx, _, mock := testutils.Database(t)
		mock.ExpectQuery("SELECT count\\(\\*\\) FROM `cache_objects` WHERE digest=\\?").
			WithArgs("sha256:aa").
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))

		got, err := NewCacheObject().CountByDigest(ctx, "sha256:aa")
		convey.So(err, convey.ShouldBeNil)
		convey.So(got, convey.ShouldEqual, 2)
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}

func TestCacheObjectRepo_Search(t *testing.T) {
	convey.Convey("按上游与关键字搜索缓存对象", t, func() {
		ctx, _, mock := testutils.Database(t)
		mock.ExpectQuery("SELECT count\\(\\*\\) FROM `cache_objects` WHERE upstream_id=\\? AND `key` LIKE \\?").
			WithArgs(int64(7), "%redis%").
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
		mock.ExpectQuery("SELECT \\* FROM `cache_objects` WHERE upstream_id=\\? AND `key` LIKE \\? ORDER BY last_access_at desc LIMIT \\? OFFSET \\?").
			WithArgs(int64(7), "%redis%", 10, 10).
			WillReturnRows(sqlmock.NewRows([]string{"id", "key"}).AddRow(9, "/redis/x"))

		list, total, err := NewCacheObject().Search(ctx, &cache_entity.SearchOption{
			UpstreamID: 7, Keyword: "redis", Offset: 10, Limit: 10,
		})
		convey.So(err, convey.ShouldBeNil)
		convey.So(total, convey.ShouldEqual, 1)
		convey.So(len(list), convey.ShouldEqual, 1)
		convey.So(list[0].Key, convey.ShouldEqual, "/redis/x")
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}

func TestCacheObjectRepo_ListByUpstream(t *testing.T) {
	convey.Convey("按上游列出全部缓存对象，供按上游清缓存用", t, func() {
		ctx, _, mock := testutils.Database(t)
		mock.ExpectQuery("SELECT \\* FROM `cache_objects` WHERE upstream_id=\\?").
			WithArgs(int64(7)).
			WillReturnRows(sqlmock.NewRows([]string{"id", "digest"}).AddRow(1, "sha256:aa"))

		got, err := NewCacheObject().ListByUpstream(ctx, 7)
		convey.So(err, convey.ShouldBeNil)
		convey.So(len(got), convey.ShouldEqual, 1)
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}

func TestCacheObjectRepo_SetPinned(t *testing.T) {
	convey.Convey("pin 只改 pinned 一列", t, func() {
		ctx, _, mock := testutils.Database(t)
		mock.ExpectBegin()
		mock.ExpectExec("UPDATE `cache_objects` SET `pinned`=\\?,`updatetime`=\\? WHERE id=\\?").
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()

		convey.So(NewCacheObject().SetPinned(ctx, 3, true), convey.ShouldBeNil)
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}

func TestCacheObjectRepo_Delete(t *testing.T) {
	convey.Convey("删除一条缓存记录", t, func() {
		ctx, _, mock := testutils.Database(t)
		mock.ExpectBegin()
		mock.ExpectExec("DELETE FROM `cache_objects` WHERE id=\\?").
			WithArgs(int64(3)).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()

		convey.So(NewCacheObject().Delete(ctx, 3), convey.ShouldBeNil)
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}
