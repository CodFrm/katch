package rollup_repo

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/cago-frame/cago/pkg/utils/testutils"
	"github.com/smartystreets/goconvey/convey"

	"github.com/CodFrm/katch/internal/model/entity/rollup_entity"
)

// 用 sqlmock 而不是真库：这一层要验的是 SQL 拼得对不对——区间的左闭右开、
// 空区间上的 SUM、裁剪的边界，这些写错了在真库上也「跑得通」，只是数不对。

func TestTrafficRollupRepo_FindByBucket(t *testing.T) {
	convey.Convey("按上游与整分钟找一行", t, func() {
		ctx, _, mock := testutils.Database(t)
		repo := NewTrafficRollup()

		convey.Convey("找到时带回累计量", func() {
			mock.ExpectQuery("SELECT \\* FROM `traffic_rollups` WHERE upstream_id=\\? AND bucket=\\?").
				WithArgs(int64(7), int64(1700000040), 1).
				WillReturnRows(sqlmock.NewRows([]string{"id", "upstream_id", "bucket", "requests", "hits"}).
					AddRow(3, 7, 1700000040, 10, 4))

			got, err := repo.FindByBucket(ctx, 7, 1700000040)
			convey.So(err, convey.ShouldBeNil)
			convey.So(got.ID, convey.ShouldEqual, 3)
			convey.So(got.Requests, convey.ShouldEqual, 10)
			convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
		})

		convey.Convey("这一分钟还没落过不是错误", func() {
			// 每分钟的第一次落库都会走到这里，当成错误会让日志每分钟多一条 error。
			mock.ExpectQuery("SELECT \\* FROM `traffic_rollups`").
				WillReturnRows(sqlmock.NewRows([]string{"id"}))

			got, err := repo.FindByBucket(ctx, 7, 1700000040)
			convey.So(err, convey.ShouldBeNil)
			convey.So(got, convey.ShouldBeNil)
		})
	})
}

func TestTrafficRollupRepo_Sum(t *testing.T) {
	convey.Convey("区间聚合是左闭右开的", t, func() {
		ctx, _, mock := testutils.Database(t)
		// 左闭右开：相邻两个区间各自聚合时，边界那一分钟只能被算进一边，
		// 否则界面上「24 小时」和「7 天」的总和永远对不上。
		mock.ExpectQuery("SELECT COALESCE\\(SUM\\(requests\\), 0\\).*FROM `traffic_rollups` WHERE bucket >= \\? AND bucket < \\?").
			WithArgs(int64(1700000000), int64(1700086400)).
			WillReturnRows(sqlmock.NewRows([]string{"requests", "hits", "denied", "origin_errors", "bytes_served", "bytes_origin"}).
				AddRow(100, 60, 5, 3, 4096, 1024))

		got, err := NewTrafficRollup().Sum(ctx, 1700000000, 1700086400)
		convey.So(err, convey.ShouldBeNil)
		convey.So(got.Requests, convey.ShouldEqual, 100)
		convey.So(got.Hits, convey.ShouldEqual, 60)
		convey.So(got.BytesOrigin, convey.ShouldEqual, 1024)
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}

func TestTrafficRollupRepo_SumByUpstream(t *testing.T) {
	convey.Convey("按上游分组聚合", t, func() {
		ctx, _, mock := testutils.Database(t)
		mock.ExpectQuery("SELECT upstream_id,COALESCE\\(SUM\\(requests\\), 0\\).*FROM `traffic_rollups` WHERE bucket >= \\? AND bucket < \\? GROUP BY `upstream_id`").
			WithArgs(int64(1700000000), int64(1700086400)).
			WillReturnRows(sqlmock.NewRows([]string{"upstream_id", "requests", "hits", "denied", "origin_errors", "bytes_served", "bytes_origin"}).
				AddRow(7, 100, 60, 0, 0, 4096, 1024).
				AddRow(8, 5, 0, 5, 0, 0, 0))

		got, err := NewTrafficRollup().SumByUpstream(ctx, 1700000000, 1700086400)
		convey.So(err, convey.ShouldBeNil)
		convey.So(len(got), convey.ShouldEqual, 2)
		convey.So(got[0].UpstreamID, convey.ShouldEqual, 7)
		convey.So(got[1].Denied, convey.ShouldEqual, 5)
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}

func TestTrafficRollupRepo_Prune(t *testing.T) {
	convey.Convey("裁掉保留期之前的行", t, func() {
		ctx, _, mock := testutils.Database(t)
		mock.ExpectBegin()
		// 严格小于：保留期起点那一分钟本身还在保留期内。
		mock.ExpectExec("DELETE FROM `traffic_rollups` WHERE bucket < \\?").
			WithArgs(int64(1690000000)).
			WillReturnResult(sqlmock.NewResult(0, 42))
		mock.ExpectCommit()

		removed, err := NewTrafficRollup().Prune(ctx, 1690000000)
		convey.So(err, convey.ShouldBeNil)
		convey.So(removed, convey.ShouldEqual, 42)
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}

func TestTrafficRollupRepo_Save(t *testing.T) {
	convey.Convey("落一行分钟桶", t, func() {
		ctx, _, mock := testutils.Database(t)
		mock.ExpectBegin()
		mock.ExpectExec("INSERT INTO `traffic_rollups`").
			WillReturnResult(sqlmock.NewResult(9, 1))
		mock.ExpectCommit()

		row := &rollup_entity.TrafficRollup{UpstreamID: 7, Bucket: 1700000040, Requests: 3}
		convey.So(NewTrafficRollup().Save(ctx, row), convey.ShouldBeNil)
		convey.So(row.ID, convey.ShouldEqual, 9)
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}

func TestTrafficRollupRepo_SumByDay(t *testing.T) {
	convey.Convey("按自然日分组聚合", t, func() {
		ctx, _, mock := testutils.Database(t)
		// 分组键在 SQL 里算：把 14 天的分钟桶捞回 Go 里再分组，等于每次打开
		// 首页都要扫回 14×1440 行。UTC 而不是进程时区：桶本身是 UTC 秒。
		mock.ExpectQuery("SELECT \\(bucket - bucket % 86400\\) AS day,COALESCE\\(SUM\\(requests\\), 0\\).*"+
			"FROM `traffic_rollups` WHERE bucket >= \\? AND bucket < \\? GROUP BY `day` ORDER BY day asc").
			WithArgs(int64(1699920000), int64(1700092800)).
			WillReturnRows(sqlmock.NewRows([]string{"day", "requests", "hits", "denied", "origin_errors", "bytes_served", "bytes_origin"}).
				AddRow(1699920000, 100, 60, 5, 3, 4096, 1024).
				AddRow(1700006400, 20, 20, 0, 0, 8192, 0))

		got, err := NewTrafficRollup().SumByDay(ctx, 1699920000, 1700092800)
		convey.So(err, convey.ShouldBeNil)
		convey.So(len(got), convey.ShouldEqual, 2)
		convey.So(got[0].Day, convey.ShouldEqual, 1699920000)
		convey.So(got[0].Requests, convey.ShouldEqual, 100)
		convey.So(got[0].Hits, convey.ShouldEqual, 60)
		convey.So(got[0].BytesOrigin, convey.ShouldEqual, 1024)
		convey.So(got[1].Day, convey.ShouldEqual, 1700006400)
		convey.So(got[1].BytesServed, convey.ShouldEqual, 8192)
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}
