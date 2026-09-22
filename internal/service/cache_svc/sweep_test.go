package cache_svc

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/smartystreets/goconvey/convey"

	"github.com/CodFrm/katch/internal/service/setting_svc"
)

// 「可变对象由 TTL 自行过期」不能只是「读到它时发现它过期了」：一个再也没人来取的
// 过期对象，记录和盘上的字节会一直留着，还一直算进配额，于是「缓存总容量有上限」
// 这条会被一批死对象慢慢顶穿——而没有上游 validator 的那些又进不了 LRU 的候选。
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

// TestSweep_DoesNotDeleteLegacyDigestPromotedAfterListing 覆盖清理与升级自愈的竞态：
// Sweep 拿到旧快照后，命中路径可能已经把同一行提升成不可变对象，删除时必须复核。
func TestSweep_DoesNotDeleteLegacyDigestPromotedAfterListing(t *testing.T) {
	convey.Convey("已被列为过期候选的 registry digest 提升后不再删除", t, func() {
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "origin")
		})
		up := registryUpstream("registry.test")
		up.ImmutablePatterns = nil
		svc, repo, _ := setupSvc(t, o, up, Options{})
		const path = "/library/redis/blobs/sha256:2e752c"
		const content = "legacy layer"
		convey.So(svc.Put(context.Background(), &PutRequest{
			UpstreamID: 9, Key: path, Content: strings.NewReader(content),
			ContentType: "application/octet-stream", Immutable: false, TTLSeconds: 300,
		}), convey.ShouldBeNil)
		repo.expire(path)
		repo.deleteStarted = make(chan struct{}, 1)
		repo.deleteGate = make(chan struct{})

		type sweepResult struct {
			removed int64
			err     error
		}
		done := make(chan sweepResult, 1)
		go func() {
			removed, err := svc.Sweep(context.Background())
			done <- sweepResult{removed: removed, err: err}
		}()
		select {
		case <-repo.deleteStarted:
		case <-time.After(3 * time.Second):
			t.Fatal("Sweep 没有走到删除候选")
		}

		got, meta := pullWith(t, svc, registryTarget("registry.test", path))
		convey.So(got, convey.ShouldEqual, content)
		convey.So(meta.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusHit)
		close(repo.deleteGate)
		result := <-done
		convey.So(result.err, convey.ShouldBeNil)
		convey.So(result.removed, convey.ShouldEqual, int64(0))
		convey.So(repo.byKey(path), convey.ShouldNotBeNil)
		convey.So(o.hits.Load(), convey.ShouldEqual, int64(0))
	})
}

// TestSweep_KeepsExpiredObjectsThatCanBeRevalidated 过期但带着上游 validator 的可变对象
// 不被例行清理收走：留着它，下一次请求才能用一次条件回源续期，上游答 304 就不必整份
// 重下（决策 9）。没有 validator 的照旧收走——留着它也只能整份重下。
func TestSweep_KeepsExpiredObjectsThatCanBeRevalidated(t *testing.T) {
	ro := newRevalidationOrigin(t, "InRelease", staticValidatorHeaders(), notModifiedAfterFirst(nil))
	up := staticUpstream("deb.debian.org")
	svc, repo, _ := setupSvc(t, ro.originStub, up, Options{})
	const path = "/dists/stable/InRelease"
	pullWith(t, svc, target(up.Host, path))
	repo.expire(path)

	removed, err := svc.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 || repo.byKey(path) == nil {
		t.Fatalf("Sweep 收走了可续期的过期对象：removed=%d", removed)
	}
	body, meta := pullWith(t, svc, target(up.Host, path))
	if body != "InRelease" || meta.Header.Get(cacheStatusHeader) != cacheStatusRevalidated {
		t.Fatalf("Sweep 之后的请求 = %q / %q，要的是一次 304 续期", body, meta.Header.Get(cacheStatusHeader))
	}

	// 同一套清理对没有 validator 的过期对象照旧生效。Put 写进去的记录不带 validator。
	if err := svc.Put(context.Background(), &PutRequest{
		UpstreamID: up.ID, Key: "/plain", Content: strings.NewReader("plain"),
		ContentType: "text/plain", TTLSeconds: 60,
	}); err != nil {
		t.Fatal(err)
	}
	repo.expire("/plain")
	removed, err = svc.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 || repo.byKey("/plain") != nil {
		t.Fatalf("没有 validator 的过期对象没被收走：removed=%d", removed)
	}
}

