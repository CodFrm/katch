package rule_repo

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/cago-frame/cago/pkg/utils/testutils"
	"github.com/smartystreets/goconvey/convey"

	"github.com/CodFrm/katch/internal/model/entity/rule_entity"
)

// 用 sqlmock 而不是真库：这一层要验的是 SQL 拼得对不对，连上真库只会把方言的
// 副作用混进来，还测不到「有没有多发一条查询」这类问题。

func TestAccessRuleRepo_List(t *testing.T) {
	convey.Convey("列出全部规则，全局的排在前面", t, func() {
		ctx, _, mock := testutils.Database(t)
		// 求值时的顺序由具体度定序决定，与这里无关；这个排序是给管理界面看的，
		// 让全局规则和同一个上游的规则各自聚在一起。
		mock.ExpectQuery("SELECT \\* FROM `access_rules` ORDER BY upstream_id asc,pattern asc").
			WillReturnRows(sqlmock.NewRows([]string{"id", "upstream_id", "action", "pattern"}).
				AddRow(1, 0, "deny", "*:latest").
				AddRow(2, 7, "allow", "dists/*"))

		got, err := NewAccessRule().List(ctx)
		convey.So(err, convey.ShouldBeNil)
		convey.So(len(got), convey.ShouldEqual, 2)
		convey.So(got[0].Global(), convey.ShouldBeTrue)
		convey.So(got[0].Pattern, convey.ShouldEqual, "*:latest")
		convey.So(got[1].UpstreamID, convey.ShouldEqual, 7)
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}

func TestAccessRuleRepo_Find(t *testing.T) {
	convey.Convey("按 id 查规则", t, func() {
		ctx, _, mock := testutils.Database(t)
		repo := NewAccessRule()

		convey.Convey("查到时读回全部字段", func() {
			mock.ExpectQuery("SELECT \\* FROM `access_rules` WHERE id=\\?").
				WithArgs(int64(3), 1).
				WillReturnRows(sqlmock.NewRows([]string{"id", "upstream_id", "action", "pattern"}).
					AddRow(3, 7, "deny", "dists/*"))

			got, err := repo.Find(ctx, 3)
			convey.So(err, convey.ShouldBeNil)
			convey.So(got.Action, convey.ShouldEqual, rule_entity.ActionDeny)
			convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
		})

		convey.Convey("查不到时是 (nil, nil) 而不是错误", func() {
			// 和上游仓储同一个约定：「不存在」由上层决定算 404 还是算可以创建。
			mock.ExpectQuery("SELECT \\* FROM `access_rules`").
				WillReturnRows(sqlmock.NewRows([]string{"id"}))

			got, err := repo.Find(ctx, 404)
			convey.So(err, convey.ShouldBeNil)
			convey.So(got, convey.ShouldBeNil)
		})
	})
}

func TestAccessRuleRepo_Save(t *testing.T) {
	convey.Convey("保存规则", t, func() {
		ctx, _, mock := testutils.Database(t)
		repo := NewAccessRule()

		convey.Convey("主键为空时插入，并把自增 id 回填", func() {
			mock.ExpectBegin()
			mock.ExpectExec("INSERT INTO `access_rules`").WillReturnResult(sqlmock.NewResult(5, 1))
			mock.ExpectCommit()

			r := &rule_entity.AccessRule{Action: rule_entity.ActionDeny, Pattern: "*:latest"}
			convey.So(repo.Save(ctx, r), convey.ShouldBeNil)
			convey.So(r.ID, convey.ShouldEqual, 5)
			convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
		})

		convey.Convey("有主键时整行更新，upstream_id 从有值改回 0 不会被当成零值丢掉", func() {
			// 「把一条上游内规则改成全局规则」正好是一次把字段改成零值的更新，
			// Updates(struct) 会静默跳过它，于是界面上改完毫无效果。
			mock.ExpectBegin()
			mock.ExpectExec("UPDATE `access_rules` SET").
				WithArgs(int64(0), rule_entity.ActionDeny, "*:latest",
					sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), int64(5)).
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit()

			convey.So(repo.Save(ctx, &rule_entity.AccessRule{
				ID: 5, UpstreamID: 0, Action: rule_entity.ActionDeny, Pattern: "*:latest",
			}), convey.ShouldBeNil)
			convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
		})
	})
}

func TestAccessRuleRepo_Delete(t *testing.T) {
	convey.Convey("按 id 删除", t, func() {
		ctx, _, mock := testutils.Database(t)
		mock.ExpectBegin()
		mock.ExpectExec("DELETE FROM `access_rules` WHERE id=\\?").
			WithArgs(int64(5)).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()

		convey.So(NewAccessRule().Delete(ctx, 5), convey.ShouldBeNil)
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}
