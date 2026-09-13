package event_repo

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/cago-frame/cago/pkg/utils/testutils"
	"github.com/smartystreets/goconvey/convey"

	"github.com/CodFrm/katch/internal/model/entity/event_entity"
)

// 用 sqlmock 而不是真库：这一层要验的是 SQL 拼得对不对——倒序和条数限制写错了
// 在真库上也「跑得通」，只是时间线的头尾反了。

func TestEventRepo_Create(t *testing.T) {
	convey.Convey("追加一条事件", t, func() {
		ctx, _, mock := testutils.Database(t)
		mock.ExpectBegin()
		mock.ExpectExec("INSERT INTO `events`").
			WithArgs("upstream_degraded", "system", int64(7), `{"host":"deb.debian.org"}`, int64(1700000000)).
			WillReturnResult(sqlmock.NewResult(3, 1))
		mock.ExpectCommit()

		event := &event_entity.Event{
			Kind: event_entity.KindUpstreamDegraded, Actor: event_entity.ActorSystem,
			UpstreamID: 7, Detail: `{"host":"deb.debian.org"}`, Createtime: 1700000000,
		}
		convey.So(NewEvent().Create(ctx, event), convey.ShouldBeNil)
		// 自增主键要回填：调用方拿它当这条事件在时间线上的位置。
		convey.So(event.ID, convey.ShouldEqual, 3)
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}

// TestEventRepo_ListNewestFirst 时间线是倒序的，且只取最近 limit 条。
//
// 排序按 id 而不是 createtime：createtime 是秒，同一秒里落的几条在它上面分不出
// 先后，而「改完设置紧接着轮换密钥」恰恰就在同一秒里。
func TestEventRepo_ListNewestFirst(t *testing.T) {
	convey.Convey("按 id 倒序取最近几条", t, func() {
		ctx, _, mock := testutils.Database(t)
		mock.ExpectQuery("SELECT \\* FROM `events` ORDER BY id desc LIMIT \\?").
			WithArgs(2).
			WillReturnRows(sqlmock.NewRows([]string{"id", "kind", "actor", "detail"}).
				AddRow(9, "rule_updated", "admin", `{"rule_id":11}`).
				AddRow(8, "cache_reclaimed", "system", `{"removed":3}`))

		list, err := NewEvent().List(ctx, 2)
		convey.So(err, convey.ShouldBeNil)
		convey.So(len(list), convey.ShouldEqual, 2)
		convey.So(list[0].ID, convey.ShouldEqual, 9)
		convey.So(list[0].Kind, convey.ShouldEqual, event_entity.KindRuleUpdated)
		convey.So(list[1].Actor, convey.ShouldEqual, event_entity.ActorSystem)
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}

// TestEventRepo_ListEmptyIsNotAnError 一台刚装好的镜像站还没有任何事件，
// 那是正常状态：当成错误会让后台概览在第一次打开时就报错。
func TestEventRepo_ListEmptyIsNotAnError(t *testing.T) {
	convey.Convey("一条都没有时给空列表", t, func() {
		ctx, _, mock := testutils.Database(t)
		mock.ExpectQuery("SELECT \\* FROM `events`").
			WillReturnRows(sqlmock.NewRows([]string{"id"}))

		list, err := NewEvent().List(ctx, 50)
		convey.So(err, convey.ShouldBeNil)
		convey.So(len(list), convey.ShouldEqual, 0)
		convey.So(list, convey.ShouldNotBeNil)
	})
}
