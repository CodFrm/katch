package cache_repo

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/smartystreets/goconvey/convey"

	"github.com/CodFrm/katch/internal/model/entity/cache_entity"
)

// 用 sqlmock 而不是真库：这一层要验的是 SQL 拼得对不对——淘汰顺序、pin 的过滤、
// 同一份内容还有没有别的记录引用，这些写错了在真库上也「跑得通」，只是结果不对。

func TestCacheObjectRepo_FindByKey(t *testing.T) {
	convey.Convey("按上游与 key 查缓存记录", t, func() {
		ctx, _, mock := database(t)
		repo := NewCacheObject()

		convey.Convey("查到时带回摘要与大小", func() {
			mock.ExpectQuery("SELECT \\* FROM `katch_cache_object` WHERE upstream_id=\\? AND `key`=\\?").
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
			mock.ExpectQuery("SELECT \\* FROM `katch_cache_object`").
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
		ctx, _, mock := database(t)
		mock.ExpectBegin()
		mock.ExpectExec("UPDATE `katch_cache_object` SET .*hit_count.*WHERE id=\\?").
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
		ctx, _, mock := database(t)
		mock.ExpectQuery("SELECT \\* FROM `katch_cache_object` WHERE immutable=\\? AND pinned=\\? ORDER BY last_access_at asc LIMIT \\?").
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
		ctx, _, mock := database(t)

		convey.Convey("一条记录都没有时是 0 而不是错误", func() {
			// SUM 在空表上给的是 NULL，扫进 int64 会报错——空缓存是正常状态。
			mock.ExpectQuery("SELECT COALESCE\\(SUM\\(size\\), 0\\) FROM `katch_cache_object`").
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
		ctx, _, mock := database(t)
		mock.ExpectQuery("SELECT count\\(\\*\\) FROM `katch_cache_object` WHERE digest=\\?").
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
		ctx, _, mock := database(t)
		mock.ExpectQuery("SELECT count\\(\\*\\) FROM `katch_cache_object` WHERE upstream_id=\\? AND `key` LIKE \\?").
			WithArgs(int64(7), "%redis%").
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
		mock.ExpectQuery("SELECT \\* FROM `katch_cache_object` WHERE upstream_id=\\? AND `key` LIKE \\? ORDER BY last_access_at desc LIMIT \\? OFFSET \\?").
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
		ctx, _, mock := database(t)
		mock.ExpectQuery("SELECT \\* FROM `katch_cache_object` WHERE upstream_id=\\?").
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
		ctx, _, mock := database(t)
		mock.ExpectBegin()
		mock.ExpectExec("UPDATE `katch_cache_object` SET `pinned`=\\?,`updatetime`=\\? WHERE id=\\?").
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()

		convey.So(NewCacheObject().SetPinned(ctx, 3, true), convey.ShouldBeNil)
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}

func TestCacheObjectRepo_PromoteImmutable(t *testing.T) {
	convey.Convey("旧记录提升时只清 TTL 并标成不可变", t, func() {
		ctx, _, mock := database(t)
		mock.ExpectBegin()
		mock.ExpectExec("UPDATE `katch_cache_object` SET `expires_at`=\\?,`immutable`=\\?,`updatetime`=\\? WHERE id=\\?").
			WithArgs(int64(0), true, sqlmock.AnyArg(), int64(3)).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()

		convey.So(NewCacheObject().PromoteImmutable(ctx, 3), convey.ShouldBeNil)
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}

func TestCacheObjectRepo_DeleteExpired(t *testing.T) {
	convey.Convey("删除前原子复核记录仍是过期可变对象", t, func() {
		ctx, _, mock := database(t)
		mock.ExpectBegin()
		mock.ExpectExec("DELETE FROM `katch_cache_object` WHERE id=\\? AND immutable=\\? AND expires_at>0 AND expires_at<=\\? AND pinned=\\?").
			WithArgs(int64(3), false, int64(1000), false).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()

		removed, err := NewCacheObject().DeleteExpired(ctx, 3, 1000)
		convey.So(err, convey.ShouldBeNil)
		convey.So(removed, convey.ShouldBeTrue)
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}

func TestCacheObjectRepo_DeleteUnchanged(t *testing.T) {
	convey.Convey("人手清除只删列出时看到的那一份", t, func() {
		ctx, _, mock := database(t)

		convey.Convey("批量清除还要求它仍未被 pin", func() {
			mock.ExpectBegin()
			mock.ExpectExec("DELETE FROM `katch_cache_object` WHERE id=\\? AND digest=\\? AND pinned=\\?$").
				WithArgs(int64(3), "sha256:a", false).
				WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectCommit()

			removed, err := NewCacheObject().DeleteUnchanged(ctx, 3, "sha256:a", true)
			convey.So(err, convey.ShouldBeNil)
			convey.So(removed, convey.ShouldBeFalse)
			convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
		})

		convey.Convey("指名清一条时不看 pin", func() {
			mock.ExpectBegin()
			mock.ExpectExec("DELETE FROM `katch_cache_object` WHERE id=\\? AND digest=\\?$").
				WithArgs(int64(3), "sha256:a").
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit()

			removed, err := NewCacheObject().DeleteUnchanged(ctx, 3, "sha256:a", false)
			convey.So(err, convey.ShouldBeNil)
			convey.So(removed, convey.ShouldBeTrue)
			convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
		})
	})
}

func TestCacheObjectRepo_Delete(t *testing.T) {
	convey.Convey("删除一条缓存记录", t, func() {
		ctx, _, mock := database(t)
		mock.ExpectBegin()
		mock.ExpectExec("DELETE FROM `katch_cache_object` WHERE id=\\?").
			WithArgs(int64(3)).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()

		convey.So(NewCacheObject().Delete(ctx, 3), convey.ShouldBeNil)
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}

func TestCacheObjectRepo_SizeByUpstream(t *testing.T) {
	convey.Convey("按上游统计缓存占用", t, func() {
		ctx, _, mock := database(t)
		repo := NewCacheObject()

		convey.Convey("每个有缓存的上游一行", func() {
			mock.ExpectQuery("SELECT upstream_id,COALESCE\\(SUM\\(size\\), 0\\) AS size " +
				"FROM `katch_cache_object` GROUP BY `upstream_id`").
				WillReturnRows(sqlmock.NewRows([]string{"upstream_id", "size"}).
					AddRow(7, 4096).
					AddRow(8, 1024))

			got, err := repo.SizeByUpstream(ctx)
			convey.So(err, convey.ShouldBeNil)
			convey.So(len(got), convey.ShouldEqual, 2)
			convey.So(got[7], convey.ShouldEqual, 4096)
			convey.So(got[8], convey.ShouldEqual, 1024)
			convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
		})

		convey.Convey("一条记录都没有时是空表而不是错误", func() {
			// 空缓存是一个新装的镜像站的正常状态，当成错误会让首页打不开。
			mock.ExpectQuery("SELECT upstream_id,COALESCE\\(SUM\\(size\\), 0\\) AS size").
				WillReturnRows(sqlmock.NewRows([]string{"upstream_id", "size"}))

			got, err := repo.SizeByUpstream(ctx)
			convey.So(err, convey.ShouldBeNil)
			convey.So(len(got), convey.ShouldEqual, 0)
		})
	})
}

func TestCacheObjectRepo_ExpiredBefore(t *testing.T) {
	convey.Convey("过期清理只挑真的过期了的可变对象", t, func() {
		ctx, _, mock := database(t)
		// expires_at>0 与 immutable=false 都不能少：正常不可变对象的 expires_at 是 0，
		// 但升级遗留或人工修复可能留下 immutable=true 且旧 TTL 仍为正的组合。
		mock.ExpectQuery("SELECT \\* FROM `katch_cache_object` WHERE expires_at>0 AND expires_at<=\\? AND immutable=\\? AND pinned=\\? ORDER BY expires_at asc LIMIT \\?").
			WithArgs(int64(1000), false, false, 2).
			WillReturnRows(sqlmock.NewRows([]string{"id", "size", "expires_at"}).
				AddRow(9, 300, 500).AddRow(10, 400, 900))

		got, err := NewCacheObject().ExpiredBefore(ctx, 1000, 2)
		convey.So(err, convey.ShouldBeNil)
		convey.So(len(got), convey.ShouldEqual, 2)
		convey.So(got[0].ID, convey.ShouldEqual, 9)
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}

func TestCacheObjectRepo_CountByUpstream(t *testing.T) {
	convey.Convey("按上游统计缓存对象数", t, func() {
		ctx, _, mock := database(t)
		mock.ExpectQuery("SELECT upstream_id,COUNT\\(\\*\\) AS count FROM `katch_cache_object` GROUP BY `upstream_id`").
			WillReturnRows(sqlmock.NewRows([]string{"upstream_id", "count"}).
				AddRow(1, 3).AddRow(2, 7))

		got, err := NewCacheObject().CountByUpstream(ctx)
		convey.So(err, convey.ShouldBeNil)
		convey.So(got[1], convey.ShouldEqual, 3)
		convey.So(got[2], convey.ShouldEqual, 7)
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}
