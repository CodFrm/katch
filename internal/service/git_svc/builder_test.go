package git_svc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/go-git/go-git/v5/plumbing/transport/server"
	"github.com/smartystreets/goconvey/convey"
)

// 这一组用例跑的是真的 go-git：状态机可以拿假 builder 验，但「盘上那份东西
// 到底是不是一个裸仓库」只能由 go-git 自己回答。
//
// 上游仓库用 go-git 在进程内建、在进程内服务（transport/server），不起子进程：
// go-git 的 file 传输会去调 git-upload-pack 这个**外部二进制**，而本仓的产物
// 承诺不依赖它，用例自然也不该依赖。

// testProtocol 用例自己的传输协议名，指向进程内那台假上游。
const testProtocol = "katchtest"

// serveRepos 把一个目录挂成进程内的 git 上游，返回拼远端地址的函数。
func serveRepos(t *testing.T, base string) func(name string) string {
	t.Helper()
	client.InstallProtocol(testProtocol, server.NewClient(server.NewFilesystemLoader(osfs.New(base))))
	t.Cleanup(func() { client.InstallProtocol(testProtocol, nil) })
	// 地址里指到 .git 那一层：go-git 的文件系统 loader 认的是仓库目录本身，
	// 给它一个带工作区的仓库根目录，它会在那里找不到任何对象，报「仓库是空的」。
	return func(name string) string {
		return testProtocol + "://upstream/" + name + "/.git"
	}
}

// createSourceRepo 建一个有一次提交的上游仓库，返回它的提交哈希。
func createSourceRepo(t *testing.T, dir string) string {
	t.Helper()
	repo, err := git.PlainInit(dir, false)
	convey.So(err, convey.ShouldBeNil)
	worktree, err := repo.Worktree()
	convey.So(err, convey.ShouldBeNil)
	convey.So(os.WriteFile(filepath.Join(dir, "README.md"), []byte("katch"), 0o600),
		convey.ShouldBeNil)
	_, err = worktree.Add("README.md")
	convey.So(err, convey.ShouldBeNil)
	hash, err := worktree.Commit("first", &git.CommitOptions{
		Author: &object.Signature{Name: "katch", Email: "katch@example.com", When: time.Now()},
	})
	convey.So(err, convey.ShouldBeNil)
	return hash.String()
}

// TestGitBuilder_BuildsBareRepository 建出来的要是一个裸仓库，提交和上游一致。
func TestGitBuilder_BuildsBareRepository(t *testing.T) {
	convey.Convey("go-git 把一个上游仓库建成本地镜像", t, func() {
		base := t.TempDir()
		remote := serveRepos(t, base)
		want := createSourceRepo(t, filepath.Join(base, "source"))

		dir := filepath.Join(t.TempDir(), "mirror", "source.git")
		size, err := NewGitBuilder().Build(context.Background(), &BuildRequest{
			Dir: dir, Remote: remote("source"), MaxBytes: 1 << 30,
		})
		convey.So(err, convey.ShouldBeNil)
		convey.So(size, convey.ShouldBeGreaterThan, 0)

		convey.Convey("盘上是一个裸仓库，不是一个带工作区的 clone", func() {
			mirror, err := git.PlainOpen(dir)
			convey.So(err, convey.ShouldBeNil)
			_, err = mirror.Worktree()
			convey.So(errors.Is(err, git.ErrIsBareRepository), convey.ShouldBeTrue)
			// 裸仓库没有 .git 子目录，对象直接躺在目录里。
			_, statErr := os.Stat(filepath.Join(dir, ".git"))
			convey.So(os.IsNotExist(statErr), convey.ShouldBeTrue)
		})

		convey.Convey("提交和上游那一份一致", func() {
			mirror, err := git.PlainOpen(dir)
			convey.So(err, convey.ShouldBeNil)
			head, err := mirror.Reference("refs/heads/master", true)
			convey.So(err, convey.ShouldBeNil)
			convey.So(head.Hash().String(), convey.ShouldEqual, want)
		})
	})
}

