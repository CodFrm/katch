package migrations

import (
	"testing"

	"github.com/cago-frame/cago/pkg/utils/testutils"
	"github.com/smartystreets/goconvey/convey"
)

// TestRunMigrations_EmptyListIsNotAnError
//
// gormigrate 在迁移列表为空时返回 "No migration defined"。而「还没有任何迁移」
// 是新项目的正常状态，不是错误——放任它冒出去会让服务在启动时直接 panic。
func TestRunMigrations_EmptyListIsNotAnError(t *testing.T) {
	convey.Convey("迁移列表为空时不报错", t, func() {
		_, gormDB, _ := testutils.Database(t)
		convey.So(runMigrations(gormDB, nil), convey.ShouldBeNil)
	})
}
