package cache_repo

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/cago-frame/cago/pkg/utils/testutils"
	"github.com/smartystreets/goconvey/convey"
)

func TestCacheObjectRepo_ScanByUpstream(t *testing.T) {
	convey.Convey("按 id 分批扫一个上游的记录，只取镜像视图用得上的列", t, func() {
		ctx, _, mock := testutils.Database(t)
		mock.ExpectQuery("SELECT id,upstream_id,`key`,digest,size,immutable,pinned,expires_at,last_access_at,hit_count "+
			"FROM `cache_objects` WHERE upstream_id=\\? AND id>\\? ORDER BY id LIMIT \\?$").
			WithArgs(int64(7), int64(100), 2).
			WillReturnRows(sqlmock.NewRows([]string{"id", "upstream_id", "key", "size"}).
				AddRow(101, 7, "/redis/manifests/7", 10).AddRow(105, 7, "/redis/blobs/sha256:a", 20))

		got, err := NewCacheObject().ScanByUpstream(ctx, 7, 100, 2)
		convey.So(err, convey.ShouldBeNil)
		convey.So(len(got), convey.ShouldEqual, 2)
		convey.So(got[0].Key, convey.ShouldEqual, "/redis/manifests/7")
		convey.So(got[1].ID, convey.ShouldEqual, 105)
		convey.So(got[1].Size, convey.ShouldEqual, 20)
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}
