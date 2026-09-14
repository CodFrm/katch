package cache_svc

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/smartystreets/goconvey/convey"
)

// TestQuiesce_WaitsForBackgroundCacheWrite 回源之后台协程必须是可等待的。
//
// 一次未命中的拉取把字节搬进缓存的活儿是交给后台协程干的（决策 9：客户端断开
// 也要把这趟下载跑完）。客户端收完响应体**不等于**那件活干完了：提交完文件、
// 写完记录之后，协程还要按配额回收一次。
//
// 没有一个等得到它的入口，调用方就只能猜。进程退出时这批活会被当场抛下——
// 一个刚提交的文件可能还没有对应的记录；而任何一个「先装配、再拆掉」的调用方
// （用例是最容易撞上的那一种）会在上一趟活还没干完的时候就把它脚下的库、
// 缓存目录和全局日志换掉，于是 -race 会在一个跟它毫无关系的地方报出竞争。
func TestQuiesce_WaitsForBackgroundCacheWrite(t *testing.T) {
	convey.Convey("后台写缓存没干完之前，Quiesce 不返回", t, func() {
		body := "一个要被搬进缓存的对象\n"
		origin := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, body)
		})
		svc, repo, _ := setupSvc(t, origin, staticUpstream("files.example.test"), Options{
			Runtime: newFakeRuntime(t, nil),
		})
		// 按住 TotalSize，后台协程就停在「提交完文件、正要回收配额」那一步。
		gate := make(chan struct{})
		repo.mu.Lock()
		repo.totalSizeGate = gate
		repo.mu.Unlock()

		ctx := context.Background()
		reader, _, err := svc.Get(ctx, target("files.example.test", "/pool/main/g/guard.deb"))
		convey.So(err, convey.ShouldBeNil)
		got, err := io.ReadAll(reader)
		convey.So(err, convey.ShouldBeNil)
		convey.So(string(got), convey.ShouldEqual, body)
		convey.So(reader.Close(), convey.ShouldBeNil)
		// 到这里客户端已经收完整个响应体，可后台那趟活还停在闸上。

		returned := make(chan error, 1)
		go func() { returned <- svc.Quiesce(ctx) }()

		select {
		case <-returned:
			t.Fatal("后台协程还停在配额回收上，Quiesce 就先返回了——它根本没在等")
		case <-time.After(100 * time.Millisecond):
			// 正是要的：它在等。
		}

		close(gate)
		select {
		case err := <-returned:
			convey.So(err, convey.ShouldBeNil)
		case <-time.After(10 * time.Second):
			t.Fatal("闸放开之后 Quiesce 仍然没返回")
		}

		// 等到了就意味着记录已经落表：调用方这时才可以安全地拆掉库与缓存目录。
		convey.So(repo.byKey("/pool/main/g/guard.deb"), convey.ShouldNotBeNil)
	})
}

// TestQuiesce_ReturnsWhenContextEnds 调用方不愿意无限等下去时要能抽身。
//
// 关停有自己的时限：一个卡在上游那边的大对象不该把整个进程的退出拖住。
func TestQuiesce_ReturnsWhenContextEnds(t *testing.T) {
	convey.Convey("ctx 结束时 Quiesce 带着错误返回，不把调用方永远挂住", t, func() {
		origin := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "对象\n")
		})
		svc, repo, _ := setupSvc(t, origin, staticUpstream("files.example.test"), Options{
			Runtime: newFakeRuntime(t, nil),
		})
		gate := make(chan struct{})
		repo.mu.Lock()
		repo.totalSizeGate = gate
		repo.mu.Unlock()
		defer close(gate)

		reader, _, err := svc.Get(context.Background(),
			target("files.example.test", "/pool/main/g/guard.deb"))
		convey.So(err, convey.ShouldBeNil)
		_, _ = io.ReadAll(reader)
		convey.So(reader.Close(), convey.ShouldBeNil)

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		convey.So(svc.Quiesce(ctx), convey.ShouldNotBeNil)
	})
}
