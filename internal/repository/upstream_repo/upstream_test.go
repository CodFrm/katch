package upstream_repo

import (
	"context"
	"errors"
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

			up := &upstream_entity.Upstream{Host: "docker.io", Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolRegistry}}
			convey.So(repo.Save(ctx, up), convey.ShouldBeNil)
			convey.So(up.ID, convey.ShouldEqual, 7)
			convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
		})

		convey.Convey("有主键时整行更新，enabled 从真改假不会被当成零值丢掉", func() {
			// 这正是不用 Updates(struct) 的理由：它跳过零值字段，
			// 于是「停用某个上游」会静默地什么都没发生。
			mock.ExpectBegin()
			mock.ExpectExec("UPDATE `upstreams` SET").
				WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), false,
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

func TestRegisterUpstreamInstallsIsolatedRewriteConfig(t *testing.T) {
	convey.Convey("legacy 上游注入使用隔离的事务与 generation", t, func() {
		beforeUpstream := defaultUpstream
		beforeRewriteConfig := defaultRewriteConfig
		t.Cleanup(func() {
			defaultUpstream = beforeUpstream
			defaultRewriteConfig = beforeRewriteConfig
		})

		RegisterRewriteConfig(NewRewriteConfig(nil))
		RegisterUpstream(NewUpstream())
		repo := RewriteConfig()
		ctx := context.Background()

		convey.So(repo.Transaction(ctx, func(txCtx context.Context) error {
			return repo.AdvanceGeneration(txCtx)
		}), convey.ShouldBeNil)
		got, err := repo.Snapshot(ctx)
		convey.So(err, convey.ShouldBeNil)
		convey.So(got.Generation, convey.ShouldEqual, 1)

		convey.So(repo.Transaction(ctx, func(txCtx context.Context) error {
			if err := repo.AdvanceGeneration(txCtx); err != nil {
				return err
			}
			return errors.New("rollback")
		}), convey.ShouldNotBeNil)
		got, err = repo.Snapshot(ctx)
		convey.So(err, convey.ShouldBeNil)
		convey.So(got.Generation, convey.ShouldEqual, 1)
	})
}

func TestRewriteConfigWithoutDatabase(t *testing.T) {
	convey.Convey("未配置数据库的生产 rewrite 仓储明确报错", t, func() {
		repo := NewRewriteConfig(nil)
		convey.So(repo.AdvanceGeneration(context.Background()), convey.ShouldNotBeNil)
		got, err := repo.Snapshot(context.Background())
		convey.So(err, convey.ShouldNotBeNil)
		convey.So(got, convey.ShouldBeNil)
	})
}

func TestRewriteConfigTransaction(t *testing.T) {
	convey.Convey("配置写入与 generation 递增共用事务", t, func() {
		ctx, database, mock := testutils.Database(t)
		repo := NewRewriteConfig(database)
		mock.ExpectBegin()
		mock.ExpectExec("UPDATE `rewrite_states` SET `generation`=generation WHERE id=\\?").
			WithArgs(int64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec("UPDATE `rewrite_states` SET `generation`=generation \\+ 1 WHERE id=\\?").
			WithArgs(int64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()

		err := repo.Transaction(ctx, func(txCtx context.Context) error {
			return repo.AdvanceGeneration(txCtx)
		})
		convey.So(err, convey.ShouldBeNil)
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})

	convey.Convey("配置写入失败时 generation 一起回滚", t, func() {
		ctx, database, mock := testutils.Database(t)
		repo := NewRewriteConfig(database)
		mock.ExpectBegin()
		mock.ExpectExec("UPDATE `rewrite_states` SET `generation`=generation WHERE id=\\?").
			WithArgs(int64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectRollback()

		err := repo.Transaction(ctx, func(context.Context) error { return errors.New("write failed") })
		convey.So(err, convey.ShouldNotBeNil)
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}

func TestRewriteConfigSnapshot(t *testing.T) {
	convey.Convey("generation、site_domain 与启用上游来自同一个事务快照", t, func() {
		ctx, database, mock := testutils.Database(t)
		mock.ExpectBegin()
		mock.ExpectQuery("SELECT \\* FROM `rewrite_states` WHERE id=\\?").
			WithArgs(int64(1), 1).
			WillReturnRows(sqlmock.NewRows([]string{"id", "generation"}).AddRow(1, 12))
		mock.ExpectQuery("SELECT \\* FROM `settings` WHERE `key`=\\?").
			WithArgs("site_domain", 1).
			WillReturnRows(sqlmock.NewRows([]string{"key", "value"}).
				AddRow("site_domain", `"mirror.example.com"`))
		mock.ExpectQuery("SELECT \\* FROM `upstreams` WHERE enabled=\\? ORDER BY host asc").
			WithArgs(true).
			WillReturnRows(sqlmock.NewRows([]string{"id", "host", "protocols", "package_profile", "enabled"}).
				AddRow(7, "pypi.org", `["static"]`, "pypi", true))
		mock.ExpectCommit()

		got, err := NewRewriteConfig(database).Snapshot(ctx)
		convey.So(err, convey.ShouldBeNil)
		convey.So(got.Generation, convey.ShouldEqual, 12)
		convey.So(got.SiteDomain, convey.ShouldEqual, "mirror.example.com")
		convey.So(len(got.Upstreams), convey.ShouldEqual, 1)
		convey.So(got.Upstreams[0].PackageProfile, convey.ShouldEqual, upstream_entity.PackageProfilePyPI)
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
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
