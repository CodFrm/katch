package cache_svc

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/smartystreets/goconvey/convey"

	"github.com/CodFrm/katch/internal/service/setting_svc"
)

// 「可变对象由 TTL 自行过期」不能只是「读到它时发现它过期了」：一个再也没人来取的
// 过期对象，记录和盘上的字节会一直留着，还一直算进配额，于是「缓存总容量有上限」
// 这条会被一批死对象慢慢顶穿——而它们又进不了 LRU 的候选（那条只挑不可变的）。
// 所以要有一趟清理真的把它们收走。

// TestSweep_RemovesExpiredMutableObjects 过期的可变对象要被清理掉，连同盘上的字节。
func TestSweep_RemovesExpiredMutableObjects(t *testing.T) {
	convey.Convey("清理一趟之后，过期的可变对象不再占地方", t, func() {
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "InRelease")
		})
		// TTL 一秒，配额给足：这一组验的是过期，不是配额。
		svc, repo, store := setupSvc(t, o, staticUpstream("deb.debian.org"),
			Options{Runtime: newFakeRuntime(t, func(rt *setting_svc.RuntimeSettings) {
				rt.MutableTTLSeconds = 1
				rt.CacheQuotaBytes = 1 << 30
			})})

		// 走一次真实的拉取，让它按生产路径写成一条可变记录。
		r, _, err := svc.Get(context.Background(), target("deb.debian.org", "/dists/stable/InRelease"))
		convey.So(err, convey.ShouldBeNil)
		_, _ = io.ReadAll(r)
		convey.So(r.Close(), convey.ShouldBeNil)

		object := repo.byKey("/dists/stable/InRelease")
		convey.So(object, convey.ShouldNotBeNil)
		convey.So(object.Immutable, convey.ShouldBeFalse)
		convey.So(object.ExpiresAt, convey.ShouldBeGreaterThan, 0)

		convey.Convey("还没过期时清理不动它", func() {
			removed, err := svc.Sweep(context.Background())
			convey.So(err, convey.ShouldBeNil)
			convey.So(removed, convey.ShouldEqual, 0)
			convey.So(repo.byKey("/dists/stable/InRelease"), convey.ShouldNotBeNil)
		})

		convey.Convey("过期之后清理把记录和字节一起收走", func() {
			// 不睡等：直接把过期时刻拨到过去，用例才不靠时钟跑得多快。
			repo.expire("/dists/stable/InRelease")

			removed, err := svc.Sweep(context.Background())
			convey.So(err, convey.ShouldBeNil)
			convey.So(removed, convey.ShouldEqual, 1)
			convey.So(repo.byKey("/dists/stable/InRelease"), convey.ShouldBeNil)
			_, ok := store.Has(digestOfString("InRelease"))
			convey.So(ok, convey.ShouldBeFalse)
		})

		convey.Convey("被 pin 的过期对象留下", func() {
			repo.expire("/dists/stable/InRelease")
			convey.So(svc.Pin(context.Background(), &PinRequest{ID: object.ID, Pinned: true}), convey.ShouldBeNil)

			removed, err := svc.Sweep(context.Background())
			convey.So(err, convey.ShouldBeNil)
			convey.So(removed, convey.ShouldEqual, 0)
			convey.So(repo.byKey("/dists/stable/InRelease"), convey.ShouldNotBeNil)
		})
	})
}
