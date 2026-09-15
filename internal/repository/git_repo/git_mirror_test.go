package git_repo

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/cago-frame/cago/pkg/utils/testutils"
	"github.com/smartystreets/goconvey/convey"

	"github.com/CodFrm/katch/internal/model/entity/git_entity"
)

// 用 sqlmock 而不是真库：这一层要验的是 SQL 拼得对不对（约定 7）。

func TestGitMirrorRepo_FindByRepo(t *testing.T) {
	convey.Convey("按 host+repo 查镜像", t, func() {
		ctx, _, mock := testutils.Database(t)
		repo := NewGitMirror()

		convey.Convey("查到时把整行读回来", func() {
			mock.ExpectQuery("SELECT \\* FROM `git_mirrors` WHERE host=\\? AND repo=\\?").
				WithArgs("github.com", "/foo/bar.git", 1).
				WillReturnRows(sqlmock.NewRows([]string{"id", "host", "repo", "state", "size_bytes"}).
					AddRow(7, "github.com", "/foo/bar.git", git_entity.MirrorReady, 1024))

			got, err := repo.FindByRepo(ctx, "github.com", "/foo/bar.git")
			convey.So(err, convey.ShouldBeNil)
			convey.So(got.ID, convey.ShouldEqual, 7)
			convey.So(got.State, convey.ShouldEqual, git_entity.MirrorReady)
			convey.So(got.SizeBytes, convey.ShouldEqual, 1024)
			convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
		})

		convey.Convey("查不到时是 (nil, nil) 而不是错误", func() {
			// 「这个仓库还没被镜像过」是拉取路径上的常态：每一个第一次被拉到的
			// 仓库都会走这一条，当成错误就是每次冷拉取都在日志里留一条 error。
			mock.ExpectQuery("SELECT \\* FROM `git_mirrors`").
				WillReturnRows(sqlmock.NewRows([]string{"id"}))

			got, err := repo.FindByRepo(ctx, "github.com", "/never/seen.git")
			convey.So(err, convey.ShouldBeNil)
			convey.So(got, convey.ShouldBeNil)
		})
	})
}

func TestGitMirrorRepo_Save(t *testing.T) {
	convey.Convey("保存镜像记录", t, func() {
		ctx, _, mock := testutils.Database(t)
		repo := NewGitMirror()

		convey.Convey("主键为空时插入，并把自增 id 回填", func() {
			mock.ExpectBegin()
			mock.ExpectExec("INSERT INTO `git_mirrors`").
				WillReturnResult(sqlmock.NewResult(7, 1))
			mock.ExpectCommit()

			mirror := &git_entity.GitMirror{Host: "github.com", Repo: "/foo/bar.git",
				State: git_entity.MirrorPending}
			convey.So(repo.Save(ctx, mirror), convey.ShouldBeNil)
			convey.So(mirror.ID, convey.ShouldEqual, 7)
			convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
		})

		convey.Convey("有主键时整行更新，清空 last_error 不会被当成零值丢掉", func() {
			// 正是不用 Updates(struct) 的理由：它跳过零值字段，于是一条
			// 从 failed 重新建成 ready 的镜像会永远挂着上一次的失败原因。
			mock.ExpectBegin()
			mock.ExpectExec("UPDATE `git_mirrors` SET").
				WithArgs("github.com", "/foo/bar.git", git_entity.MirrorReady,
					sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), "",
					sqlmock.AnyArg(), sqlmock.AnyArg(), int64(7)).
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit()

			convey.So(repo.Save(ctx, &git_entity.GitMirror{ID: 7, Host: "github.com",
				Repo: "/foo/bar.git", State: git_entity.MirrorReady, LastError: ""}),
				convey.ShouldBeNil)
			convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
		})
	})
}

