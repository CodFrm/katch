package migrations

import (
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/go-gormigrate/gormigrate/v2"
	"github.com/smartystreets/goconvey/convey"
	"gorm.io/gorm"

	"github.com/CodFrm/katch/internal/model/entity/git_entity"
)

// TestGitMirrorMigration 建出来的表要能被 repository 真的读写。
//
// 同 event 那条：迁移的用例连真 sqlite，因为要验的正是方言副作用——唯一索引
// 这种东西在 sqlmock 里怎么写都是通的。
func TestGitMirrorMigration(t *testing.T) {
	convey.Convey("git_mirror 迁移在 sqlite 上建得出来", t, func() {
		gormDB, err := gorm.Open(sqlite.Open("file:git_mirror?mode=memory&cache=shared"), &gorm.Config{})
		convey.So(err, convey.ShouldBeNil)
		sqlDB, err := gormDB.DB()
		convey.So(err, convey.ShouldBeNil)
		defer func() { _ = sqlDB.Close() }()

		convey.So(RunMigrations(gormDB), convey.ShouldBeNil)

		convey.Convey("写进去的镜像能原样读回来", func() {
			mirror := &git_entity.GitMirror{
				Host: "github.com", Repo: "/foo/bar.git",
				State: git_entity.MirrorPending, Createtime: 1700000000, Updatetime: 1700000000,
			}
			convey.So(gormDB.Create(mirror).Error, convey.ShouldBeNil)
			convey.So(mirror.ID, convey.ShouldBeGreaterThan, 0)

			got := &git_entity.GitMirror{}
			convey.So(gormDB.Where("id=?", mirror.ID).First(got).Error, convey.ShouldBeNil)
			convey.So(got.Host, convey.ShouldEqual, "github.com")
			convey.So(got.Repo, convey.ShouldEqual, "/foo/bar.git")
			convey.So(got.State, convey.ShouldEqual, git_entity.MirrorPending)
			// 没写过的几列要有默认值，不能是 NULL——扫进 int64 会直接报错。
			convey.So(got.SizeBytes, convey.ShouldEqual, 0)
			convey.So(got.LastSyncAt, convey.ShouldEqual, 0)
			convey.So(got.LastAccessAt, convey.ShouldEqual, 0)
			convey.So(got.LastError, convey.ShouldEqual, "")
		})

		convey.Convey("同一个主机下的同一个仓库只能有一条", func() {
			// host+repo 是这张表的业务主键：并发的两次穿透会同时想登记同一个
			// 仓库，没有这条唯一索引，盘上一份镜像会对应库里两条真相。
			first := &git_entity.GitMirror{Host: "gitlab.com", Repo: "/a/b.git",
				State: git_entity.MirrorReady, Createtime: 1, Updatetime: 1}
			convey.So(gormDB.Create(first).Error, convey.ShouldBeNil)
			second := &git_entity.GitMirror{Host: "gitlab.com", Repo: "/a/b.git",
				State: git_entity.MirrorPending, Createtime: 2, Updatetime: 2}
			convey.So(gormDB.Create(second).Error, convey.ShouldNotBeNil)
		})

		convey.Convey("不同主机下的同名仓库互不相干", func() {
			convey.So(gormDB.Create(&git_entity.GitMirror{Host: "a.example",
				Repo: "/x.git", State: git_entity.MirrorReady, Createtime: 1, Updatetime: 1}).
				Error, convey.ShouldBeNil)
			convey.So(gormDB.Create(&git_entity.GitMirror{Host: "b.example",
				Repo: "/x.git", State: git_entity.MirrorReady, Createtime: 1, Updatetime: 1}).
				Error, convey.ShouldBeNil)
		})

		convey.Convey("迁移可回滚，不会把别的表一起带走", func() {
			convey.So(gormigrate.New(gormDB, gormigrate.DefaultOptions, migrationList()).
				RollbackMigration(gitMirror()), convey.ShouldBeNil)
			convey.So(gormDB.Migrator().HasTable(&git_entity.GitMirror{}), convey.ShouldBeFalse)
		})
	})
}