// TestReclaim_EvictsExpiredRevalidatableMutableObjects Sweep 不收的那批过期对象要由配额
// 压力按 LRU 收走，否则它们会一直占着配额；还新鲜的可变对象仍然不进 LRU。
func TestReclaim_EvictsExpiredRevalidatableMutableObjects(t *testing.T) {
	ro := newRevalidationOrigin(t, "12345", staticValidatorHeaders(), nil)
	up := staticUpstream("deb.debian.org")
	svc, repo, _ := setupSvc(t, ro.originStub, up, Options{Runtime: newFakeRuntime(t, quotaOf(12, 100))})

	pullWith(t, svc, target(up.Host, "/dists/old/InRelease"))   // 最早访问，稍后过期
	pullWith(t, svc, target(up.Host, "/dists/fresh/InRelease")) // 可变，仍新鲜
	repo.expire("/dists/old/InRelease")
	// 第三个对象把总量推过配额（15 > 12），回收到 12 以下只需收走一个。
	if err := svc.Put(context.Background(), &PutRequest{
		UpstreamID: up.ID, Key: "/pool/main/h/hello.deb", Content: strings.NewReader("abcde"),
		ContentType: "application/vnd.debian.binary-package", Immutable: true,
	}); err != nil {
		t.Fatal(err)
	}

	if row := repo.byKey("/dists/old/InRelease"); row != nil {
		t.Fatalf("过期可续期的对象最久未访问，却没被淘汰：%+v", row)
	}
	if repo.byKey("/dists/fresh/InRelease") == nil {
		t.Fatal("还新鲜的可变对象被淘汰了")
	}
	if repo.byKey("/pool/main/h/hello.deb") == nil {
		t.Fatal("不可变对象比更久未访问的过期对象先被淘汰")
	}
}

// TestReclaim_DoesNotEvictRowRewrittenAfterListing 覆盖淘汰与重新写入的竞态：过期可续期的
// 可变对象被列为淘汰候选之后，一次回源可能已经把同一行换成了新内容、新的过期时刻。
// 淘汰删除时必须复核，否则删掉的是刚写进去的那份，新内容的文件也没人再引用、白占着盘。
func TestReclaim_DoesNotEvictRowRewrittenAfterListing(t *testing.T) {
	ro := newRevalidationOrigin(t, "old-v1", staticValidatorHeaders(),
		func(n int, _ *http.Request, w http.ResponseWriter) int {
			if n > 1 {
				w.Header().Set("Content-Type", "text/plain")
				w.Header().Set("Etag", `"origin-v2"`)
				w.Header().Set("Cache-Control", "public, max-age=60")
				_, _ = io.WriteString(w, "new-content-v2")
				return -1
			}
			return 0
		})
	up := staticUpstream("deb.debian.org")
	runtime := newFakeRuntime(t, quotaOf(1<<30, 100))
	svc, repo, store := setupSvc(t, ro.originStub, up, Options{Runtime: runtime})
	const path = "/dists/stable/InRelease"
	pullWith(t, svc, target(up.Host, path))
	repo.expire(path)

	runtime.mu.Lock()
	runtime.rt.CacheQuotaBytes = 1
	runtime.mu.Unlock()
	repo.deleteStarted = make(chan struct{}, 1)
	repo.deleteGate = make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := svc.Sweep(context.Background())
		done <- err
	}()
	select {
	case <-repo.deleteStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("配额回收没有走到删除候选")
	}

	body, _ := pullWith(t, svc, target(up.Host, path))
	if body != "new-content-v2" {
		t.Fatalf("重新回源的正文 = %q", body)
	}
	close(repo.deleteGate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	row := repo.byKey(path)
	if row == nil {
		t.Fatal("列为候选之后被重新写入的记录仍被淘汰了")
	}
	if row.Digest != digestOfString("new-content-v2") {
		t.Fatalf("记录 digest = %q，要的是新内容", row.Digest)
	}
	file, _, err := store.Open(row.Digest)
	if err != nil {
		t.Fatalf("新内容的文件不见了：%v", err)
	}
	_ = file.Close()
}
