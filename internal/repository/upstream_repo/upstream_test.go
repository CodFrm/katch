package upstream_repo

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/cago-frame/cago/pkg/utils/testutils"
	"github.com/smartystreets/goconvey/convey"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
)

// 用 sqlmock 而不是真库：这一层要验的是 SQL 拼得对不对，连上真库只会把
// 方言的副作用混进来，还测不到「有没有多发一条查询」这类问题。

func TestUpstreamRepo_FindByHost(t *testing.T) {
	convey.Convey("按 host 查上游", t, func() {
		ctx, _, mock := testutils.Database(t)
		repo := NewUpstream()

		convey.Convey("查到时把 JSON 文本读回成模式列表", func() {
			mock.ExpectQuery("SELECT \\* FROM `upstreams` WHERE host=\\?").
				WithArgs("deb.debian.org", 1).
				WillReturnRows(sqlmock.NewRows([]string{"id", "host", "enabled", "immutable_patterns"}).
					AddRow(7, "deb.debian.org", true, `["pool/","dists/"]`))

			got, err := repo.FindByHost(ctx, "deb.debian.org")
			convey.So(err, convey.ShouldBeNil)
			convey.So(got.ID, convey.ShouldEqual, 7)
			convey.So([]string(got.ImmutablePatterns), convey.ShouldResemble, []string{"pool/", "dists/"})
			convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
		})

		convey.Convey("查不到时是 (nil, nil) 而不是错误", func() {
			// 「这个 host 不在白名单里」是分发路径上的常态，不是故障；
			// 当成错误会让每一次探测都在日志里留下一条 error。
			mock.ExpectQuery("SELECT \\* FROM `upstreams`").
				WillReturnRows(sqlmock.NewRows([]string{"id"}))

			got, err := repo.FindByHost(ctx, "evil.example.com")
			convey.So(err, convey.ShouldBeNil)
			convey.So(got, convey.ShouldBeNil)
		})
	})
}

func TestUpstreamRepo_List(t *testing.T) {
	convey.Convey("列表按 host 升序，而不是按插入顺序", t, func() {
		ctx, _, mock := testutils.Database(t)
		mock.ExpectQuery("SELECT \\* FROM `upstreams` ORDER BY host asc").
			WillReturnRows(sqlmock.NewRows([]string{"id", "host"}).
				AddRow(2, "deb.debian.org").AddRow(1, "docker.io"))

		got, err := NewUpstream().List(ctx)
		convey.So(err, convey.ShouldBeNil)
		convey.So(len(got), convey.ShouldEqual, 2)
		convey.So(got[0].Host, convey.ShouldEqual, "deb.debian.org")
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}

func TestUpstreamRepo_Save(t *testing.T) {
	convey.Convey("保存上游", t, func() {
		ctx, _, mock := testutils.Database(t)
		repo := NewUpstream()

		convey.Convey("主键为空时插入，并把自增 id 回填", func() {
			mock.ExpectBegin()
			mock.ExpectExec("INSERT INTO `upstreams`").
				WillReturnResult(sqlmock.NewResult(7, 1))
			mock.ExpectCommit()

			up := &upstream_entity.Upstream{Host: "docker.io", Kind: upstream_entity.KindRegistry}
			convey.So(repo.Save(ctx, up), convey.ShouldBeNil)
			convey.So(up.ID, convey.ShouldEqual, 7)
			convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
		})

		convey.Convey("有主键时整行更新，enabled 从真改假不会被当成零值丢掉", func() {
			// 这正是不用 Updates(struct) 的理由：它跳过零值字段，
			// 于是「停用某个上游」会静默地什么都没发生。
			mock.ExpectBegin()
			mock.ExpectExec("UPDATE `upstreams` SET").
				WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), false,
					sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
					sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), int64(7)).
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit()

			convey.So(repo.Save(ctx, &upstream_entity.Upstream{
				ID: 7, Host: "docker.io", Enabled: false,
			}), convey.ShouldBeNil)
			convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
		})
	})
}

func TestUpstreamRepo_Delete(t *testing.T) {
	convey.Convey("按 id 删除", t, func() {
		ctx, _, mock := testutils.Database(t)
		mock.ExpectBegin()
		mock.ExpectExec("DELETE FROM `upstreams` WHERE id=\\?").
			WithArgs(int64(7)).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()

		convey.So(NewUpstream().Delete(ctx, 7), convey.ShouldBeNil)
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}
