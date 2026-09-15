package git_svc

import (
	"context"
	"crypto/rand"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/smartystreets/goconvey/convey"

	"github.com/CodFrm/katch/internal/model/entity/event_entity"
	"github.com/CodFrm/katch/internal/model/entity/git_entity"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/service/setting_svc"
)

// 这一组验的是体积闸：上限管的是**收字节**这件事，不是收完之后量一量。
// 上游照旧是 builder_test.go 里那台进程内的假上游，不起子进程、不碰公网。

// boundsOrigin 假上游的回源地址，拼出来的远端就是 serveRepos 认的那一个。
const boundsOrigin = testProtocol + "://upstream"

// boundsRepo 被镜像的仓库路径。
const boundsRepo = "/source/.git"

// boundsNextRepo 排在超标仓库后面的那个小仓库：它建不建得起来，就是「被拒的
// 那一趟把并发名额还回来了没有」。
const boundsNextRepo = "/quick/.git"

// createBigSourceRepo 建一个带着 size 字节随机内容的上游仓库。
//
// 随机内容而不是重复串：pack 是压过的，一个能被压到几 KB 的大文件量不出
// 「下载了多少字节」这件事。
func createBigSourceRepo(t *testing.T, dir string, size int) {
	t.Helper()
	repo, err := git.PlainInit(dir, false)
	convey.So(err, convey.ShouldBeNil)
	worktree, err := repo.Worktree()
	convey.So(err, convey.ShouldBeNil)
	blob := make([]byte, size)
	_, err = rand.Read(blob)
	convey.So(err, convey.ShouldBeNil)
	convey.So(os.WriteFile(filepath.Join(dir, "big.bin"), blob, 0o600), convey.ShouldBeNil)
	_, err = worktree.Add("big.bin")
	convey.So(err, convey.ShouldBeNil)
	_, err = worktree.Commit("big", &git.CommitOptions{
		Author: &object.Signature{Name: "katch", Email: "katch@example.com", When: time.Now()},
	})
	convey.So(err, convey.ShouldBeNil)
}

// treeSize 一棵目录树此刻占了多少字节，走到一半被删掉的条目直接跳过。
func treeSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		// 观察者遇到正在被建或正在被删的条目就跳过：量不到不是失败。
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}

// peakWatcher 盯着一个目录，记下它在观察期间涨到过的最大字节数。
//
// 「峰值」只能在建镜像**跑着**的时候看：中止之后目录就被清了，事后去量
// 量到的永远是 0。
type peakWatcher struct {
	peak atomic.Int64
	stop chan struct{}
	done chan struct{}
}

func watchPeak(dir string) *peakWatcher {
	w := &peakWatcher{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		for {
			if size := treeSize(dir); size > w.peak.Load() {
				w.peak.Store(size)
			}
			select {
			case <-w.stop:
				return
			default:
			}
			time.Sleep(time.Millisecond)
		}
	}()
	return w
}

// stopAndPeak 收工，返回观察到的峰值。
func (w *peakWatcher) stopAndPeak() int64 {
	close(w.stop)
	<-w.done
	return w.peak.Load()
}

// TestEnsure_SizeGateStopsTheDownloadAtTheByte 体积闸在接收字节处拦人：
// 一个超标仓库不会先被完整下载下来，盘上峰值也就不会是上限的好几倍。
func TestEnsure_SizeGateStopsTheDownloadAtTheByte(t *testing.T) {
	convey.Convey("上游仓库远大于单仓上限时", t, func() {
		const maxBytes = 512 << 10
		base := t.TempDir()
		_ = serveRepos(t, base)
		createBigSourceRepo(t, filepath.Join(base, "source"), 3<<20)
		createSourceRepo(t, filepath.Join(base, "quick"))
		// 用例本身得先站得住：上游确实远比上限大，不然下面那条峰值断言没有牙。
		convey.So(treeSize(filepath.Join(base, "source")),
			convey.ShouldBeGreaterThan, 4*maxBytes)

		registerUpstreamWithOrigin(t, testHost, boundsOrigin, upstream_entity.ProtocolGit)
		repo := newFakeMirrorRepo(t)
		events := captureEvents(t)
		rt := newFakeRuntime(t, func(rt *setting_svc.RuntimeSettings) {
			rt.GitRepoMaxBytes = maxBytes
			// 一个名额：被拒的那一趟要是不还，后面那个仓库永远排不上队。
			rt.GitSyncConcurrency = 1
		})
		root := t.TempDir()
		svc := New(Options{Dir: root, Runtime: rt})
		ctx := context.Background()

		watcher := watchPeak(root)
		svc.Ensure(ctx, testHost, boundsRepo)
		convey.So(svc.Quiesce(ctx), convey.ShouldBeNil)
		peak := watcher.stopAndPeak()

		convey.Convey("下载途中就被拦住：盘上峰值不超过上限加一次 pack 写入缓冲", func() {
			// 容差给到 512 KiB：闸是按一次写入判的，越界的那一次写整块不落盘，
			// 真正的余量只有 go-git 的拷贝缓冲那么大。
			convey.So(peak, convey.ShouldBeLessThan, maxBytes+(512<<10))
		})

		convey.Convey("半个裸仓库不留在盘上", func() {
			mirror, err := mirrorDir(root, testHost, boundsRepo)
			convey.So(err, convey.ShouldBeNil)
			_, statErr := os.Stat(mirror)
			convey.So(os.IsNotExist(statErr), convey.ShouldBeTrue)
		})

		convey.Convey("记录转 rejected，last_error 指明是体积闸拦的", func() {
			row := repo.find(testHost, boundsRepo)
			convey.So(row, convey.ShouldNotBeNil)
			convey.So(row.State, convey.ShouldEqual, git_entity.MirrorRejected)
			convey.So(row.SizeBytes, convey.ShouldEqual, 0)
			convey.So(row.LastError, convey.ShouldContainSubstring, ErrRepoTooLarge.Error())
		})

		convey.Convey("名额立刻归还：下一个仓库建起来了", func() {
			// 排在被拒的那一趟**后面**登记，而不是和它同时：同时登记的话谁先
			// 抢到那唯一一个名额是不定的，这条断言也就证不了名额还回来了没有。
			//
			// 总时限收到 5 秒：名额要是没还回去，这个小仓库会卡在队列里，
			// 5 秒后以「总时限全花在排队上」收场——而不是把用例挂死到超时。
			rt.set(func(rt *setting_svc.RuntimeSettings) { rt.GitBuildTimeoutSeconds = 5 })
			svc.Ensure(ctx, testHost, boundsNextRepo)
			convey.So(svc.Quiesce(ctx), convey.ShouldBeNil)

			row := repo.find(testHost, boundsNextRepo)
			convey.So(row, convey.ShouldNotBeNil)
			convey.So(row.State, convey.ShouldEqual, git_entity.MirrorReady)
			convey.So(row.SizeBytes, convey.ShouldBeGreaterThan, 0)
		})

		convey.Convey("事件流上是 rejected", func() {
			convey.So(events.kinds(), convey.ShouldContain,
				event_entity.KindGitMirrorRejected)
			convey.So(events.kinds(), convey.ShouldNotContain,
				event_entity.KindGitMirrorFailed)
		})
	})
}