// TestGitBuilder_TooLarge 超过单仓上限时报 ErrRepoTooLarge，且盘上不留东西。
func TestGitBuilder_TooLarge(t *testing.T) {
	convey.Convey("仓库大过单仓上限时", t, func() {
		base := t.TempDir()
		remote := serveRepos(t, base)
		createSourceRepo(t, filepath.Join(base, "source"))

		dir := filepath.Join(t.TempDir(), "mirror", "source.git")
		size, err := NewGitBuilder().Build(context.Background(), &BuildRequest{
			Dir: dir, Remote: remote("source"), MaxBytes: 1,
		})
		convey.So(errors.Is(err, ErrRepoTooLarge), convey.ShouldBeTrue)
		convey.So(size, convey.ShouldEqual, 0)

		convey.Convey("半份镜像不留在盘上：它既占配额，又会挡住下一次重试", func() {
			_, statErr := os.Stat(dir)
			convey.So(os.IsNotExist(statErr), convey.ShouldBeTrue)
		})
	})
}

// TestGitBuilder_UnreachableRemote 上游不存在时是一次普通失败，不是拒绝。
func TestGitBuilder_UnreachableRemote(t *testing.T) {
	convey.Convey("上游仓库不存在时", t, func() {
		base := t.TempDir()
		remote := serveRepos(t, base)

		dir := filepath.Join(t.TempDir(), "mirror", "missing.git")
		_, err := NewGitBuilder().Build(context.Background(), &BuildRequest{
			Dir: dir, Remote: remote("missing"), MaxBytes: 1 << 30,
		})
		convey.So(err, convey.ShouldNotBeNil)
		convey.So(errors.Is(err, ErrRepoTooLarge), convey.ShouldBeFalse)
		_, statErr := os.Stat(dir)
		convey.So(os.IsNotExist(statErr), convey.ShouldBeTrue)
	})
}

// TestGitBuilder_RetriesOverLeftovers 上一趟留下的半个仓库不会挡住重来一次。
func TestGitBuilder_RetriesOverLeftovers(t *testing.T) {
	convey.Convey("目录里还躺着上一趟的残留时", t, func() {
		base := t.TempDir()
		remote := serveRepos(t, base)
		createSourceRepo(t, filepath.Join(base, "source"))

		dir := filepath.Join(t.TempDir(), "mirror", "source.git")
		convey.So(os.MkdirAll(dir, 0o750), convey.ShouldBeNil)
		convey.So(os.WriteFile(filepath.Join(dir, "half-written"), []byte("x"), 0o600),
			convey.ShouldBeNil)

		size, err := NewGitBuilder().Build(context.Background(), &BuildRequest{
			Dir: dir, Remote: remote("source"), MaxBytes: 1 << 30,
		})
		convey.So(err, convey.ShouldBeNil)
		convey.So(size, convey.ShouldBeGreaterThan, 0)
		_, statErr := os.Stat(filepath.Join(dir, "half-written"))
		convey.So(os.IsNotExist(statErr), convey.ShouldBeTrue)
	})
}

// commitAgain 往上游仓库里再压一条提交，返回它的哈希。
func commitAgain(t *testing.T, dir, name string) string {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	convey.So(err, convey.ShouldBeNil)
	worktree, err := repo.Worktree()
	convey.So(err, convey.ShouldBeNil)
	convey.So(os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600), convey.ShouldBeNil)
	_, err = worktree.Add(name)
	convey.So(err, convey.ShouldBeNil)
	hash, err := worktree.Commit("second", &git.CommitOptions{
		Author: &object.Signature{Name: "katch", Email: "katch@example.com", When: time.Now()},
	})
	convey.So(err, convey.ShouldBeNil)
	return hash.String()
}

