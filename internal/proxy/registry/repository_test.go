package registry

import (
	"testing"

	"github.com/smartystreets/goconvey/convey"
)

// TestSplitRepository 导出的切分规则与回源用的是同一份：管理界面的镜像视图按它
// 断句，两边一旦分叉，同一个键在界面上归到的仓库就不是回源时拉的那个。
func TestSplitRepository(t *testing.T) {
	cases := []struct {
		path, repository, tail string
	}{
		{"/redis/manifests/7", "redis", "/manifests/7"},
		{"/ns/sub/repo/blobs/sha256:abc", "ns/sub/repo", "/blobs/sha256:abc"},
		{"/blobs/manifests/7", "blobs", "/manifests/7"},
		{"/library/manifests/blobs/sha256:abc", "library/manifests", "/blobs/sha256:abc"},
		{"/redis/tags/list", "redis", "/tags/list"},
		{"/redis/referrers/sha256:abc", "redis", "/referrers/sha256:abc"},
		{"/_catalog", "", ""},
		{"/manifests/7", "", ""},
		{"/", "", ""},
	}
	convey.Convey("按最后一个动词段切出仓库名", t, func() {
		for _, c := range cases {
			repository, tail := SplitRepository(c.path)
			convey.So(repository, convey.ShouldEqual, c.repository)
			convey.So(tail, convey.ShouldEqual, c.tail)
		}
	})
}

// TestCompleteLibrary library/ 补全只看上游记录上的开关，只补不含 / 的仓库名。
func TestCompleteLibrary(t *testing.T) {
	convey.Convey("library/ 补全", t, func() {
		convey.So(CompleteLibrary("redis", true), convey.ShouldEqual, "library/redis")
		convey.So(CompleteLibrary("redis", false), convey.ShouldEqual, "redis")
		convey.So(CompleteLibrary("library/redis", true), convey.ShouldEqual, "library/redis")
		convey.So(CompleteLibrary("bitnami/redis", true), convey.ShouldEqual, "bitnami/redis")
	})
}