// refusedOn 被拦下的那些读里，有没有落在名字带 want 的文件上。
func refusedOn(names []string, want string) bool {
	for _, name := range names {
		if strings.Contains(name, want) {
			return true
		}
	}
	return false
}

// TestCloneBounded_ParseDiesOnItsNextRead 闸合上之后，与下载并发跑着的那个
// 解析 goroutine 要在下一次读上就地结束，而不是把已经落盘的那段解析完再说。
//
// 这是 Problem 4 的回归防线：go-git 的 PackWriter 在构造的时候就起了解析
// goroutine（dotgit/writers.go 的 go writer.buildIndex），它从 objects/pack
// 下那个临时 pack 文件读，整条路径都不看 ctx——一个靠 cancel 的闸对它无效。
func TestCloneBounded_ParseDiesOnItsNextRead(t *testing.T) {
	convey.Convey("写侧在越界那一刻停下时", t, func() {
		const maxBytes = 512 << 10
		base := t.TempDir()
		remote := serveRepos(t, base)
		createBigSourceRepo(t, filepath.Join(base, "source"), 3<<20)

		dir := filepath.Join(t.TempDir(), "mirror", "source.git")
		limit := newBounds(maxBytes, 0, 0)
		err := cloneBounded(context.Background(), dir, remote("source"), limit)

		convey.Convey("建镜像报的是体积闸那一个错，不是它在下游的症状", func() {
			convey.So(errors.Is(err, ErrRepoTooLarge), convey.ShouldBeTrue)
		})

		convey.Convey("解析 goroutine 是被读侧掐断的：临时 pack 上有读被拦下", func() {
			// 这里断的是「掐断解析用的是哪条通道」。只看建镜像返回了什么分不出
			// 两件事：解析被就地终止，还是它把已经落盘的那一段跑完之后才停。
			convey.So(refusedOn(limit.refusedReadNames(), "tmp_pack_"), convey.ShouldBeTrue)
		})
	})
}

// TestBoundedFS_GateSurvivesChroot 闸要跟着 Chroot 一路走下去，按偏移的读也拦。
//
// billy 的文件系统是可以就地 chroot 到子目录上的（dotgit 对 objects/pack 这类
// 子树就这么用）。Chroot 要是不再包一层，子文件系统上的写就整个绕过了体积闸——
// 而一次 clone 的字节几乎全都落在 objects/pack 底下，闸也就等于没有。ReadAt
// 同理：解析阶段按偏移取对象走的是它，不拦的话闸合上之后解析照旧跑到底。
func TestBoundedFS_GateSurvivesChroot(t *testing.T) {
	convey.Convey("在 chroot 出来的子文件系统上", t, func() {
		limit := newBounds(16, 0, 0)
		fs := newBoundedFS(osfs.New(t.TempDir()), limit)
		sub, err := fs.Chroot("objects")
		convey.So(err, convey.ShouldBeNil)
		file, err := sub.Create("pack")
		convey.So(err, convey.ShouldBeNil)
		defer func() { _ = file.Close() }()

		convey.Convey("写照样过闸：上限之内落盘，越界的那一次报体积闸", func() {
			n, err := file.Write(make([]byte, 16))
			convey.So(err, convey.ShouldBeNil)
			convey.So(n, convey.ShouldEqual, 16)

			_, err = file.Write([]byte("x"))
			convey.So(errors.Is(err, ErrRepoTooLarge), convey.ShouldBeTrue)
		})

		convey.Convey("闸合上之后按偏移读也报错，报的是合闸的那个原因", func() {
			limit.trip(ErrBuildTimedOut)
			_, err := file.ReadAt(make([]byte, 1), 0)
			convey.So(errors.Is(err, ErrBuildTimedOut), convey.ShouldBeTrue)
		})
	})
}