// TestGitBuilder_SyncFetchesNewCommits 目标 (c)：增量同步把上游的新提交拉进来，
// 而不是把整个仓库重新 clone 一遍。
func TestGitBuilder_SyncFetchesNewCommits(t *testing.T) {
	convey.Convey("已经建好的镜像做一次增量同步", t, func() {
		base := t.TempDir()
		remote := serveRepos(t, base)
		source := filepath.Join(base, "source")
		first := createSourceRepo(t, source)

		dir := filepath.Join(t.TempDir(), "mirror", "source.git")
		_, err := NewGitBuilder().Build(context.Background(), &BuildRequest{
			Dir: dir, Remote: remote("source"), MaxBytes: 1 << 30,
		})
		convey.So(err, convey.ShouldBeNil)
		second := commitAgain(t, source, "NEWS.md")
		convey.So(second, convey.ShouldNotEqual, first)

		size, err := NewGitBuilder().Sync(context.Background(), &SyncRequest{
			Dir: dir, Remote: remote("source"),
		})
		convey.So(err, convey.ShouldBeNil)
		convey.So(size, convey.ShouldBeGreaterThan, 0)

		convey.Convey("镜像上的分支指到了上游那条新提交", func() {
			mirror, err := git.PlainOpen(dir)
			convey.So(err, convey.ShouldBeNil)
			head, err := mirror.Reference("refs/heads/master", true)
			convey.So(err, convey.ShouldBeNil)
			convey.So(head.Hash().String(), convey.ShouldEqual, second)
		})

		convey.Convey("上游没有新东西时不是错误：这是最常见的那一次同步", func() {
			again, err := NewGitBuilder().Sync(context.Background(), &SyncRequest{
				Dir: dir, Remote: remote("source"),
			})
			convey.So(err, convey.ShouldBeNil)
			convey.So(again, convey.ShouldBeGreaterThan, 0)
		})
	})
}

// TestGitBuilder_SyncDropsRefsTheUpstreamDeleted 上游删掉的分支，同步之后镜像上
// 也要没有——镜像广播出去的是「上游此刻的样子」，留着一条上游已经不存在的分支，
// 客户端会一直看到它，甚至 clone 出一条早就被删掉的历史。
func TestGitBuilder_SyncDropsRefsTheUpstreamDeleted(t *testing.T) {
	convey.Convey("上游删掉一条分支之后同步", t, func() {
		base := t.TempDir()
		remote := serveRepos(t, base)
		source := filepath.Join(base, "source")
		createSourceRepo(t, source)
		sourceRepo, err := git.PlainOpen(source)
		convey.So(err, convey.ShouldBeNil)
		head, err := sourceRepo.Reference("refs/heads/master", true)
		convey.So(err, convey.ShouldBeNil)
		doomed := plumbing.NewHashReference("refs/heads/doomed", head.Hash())
		convey.So(sourceRepo.Storer.SetReference(doomed), convey.ShouldBeNil)

		dir := filepath.Join(t.TempDir(), "mirror", "source.git")
		_, err = NewGitBuilder().Build(context.Background(), &BuildRequest{
			Dir: dir, Remote: remote("source"), MaxBytes: 1 << 30,
		})
		convey.So(err, convey.ShouldBeNil)
		mirror, err := git.PlainOpen(dir)
		convey.So(err, convey.ShouldBeNil)
		_, err = mirror.Reference("refs/heads/doomed", false)
		convey.So(err, convey.ShouldBeNil)

		convey.So(sourceRepo.Storer.RemoveReference("refs/heads/doomed"), convey.ShouldBeNil)
		_, err = NewGitBuilder().Sync(context.Background(), &SyncRequest{
			Dir: dir, Remote: remote("source"),
		})
		convey.So(err, convey.ShouldBeNil)

		_, err = mirror.Reference("refs/heads/doomed", false)
		convey.So(errors.Is(err, plumbing.ErrReferenceNotFound), convey.ShouldBeTrue)
		// 剩下的那条还在：删的是上游删掉的那一条，不是所有的。
		_, err = mirror.Reference("refs/heads/master", false)
		convey.So(err, convey.ShouldBeNil)
	})
}

