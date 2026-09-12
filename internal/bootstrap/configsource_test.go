package bootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cago-frame/cago/configs/source"
	"github.com/smartystreets/goconvey/convey"
)

const sampleConfig = `# 这行注释必须活下来
env: DEV
debug: true
db:
  driver: sqlite
  # 内嵌注释同样不能丢
  dsn: "file:./data/katch.db"
`

func writeSample(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	convey.So(os.WriteFile(path, []byte(sampleConfig), 0o600), convey.ShouldBeNil)
	return path
}

// TestConfigSource_MissingKeyDoesNotRewriteFile
//
// 这是换掉 cago 自带文件源的全部理由：它在 key 缺失时会把零值写入并覆写整个文件，
// 于是注释被擦光、工作区凭空多出一份配置改动。
func TestConfigSource_MissingKeyDoesNotRewriteFile(t *testing.T) {
	convey.Convey("读不存在的 key 不会改写配置文件", t, func() {
		path := writeSample(t)
		before, err := os.ReadFile(path)
		convey.So(err, convey.ShouldBeNil)

		src, err := NewConfigSource(path)
		convey.So(err, convey.ShouldBeNil)

		var v string
		err = src.Scan(context.Background(), "redis", &v)
		convey.So(errors.Is(err, source.ErrNotFound), convey.ShouldBeTrue)

		after, err := os.ReadFile(path)
		convey.So(err, convey.ShouldBeNil)
		convey.So(string(after), convey.ShouldEqual, string(before))
		convey.So(string(after), convey.ShouldContainSubstring, "# 这行注释必须活下来")
		convey.So(string(after), convey.ShouldContainSubstring, "# 内嵌注释同样不能丢")
	})
}

// TestConfigSource_ScanExistingKeys 已存在的 key 要能正常读出，包括嵌套结构。
func TestConfigSource_ScanExistingKeys(t *testing.T) {
	convey.Convey("存在的 key 能读出来", t, func() {
		src, err := NewConfigSource(writeSample(t))
		convey.So(err, convey.ShouldBeNil)
		ctx := context.Background()

		var env string
		convey.So(src.Scan(ctx, "env", &env), convey.ShouldBeNil)
		convey.So(env, convey.ShouldEqual, "DEV")

		var debug bool
		convey.So(src.Scan(ctx, "debug", &debug), convey.ShouldBeNil)
		convey.So(debug, convey.ShouldBeTrue)

		var db struct {
			Driver string `yaml:"driver"`
			Dsn    string `yaml:"dsn"`
		}
		convey.So(src.Scan(ctx, "db", &db), convey.ShouldBeNil)
		convey.So(db.Driver, convey.ShouldEqual, "sqlite")
		convey.So(db.Dsn, convey.ShouldEqual, "file:./data/katch.db")

		has, err := src.Has(ctx, "db")
		convey.So(err, convey.ShouldBeNil)
		convey.So(has, convey.ShouldBeTrue)
		has, err = src.Has(ctx, "redis")
		convey.So(err, convey.ShouldBeNil)
		convey.So(has, convey.ShouldBeFalse)
	})
}
