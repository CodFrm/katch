package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/smartystreets/goconvey/convey"
)

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestStore_WriteThenRead(t *testing.T) {
	convey.Convey("提交后的对象能按内容摘要读回来", t, func() {
		store, err := NewStore(t.TempDir())
		convey.So(err, convey.ShouldBeNil)

		w, err := store.Create()
		convey.So(err, convey.ShouldBeNil)
		_, err = w.Write([]byte("Origin: Debian\n"))
		convey.So(err, convey.ShouldBeNil)
		digest, size, err := w.Commit()
		convey.So(err, convey.ShouldBeNil)
		convey.So(digest, convey.ShouldEqual, digestOf([]byte("Origin: Debian\n")))
		convey.So(size, convey.ShouldEqual, 15)
		convey.So(w.Close(), convey.ShouldBeNil)

		f, got, err := store.Open(digest)
		convey.So(err, convey.ShouldBeNil)
		defer func() { _ = f.Close() }()
		convey.So(got, convey.ShouldEqual, 15)
		b, err := io.ReadAll(f)
		convey.So(err, convey.ShouldBeNil)
		convey.So(string(b), convey.ShouldEqual, "Origin: Debian\n")
	})
}

// TestStore_SameContentStoredOnce 决策 7 的「同一份内容只存一份」：
// 两个上游路径指向同一份字节时，磁盘上不该出现两个副本。
func TestStore_SameContentStoredOnce(t *testing.T) {
	convey.Convey("相同内容写两次只落一个文件", t, func() {
		root := t.TempDir()
		store, err := NewStore(root)
		convey.So(err, convey.ShouldBeNil)

		for range 2 {
			w, err := store.Create()
			convey.So(err, convey.ShouldBeNil)
			_, err = w.Write([]byte("same bytes"))
			convey.So(err, convey.ShouldBeNil)
			_, _, err = w.Commit()
			convey.So(err, convey.ShouldBeNil)
			convey.So(w.Close(), convey.ShouldBeNil)
		}

		convey.So(countBlobs(t, root), convey.ShouldEqual, 1)
		// 临时文件不能留在盘上：留一个就等于每次未命中都漏一份磁盘。
		convey.So(countTemps(t, root), convey.ShouldEqual, 0)
	})
}

func TestStore_UncommittedWriterLeavesNothing(t *testing.T) {
	convey.Convey("写到一半主动放弃时不留文件", t, func() {
		root := t.TempDir()
		store, err := NewStore(root)
		convey.So(err, convey.ShouldBeNil)

		w, err := store.Create()
		convey.So(err, convey.ShouldBeNil)
		_, err = w.Write([]byte("half"))
		convey.So(err, convey.ShouldBeNil)
		convey.So(w.Close(), convey.ShouldBeNil)

		convey.So(countBlobs(t, root), convey.ShouldEqual, 0)
		convey.So(countTemps(t, root), convey.ShouldEqual, 0)
	})
}

func TestStore_RemoveAndHas(t *testing.T) {
	convey.Convey("删除后既查不到也打不开", t, func() {
		store, err := NewStore(t.TempDir())
		convey.So(err, convey.ShouldBeNil)
		w, err := store.Create()
		convey.So(err, convey.ShouldBeNil)
		_, err = w.Write([]byte("x"))
		convey.So(err, convey.ShouldBeNil)
		digest, _, err := w.Commit()
		convey.So(err, convey.ShouldBeNil)
		convey.So(w.Close(), convey.ShouldBeNil)

		size, ok := store.Has(digest)
		convey.So(ok, convey.ShouldBeTrue)
		convey.So(size, convey.ShouldEqual, 1)

		convey.So(store.Remove(digest), convey.ShouldBeNil)
		_, ok = store.Has(digest)
		convey.So(ok, convey.ShouldBeFalse)
		_, _, err = store.Open(digest)
		convey.So(err, convey.ShouldNotBeNil)
		// 重复删除不算错：淘汰与清理可能同时盯上同一份内容。
		convey.So(store.Remove(digest), convey.ShouldBeNil)
	})
}

