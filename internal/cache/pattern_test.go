package cache

import (
	"testing"

	"github.com/smartystreets/goconvey/convey"
)

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
}
