package cache

import (
	"strings"
	"testing"

	"github.com/smartystreets/goconvey/convey"
)

// presetPatterns 是表单五个预设会填进上游的那几组普通模式。它们是纯数据，同一个
// 表格同时写在 spec、frontend/src/lib/upstream-presets.ts 和这里；这组用例锚定的是
// 引擎读它们的结果。关键一条：Go 不能再写宽泛的 `/@v/`——那样会变的 `@v/list`
// 也会被当成不可变对象永久缓存（spec 问题 3）。
var presetPatterns = map[string][]string{
	"apt":  {"/pool/"},
	"go":   {"/@v/*.info", "/@v/*.mod", "/@v/*.zip"},
	"git":  {"/????????????????????????????????????????/"},
	"pypi": {"/packages/??/??/????????????????????????????????????????????????????????????????/"},
}

// TestIsImmutable 决策 13：「哪些路径不可变」是每条上游记录上的数据，不是代码里
// 按 host 分支判断出来的。这里验的是那张模式表怎么读。
func TestIsImmutable(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		path     string
		want     bool
	}{
		{"没有模式时一律当可变", nil, "/debian/pool/main/n/nginx/nginx_1.22.orig.tar.gz", false},
		{"APT 的 pool/ 作为子串命中", []string{"pool/"}, "/debian/pool/main/n/nginx.deb", true},
		{"APT 的 dists/ 下的索引不该被当成不可变", []string{"pool/"}, "/debian/dists/stable/InRelease", false},
		{"Go proxy 的 @v/ 命中", []string{"/@v/"}, "/github.com/gin-gonic/gin/@v/v1.12.0.zip", true},
		{"Go proxy 的 @latest 不命中", []string{"/@v/"}, "/github.com/gin-gonic/gin/@latest", false},
		{"* 跨路径分隔符匹配", []string{"/blobs/sha256:*"}, "/v2/library/redis/blobs/sha256:abc123", true},
		{"? 逐字符匹配，可用来写定长摘要", []string{"/????????????????????????????????????????/"},
			"/foo/bar/0123456789abcdef0123456789abcdef01234567/x.sh", true},
		{"? 的长度对不上就不命中", []string{"/????????????????????????????????????????/"},
			"/foo/bar/short/x.sh", false},
		{"多条模式里命中任意一条即可", []string{"pool/", "/@v/"}, "/mod/example.com/@v/v1.0.0.info", true},
		{"空模式不匹配任何路径", []string{""}, "/anything", false},
	}
	convey.Convey("按上游的模式列表判定路径是否内容寻址", t, func() {
		for _, c := range cases {
			convey.Convey(c.name, func() {
				convey.So(IsImmutable(c.patterns, c.path), convey.ShouldEqual, c.want)
			})
		}
	})

	convey.Convey("表单预设的代表路径与反例", t, func() {
		representative := []struct {
			name   string
			preset string
			path   string
			want   bool
		}{
			{"APT 的 pool/ 命中", "apt", "/debian/pool/main/n/nginx/nginx_1.22.deb", true},
			{"APT 的 InRelease 保持可变", "apt", "/debian/dists/stable/InRelease", false},
			{"Go 版本文件 .info 命中", "go", "/github.com/gin-gonic/gin/@v/v1.12.0.info", true},
			{"Go 版本文件 .mod 命中", "go", "/github.com/gin-gonic/gin/@v/v1.12.0.mod", true},
			{"Go 版本文件 .zip 命中", "go", "/github.com/gin-gonic/gin/@v/v1.12.0.zip", true},
			{"Go 的 @v/list 保持可变", "go", "/github.com/gin-gonic/gin/@v/list", false},
			{"Go 的 @latest 保持可变", "go", "/github.com/gin-gonic/gin/@latest", false},
			{"Git 40 位 commit 命中", "git",
				"/foo/bar/" + strings.Repeat("0", 40) + "/x.sh", true},
			{"Git 分支名路径保持可变", "git", "/foo/bar/main/x.sh", false},
			{"PyPI 包文件命中", "pypi",
				"/packages/ab/cd/" + strings.Repeat("a", 64) + "/requests-2.32.0-py3-none-any.whl", true},
			{"PyPI 项目索引保持可变", "pypi", "/simple/requests/", false},
		}
		for _, c := range representative {
			convey.Convey(c.name, func() {
				convey.So(IsImmutable(presetPatterns[c.preset], c.path), convey.ShouldEqual, c.want)
			})
		}
	})
}