func TestGitMirrorRepo_Touch(t *testing.T) {
	convey.Convey("只更新访问时间，不整行写回", t, func() {
		// 每一次穿透都会碰一下这一列；整行写回会把后台那趟建镜像刚写下的状态
		// 覆盖回调用方手上那份旧的。
		ctx, _, mock := testutils.Database(t)
		mock.ExpectBegin()
		mock.ExpectExec("UPDATE `git_mirrors` SET `last_access_at`=\\? WHERE id=\\?").
			WithArgs(int64(1700000000), int64(7)).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()

		convey.So(NewGitMirror().Touch(ctx, 7, 1700000000), convey.ShouldBeNil)
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}

func TestGitMirrorRepo_Find(t *testing.T) {
	convey.Convey("按主键取一条", t, func() {
		ctx, _, mock := testutils.Database(t)
		repo := NewGitMirror()

		convey.Convey("找到时把整行读回来", func() {
			mock.ExpectQuery("SELECT \\* FROM `git_mirrors` WHERE id=\\?").
				WithArgs(int64(7), 1).
				WillReturnRows(sqlmock.NewRows([]string{"id", "host", "repo", "state"}).
					AddRow(7, "github.com", "/foo/bar.git", git_entity.MirrorReady))

			got, err := repo.Find(ctx, 7)
			convey.So(err, convey.ShouldBeNil)
			convey.So(got.ID, convey.ShouldEqual, 7)
			convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
		})

		convey.Convey("找不到时是 (nil, nil) 而不是错误——管理界面删一条已经不在的镜像不该报错", func() {
			mock.ExpectQuery("SELECT \\* FROM `git_mirrors` WHERE id=\\?").
				WillReturnRows(sqlmock.NewRows([]string{"id"}))

			got, err := repo.Find(ctx, 999)
			convey.So(err, convey.ShouldBeNil)
			convey.So(got, convey.ShouldBeNil)
		})
	})
}

func TestGitMirrorRepo_Delete(t *testing.T) {
	convey.Convey("删一条镜像记录", t, func() {
		ctx, _, mock := testutils.Database(t)
		mock.ExpectBegin()
		mock.ExpectExec("DELETE FROM `git_mirrors` WHERE id=\\?").
			WithArgs(int64(7)).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()

		convey.So(NewGitMirror().Delete(ctx, 7), convey.ShouldBeNil)
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}

func TestGitMirrorRepo_List(t *testing.T) {
	convey.Convey("列出全部镜像记录，按最后访问时间倒序", t, func() {
		ctx, _, mock := testutils.Database(t)
		mock.ExpectQuery("SELECT \\* FROM `git_mirrors` ORDER BY last_access_at desc").
			WillReturnRows(sqlmock.NewRows([]string{"id", "host", "repo"}).
				AddRow(7, "github.com", "/foo/bar.git").
				AddRow(8, "github.com", "/foo/baz.git"))

		list, err := NewGitMirror().List(ctx)
		convey.So(err, convey.ShouldBeNil)
		convey.So(len(list), convey.ShouldEqual, 2)
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}

func TestGitMirrorRepo_TotalSize(t *testing.T) {
	convey.Convey("只统计 ready 状态的体积", t, func() {
		// pending 还没落盘，failed/rejected 建失败时已经把半成品清掉了，
		// 两者都不该算进配额。
		ctx, _, mock := testutils.Database(t)
		mock.ExpectQuery("SELECT COALESCE\\(SUM\\(size_bytes\\), 0\\) FROM `git_mirrors` WHERE state=\\?").
			WithArgs(git_entity.MirrorReady).
			WillReturnRows(sqlmock.NewRows([]string{"COALESCE(SUM(size_bytes), 0)"}).AddRow(2048))

		total, err := NewGitMirror().TotalSize(ctx)
		convey.So(err, convey.ShouldBeNil)
		convey.So(total, convey.ShouldEqual, 2048)
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}

func TestGitMirrorRepo_EvictCandidates(t *testing.T) {
	convey.Convey("按最后访问时间最旧的顺序给可淘汰的 ready 镜像", t, func() {
		ctx, _, mock := testutils.Database(t)
		mock.ExpectQuery("SELECT \\* FROM `git_mirrors` WHERE state=\\? ORDER BY last_access_at asc").
			WithArgs(git_entity.MirrorReady, 10).
			WillReturnRows(sqlmock.NewRows([]string{"id", "host", "repo", "state", "last_access_at"}).
				AddRow(3, "github.com", "/old.git", git_entity.MirrorReady, 100).
				AddRow(7, "github.com", "/newer.git", git_entity.MirrorReady, 200))

		list, err := NewGitMirror().EvictCandidates(ctx, 10)
		convey.So(err, convey.ShouldBeNil)
		convey.So(len(list), convey.ShouldEqual, 2)
		convey.So(list[0].ID, convey.ShouldEqual, 3)
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}
