package request_log_repo

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/cago-frame/cago/pkg/utils/testutils"
	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"

	"github.com/CodFrm/katch/internal/model/entity/request_log_entity"
	mock_request_log_repo "github.com/CodFrm/katch/internal/repository/request_log_repo/mock"
)

// 用 sqlmock 而不是真库：这一层要验的是 SQL 拼得对不对——批量插入的列与顺序、
// 最近 N 条的排序与条数上限、裁剪的边界，这些写错了在真库上也「跑得通」，
// 只是落进库的数和面板上的顺序不对。
//
// 表名在这一层是 `recent_requests`：testutils.Database 的 gorm 没配命名策略，用的是
// gorm 默认的复数名。运行时 cago 的 db 组件配的是 TablePrefix + SingularTable
// （见 migrations/recent_request_test.go），真库里那张表叫 katch_recent_request。
// 命名策略不在这一层的覆盖范围里，它是对着这里拼出来的语句做断言。

func TestRequestLogRepo_Register(t *testing.T) {
	convey.Convey("注册后能取回同一份实现", t, func() {
		ctrl := gomock.NewController(t)
		repo := mock_request_log_repo.NewMockRequestLogRepo(ctrl)
		RegisterRequestLog(repo)
		convey.So(RecentRequestLog(), convey.ShouldEqual, repo)
	})
}

func TestRequestLogRepo_Save(t *testing.T) {
	convey.Convey("把一批最近请求写进库", t, func() {
		convey.Convey("一次插一批", func() {
			ctx, _, mock := testutils.Database(t)
			// 列的顺序不是随便定的：它跟着实体字段的顺序走，而这条语句是唯一一处把
			// 「实体字段」和「表列」对应起来的地方。顺序写歪了不会报错，只会让
			// duration_ms 落进 bytes、面板上的耗时变成字节数。
			mock.ExpectBegin()
			mock.ExpectExec("INSERT INTO `recent_requests` "+
				"\\(`upstream_id`,`at`,`object`,`result`,`bytes`,`duration_ms`,`createtime`,`updatetime`\\) "+
				"VALUES \\(\\?,\\?,\\?,\\?,\\?,\\?,\\?,\\?\\),\\(\\?,\\?,\\?,\\?,\\?,\\?,\\?,\\?\\)").
				WithArgs(
					int64(7), int64(1700000040), "library/redis", "hit", int64(1024), int64(12), int64(1700000040), int64(1700000040),
					int64(8), int64(1700000041), "debian/dists/bookworm/InRelease", "miss", int64(2048), int64(30), int64(1700000041), int64(1700000041),
				).
				WillReturnResult(sqlmock.NewResult(1, 2))
			mock.ExpectCommit()

			err := NewRequestLog().Save(ctx, []*request_log_entity.RecentRequest{
				{
					UpstreamID: 7, At: 1700000040, Object: "library/redis", Result: "hit",
					Bytes: 1024, DurationMS: 12, Createtime: 1700000040, Updatetime: 1700000040,
				},
				{
					UpstreamID: 8, At: 1700000041, Object: "debian/dists/bookworm/InRelease", Result: "miss",
					Bytes: 2048, DurationMS: 30, Createtime: 1700000041, Updatetime: 1700000041,
				},
			})
			convey.So(err, convey.ShouldBeNil)
			convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
		})

		convey.Convey("空的一批不写库也不是错误", func() {
			// 落库循环在没有新请求时也会走一遍这个缝；GORM 对空切片报 ErrEmptySlice，
			// 放任它冒出去只会让每秒多一条没有意义的 error。
			ctx, _, mock := testutils.Database(t)
			convey.So(NewRequestLog().Save(ctx, nil), convey.ShouldBeNil)
			convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
		})
	})
}

func TestRequestLogRepo_ListRecent(t *testing.T) {
	convey.Convey("按上游取最近的若干条", t, func() {
		convey.Convey("带条数上限取一批", func() {
			ctx, _, mock := testutils.Database(t)
			// 同一秒内落进来的多条靠 id 兜底：只按 at 排的话，同一秒的先后在两次
			// 查询之间可以互换，面板上的顺序会跳。
			mock.ExpectQuery("SELECT \\* FROM `recent_requests` WHERE upstream_id = \\? ORDER BY at DESC, id DESC LIMIT \\?").
				WithArgs(int64(7), 100).
				WillReturnRows(sqlmock.NewRows([]string{"id", "upstream_id", "at", "object", "result", "bytes", "duration_ms", "createtime", "updatetime"}).
					AddRow(2, 7, 1700000041, "library/redis", "miss", 2048, 30, 1700000041, 1700000041).
					AddRow(1, 7, 1700000040, "library/redis", "hit", 1024, 12, 1700000040, 1700000040))

			got, err := NewRequestLog().ListRecent(ctx, 7, 100)
			convey.So(err, convey.ShouldBeNil)
			convey.So(len(got), convey.ShouldEqual, 2)
			convey.So(got[0].ID, convey.ShouldEqual, 2)
			convey.So(got[0].At, convey.ShouldEqual, 1700000041)
			convey.So(got[0].Object, convey.ShouldEqual, "library/redis")
			convey.So(got[0].DurationMS, convey.ShouldEqual, 30)
			convey.So(got[1].Result, convey.ShouldEqual, "hit")
			convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
		})

		convey.Convey("条数上限非正时一条也不查", func() {
			// 不夹这一下，limit 为负时 GORM 会把 LIMIT 整个省掉，查询退化成对上
			// 游全部历史的全表扫——面板每刷新一次就扫一遍。
			ctx, _, mock := testutils.Database(t)
			got, err := NewRequestLog().ListRecent(ctx, 7, -1)
			convey.So(err, convey.ShouldBeNil)
			convey.So(len(got), convey.ShouldEqual, 0)
			convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
		})
	})
}

func TestRequestLogRepo_Prune(t *testing.T) {
	convey.Convey("裁掉保留期之前的行并返回行数", t, func() {
		ctx, _, mock := testutils.Database(t)
		mock.ExpectBegin()
		// 严格小于：保留期起点那一秒本身还在保留期内。
		mock.ExpectExec("DELETE FROM `recent_requests` WHERE at < \\?").
			WithArgs(int64(1690000000)).
			WillReturnResult(sqlmock.NewResult(0, 42))
		mock.ExpectCommit()

		removed, err := NewRequestLog().Prune(ctx, 1690000000)
		convey.So(err, convey.ShouldBeNil)
		convey.So(removed, convey.ShouldEqual, 42)
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}
