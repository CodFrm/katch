package dispatch

import (
	"testing"

	"github.com/smartystreets/goconvey/convey"
)

// TestClassifyGit 穷举 git 端点的识别。
//
// 判错的两个方向都有代价：把普通静态路径认成 git 端点，会让一个 release 资产
// 的下载被当成协商请求；把 git 端点漏认成静态路径，clone 就会在第一步拿到一份
// 被缓存的 ref 广播。识别只看路径后缀与查询串里的 service，不看方法——方法闸
// 是拉取路径那一层的事，这里只回答「这个 URL 是不是 git 的那两个端点」。
func TestClassifyGit(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		rawQuery string
		want     GitEndpoint
	}{
		// ── upload-pack：只读的那一半 ──
		{"ref 广播", "/CodFrm/katch/info/refs", "service=git-upload-pack",
			GitEndpoint{Service: GitUploadPack, Advertise: true, Repo: "/CodFrm/katch"}},
		{"协商", "/CodFrm/katch/git-upload-pack", "",
			GitEndpoint{Service: GitUploadPack, Repo: "/CodFrm/katch"}},
		{"仓库路径带 .git 后缀", "/CodFrm/katch.git/info/refs", "service=git-upload-pack",
			GitEndpoint{Service: GitUploadPack, Advertise: true, Repo: "/CodFrm/katch.git"}},
		{"仓库路径里含点", "/a.b/c.d/git-upload-pack", "",
			GitEndpoint{Service: GitUploadPack, Repo: "/a.b/c.d"}},
		{"单层仓库路径", "/katch.git/git-upload-pack", "",
			GitEndpoint{Service: GitUploadPack, Repo: "/katch.git"}},
		{"仓库落在回源地址的基路径上（上游侧路径只剩端点）", "/info/refs", "service=git-upload-pack",
			GitEndpoint{Service: GitUploadPack, Advertise: true, Repo: "/"}},
		{"service 之外还带别的查询项", "/a/b/info/refs", "service=git-upload-pack&x=1",
			GitEndpoint{Service: GitUploadPack, Advertise: true, Repo: "/a/b"}},

		// ── receive-pack：写的那一半，同样要认出来才拒绝得了 ──
		{"push 的 ref 广播", "/CodFrm/katch/info/refs", "service=git-receive-pack",
			GitEndpoint{Service: GitReceivePack, Advertise: true, Repo: "/CodFrm/katch"}},
		{"push 的协商", "/CodFrm/katch/git-receive-pack", "",
			GitEndpoint{Service: GitReceivePack, Repo: "/CodFrm/katch"}},

		// ── 形似而非 git 端点的普通路径 ──
		{"不带 service 的 info/refs 是哑协议，按静态处理", "/a/b/info/refs", "",
			GitEndpoint{}},
		{"service 取值不认得", "/a/b/info/refs", "service=git-evil-pack", GitEndpoint{}},
		{"service 大小写不符", "/a/b/info/refs", "service=GIT-UPLOAD-PACK", GitEndpoint{}},
		{"查询串本身坏掉", "/a/b/info/refs", "service=git-upload-pack;%zz", GitEndpoint{}},
		{"info/refs 在路径中间不算端点", "/a/info/refs/b", "service=git-upload-pack",
			GitEndpoint{}},
		{"objects/info/packs 是哑协议的文件", "/a/b/objects/info/packs", "", GitEndpoint{}},
		{"名字里带 git-upload-pack 的普通文件", "/a/b/git-upload-pack.txt", "", GitEndpoint{}},
		{"路径后缀相同但少一层分隔", "/a/bgit-upload-pack", "", GitEndpoint{}},
		{"普通静态资源", "/CodFrm/katch/releases/download/v1/katch.tar.gz", "", GitEndpoint{}},
		{"根路径", "/", "", GitEndpoint{}},
		{"端点自己就是整条路径时没有仓库", "/git-upload-pack", "",
			GitEndpoint{Service: GitUploadPack, Repo: "/"}},
	}

	convey.Convey("按路径后缀与 service 识别 git 端点", t, func() {
		for _, c := range cases {
			convey.Convey(c.name+"："+c.path+"?"+c.rawQuery, func() {
				got := ClassifyGit(c.path, c.rawQuery)
				convey.So(got, convey.ShouldResemble, c.want)
			})
		}
	})
}

// TestGitEndpoint_ZeroValueIsNotGit 零值即「不是 git 请求」：Target 上带的就是这个
// 结构，零值必须表示「按原来的静态路径处理」，否则每个构造 Target 的地方都要记得
// 显式填一个「不是 git」。
func TestGitEndpoint_ZeroValueIsNotGit(t *testing.T) {
	convey.Convey("零值不是 git 请求", t, func() {
		convey.So(GitEndpoint{}.IsGit(), convey.ShouldBeFalse)
		convey.So(GitEndpoint{}.IsUploadPack(), convey.ShouldBeFalse)
		convey.So(ClassifyGit("/a/b/git-upload-pack", "").IsGit(), convey.ShouldBeTrue)
		convey.So(ClassifyGit("/a/b/git-upload-pack", "").IsUploadPack(), convey.ShouldBeTrue)
		convey.So(ClassifyGit("/a/b/git-receive-pack", "").IsGit(), convey.ShouldBeTrue)
		// 只读那一半的判定单独立着：拒绝 push 靠的就是它，混进 IsGit 里
		// 会让「是 git」和「可以服务」变成同一句话。
		convey.So(ClassifyGit("/a/b/git-receive-pack", "").IsUploadPack(), convey.ShouldBeFalse)
	})
}