// TestGitBuilder_SyncWithoutMirrorFails 盘上没有那份镜像时同步是一次失败，
// 调用方据此把这一次降级为穿透，而不是在一个空目录上建出半个仓库。
func TestGitBuilder_SyncWithoutMirrorFails(t *testing.T) {
	convey.Convey("镜像目录不在时同步报错", t, func() {
		base := t.TempDir()
		remote := serveRepos(t, base)
		createSourceRepo(t, filepath.Join(base, "source"))

		_, err := NewGitBuilder().Sync(context.Background(), &SyncRequest{
			Dir: filepath.Join(t.TempDir(), "gone.git"), Remote: remote("source"),
		})
		convey.So(err, convey.ShouldNotBeNil)
	})
}

// TestMirrorDir 镜像目录的排布，以及它绝不跑出根目录。
func TestMirrorDir(t *testing.T) {
	convey.Convey("镜像目录", t, func() {
		root := filepath.Join("/tmp", "katch-mirrors")

		convey.Convey("照着主机名与仓库路径排，末尾缀 .git", func() {
			dir, err := mirrorDir(root, "github.com", "/foo/bar")
			convey.So(err, convey.ShouldBeNil)
			convey.So(dir, convey.ShouldEqual, filepath.Join(root, "github.com", "foo", "bar.git"))
		})

		convey.Convey("仓库路径自带 .git 时不归一，两条路径是两份镜像", func() {
			// 归一成同一个目录，就会有两趟建镜像同时往一处写。
			withSuffix, err := mirrorDir(root, "github.com", "/foo/bar.git")
			convey.So(err, convey.ShouldBeNil)
			without, err := mirrorDir(root, "github.com", "/foo/bar")
			convey.So(err, convey.ShouldBeNil)
			convey.So(withSuffix, convey.ShouldNotEqual, without)
		})

		convey.Convey("仓库落在基路径上时挂在主机目录旁边，不会和它自己打架", func() {
			dir, err := mirrorDir(root, "git.example.com", "/")
			convey.So(err, convey.ShouldBeNil)
			convey.So(dir, convey.ShouldEqual, filepath.Join(root, "git.example.com.git"))
		})

		convey.Convey("带回溯段、分隔符或空字节的路径一律拒绝", func() {
			for _, repo := range []string{
				"/../../etc/passwd", "/foo/../../..", "/foo/./../..", "/a\x00b", "/..",
			} {
				_, err := mirrorDir(root, "github.com", repo)
				convey.So(err, convey.ShouldNotBeNil)
			}
			for _, host := range []string{"..", "a/b", "", "a\\b"} {
				_, err := mirrorDir(root, host, "/foo")
				convey.So(err, convey.ShouldNotBeNil)
			}
		})

		convey.Convey("没配根目录时拒绝，而不是往当前目录里写", func() {
			_, err := mirrorDir("", "github.com", "/foo")
			convey.So(err, convey.ShouldNotBeNil)
		})
	})
}

// TestRemoteURL 远端地址是上游的回源地址加仓库路径，转义形态原样保留。
func TestRemoteURL(t *testing.T) {
	convey.Convey("拼远端地址", t, func() {
		convey.So(remoteURL("https://github.com", "/foo/bar.git"),
			convey.ShouldEqual, "https://github.com/foo/bar.git")
		convey.So(remoteURL("https://github.com/", "/foo/bar.git"),
			convey.ShouldEqual, "https://github.com/foo/bar.git")
		convey.So(remoteURL("https://git.example.com/team", "/"),
			convey.ShouldEqual, "https://git.example.com/team")
		// 转义形态原样拼上去：解一次再拼回去，%2F 会变成一个真的分隔符。
		convey.So(remoteURL("https://github.com", "/foo%2Fbar"),
			convey.ShouldEqual, "https://github.com/foo%2Fbar")
	})
}