// TestStore_RootMustBeWritable 缓存目录不可写时构造就失败，由调用方降级为纯透传。
// 这道判定必须在启动时给出结论，而不是等到第一次拉取才发现写不进去。
func TestStore_RootMustBeWritable(t *testing.T) {
	convey.Convey("缓存根目录建不出来时构造失败", t, func() {
		// 拿一个普通文件当根目录的父级：在它下面 mkdir 一定失败，
		// 且不依赖进程的 uid（以 root 跑时 0500 的目录照样能写）。
		file := filepath.Join(t.TempDir(), "not-a-dir")
		convey.So(os.WriteFile(file, []byte("x"), 0o600), convey.ShouldBeNil)

		store, err := NewStore(filepath.Join(file, "cache"))
		convey.So(err, convey.ShouldNotBeNil)
		convey.So(store, convey.ShouldBeNil)
	})
}

// TestStore_KilledMidWriteLeavesNoUsableBlob 决策 8：进程在下载中途被杀，
// 不能留下一个会被后续请求当成完整缓存的半截文件。
//
// 这里真的把一个子进程 SIGKILL 掉——不是设一个「假装崩溃」的开关：要验的正是
// 「没有任何 defer、没有任何清理代码能跑」这种情况下磁盘上剩下什么。
func TestStore_KilledMidWriteLeavesNoUsableBlob(t *testing.T) {
	if root := os.Getenv("KATCH_CACHE_KILL_ROOT"); root != "" {
		killHelper(root)
		return
	}

	convey.Convey("写到一半被 SIGKILL，剩下的东西不会被当成缓存", t, func() {
		root := t.TempDir()
		cmd := exec.Command(os.Args[0], "-test.run=TestStore_KilledMidWriteLeavesNoUsableBlob")
		cmd.Env = append(os.Environ(), "KATCH_CACHE_KILL_ROOT="+root)
		err := cmd.Run()

		// 子进程确实是被信号杀死的，而不是自己正常退出。
		var exitErr *exec.ExitError
		convey.So(errors.As(err, &exitErr), convey.ShouldBeTrue)
		status, ok := exitErr.Sys().(syscall.WaitStatus)
		convey.So(ok, convey.ShouldBeTrue)
		convey.So(status.Signaled(), convey.ShouldBeTrue)

		store, err := NewStore(root)
		convey.So(err, convey.ShouldBeNil)
		// 半截内容的摘要、以及它本应有的完整内容的摘要，都不该命中。
		_, ok = store.Has(digestOf([]byte(strings.Repeat("a", 1024))))
		convey.So(ok, convey.ShouldBeFalse)
		_, ok = store.Has(digestOf(append([]byte(strings.Repeat("a", 1024)), []byte(strings.Repeat("b", 1024))...)))
		convey.So(ok, convey.ShouldBeFalse)
		// 更强的一条：内容寻址目录里根本没有任何文件，谁也没法把半截当整份。
		convey.So(countBlobs(t, root), convey.ShouldEqual, 0)
	})
}

// killHelper 在子进程里跑：写一半、不提交、直接被信号带走。
func killHelper(root string) {
	store, err := NewStore(root)
	if err != nil {
		os.Exit(2)
	}
	w, err := store.Create()
	if err != nil {
		os.Exit(3)
	}
	if _, err := w.Write([]byte(strings.Repeat("a", 1024))); err != nil {
		os.Exit(4)
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	// SIGKILL 不会被捕获，走不到这里；万一走到了也要让父进程看出不对。
	os.Exit(5)
}

func countBlobs(t *testing.T, root string) int {
	t.Helper()
	return countFiles(t, filepath.Join(root, "blobs"))
}

func countTemps(t *testing.T, root string) int {
	t.Helper()
	return countFiles(t, filepath.Join(root, "tmp"))
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("遍历 %s 失败：%v", dir, err)
	}
	return n
}
