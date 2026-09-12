package cache_svc

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smartystreets/goconvey/convey"

	"github.com/CodFrm/katch/internal/model/entity/cache_entity"
)

// TestGet_SecondPullIsServedFromDisk 目标的第一条：两次相同拉取只回源一次，
// 第二次由磁盘服务。
//
// 「由磁盘服务」不靠命中率计数器来证明——第一次拉完就把假源站关掉，第二次还能
// 拿到同样的字节，说明这份内容确实来自本地，而不是又走了一趟网络。
func TestGet_SecondPullIsServedFromDisk(t *testing.T) {
	convey.Convey("同一个对象拉两次只回源一次，第二次来自磁盘", t, func() {
		const body = "deb package bytes"
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/vnd.debian.binary-package")
			_, _ = io.WriteString(w, body)
		})
		svc, repo, _ := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})
		ctx := context.Background()

		first, meta, err := svc.Get(ctx, target("deb.debian.org", "/pool/main/n/nginx.deb"))
		convey.So(err, convey.ShouldBeNil)
		got, err := io.ReadAll(first)
		convey.So(err, convey.ShouldBeNil)
		convey.So(first.Close(), convey.ShouldBeNil)
		convey.So(string(got), convey.ShouldEqual, body)
		convey.So(meta.StatusCode, convey.ShouldEqual, http.StatusOK)

		// 关掉假源站：接下来的任何一次回源都会失败。
		o.srv.Close()

		second, meta2, err := svc.Get(ctx, target("deb.debian.org", "/pool/main/n/nginx.deb"))
		convey.So(err, convey.ShouldBeNil)
		got2, err := io.ReadAll(second)
		convey.So(err, convey.ShouldBeNil)
		convey.So(second.Close(), convey.ShouldBeNil)
		convey.So(string(got2), convey.ShouldEqual, body)
		convey.So(meta2.StatusCode, convey.ShouldEqual, http.StatusOK)
		convey.So(o.hits.Load(), convey.ShouldEqual, 1)

		// 缓存命中要和回源给出同样的响应，否则客户端会按别的类型解析同一份字节。
		convey.So(meta2.Header.Get("Content-Type"), convey.ShouldEqual, "application/vnd.debian.binary-package")
		convey.So(meta2.ContentLength, convey.ShouldEqual, int64(len(body)))

		row := repo.byKey("/pool/main/n/nginx.deb")
		convey.So(row, convey.ShouldNotBeNil)
		convey.So(row.Immutable, convey.ShouldBeTrue)
		convey.So(row.Size, convey.ShouldEqual, int64(len(body)))
		convey.So(row.HitCount, convey.ShouldEqual, 1)
		convey.So(row.LastAccessAt, convey.ShouldBeGreaterThan, 0)
	})
}

// TestGet_FirstByteArrivesBeforeDownloadCompletes 目标的第二条：首字节先于响应体
// 下载完成到达。
//
// 假源站发完前半段就卡住，直到用例确认已经读到前半段才继续。若实现是「下完再发」，
// 这里会死等到超时——一个几百 MB 的镜像层，先下完再发就等于首字节时间等于整个
// 下载时间。
func TestGet_FirstByteArrivesBeforeDownloadCompletes(t *testing.T) {
	convey.Convey("下载还没完成就能读到首字节", t, func() {
		release := make(chan struct{})
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "HEAD")
			w.(http.Flusher).Flush()
			<-release
			_, _ = io.WriteString(w, "TAIL")
		})
		svc, _, _ := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})

		// 取响应体和读首字节都放在这个协程里：若实现是「下完再发」，卡住的可能是
		// Get 本身，也可能是第一次 Read，两处都要被这道超时罩住。
		head := make([]byte, 4)
		opened := make(chan io.ReadCloser, 1)
		read := make(chan error, 1)
		go func() {
			body, _, err := svc.Get(context.Background(), target("deb.debian.org", "/pool/big.deb"))
			if err != nil {
				read <- err
				return
			}
			opened <- body
			_, err = io.ReadFull(body, head)
			read <- err
		}()
		select {
		case err := <-read:
			convey.So(err, convey.ShouldBeNil)
			convey.So(string(head), convey.ShouldEqual, "HEAD")
		case <-time.After(3 * time.Second):
			close(release)
			t.Fatal("上游还没发完，首字节就已经等不到了——说明是下完整份才开始转发")
		}

		close(release)
		body := <-opened
		rest, err := io.ReadAll(body)
		convey.So(err, convey.ShouldBeNil)
		convey.So(string(rest), convey.ShouldEqual, "TAIL")
		convey.So(body.Close(), convey.ShouldBeNil)
	})
}

// TestGet_ConcurrentPullsCoalesceIntoOneOriginFetch 决策 9：冷启动时一堆客户端
// 同时拉同一个基础镜像层，不合并就把并发原样放大到上游。
//
// 假源站在收到第一个请求后卡住，用例确认 20 个请求都已经在等同一份下载之后才放行；
// 若实现没有合并，这 20 个请求会各自打一次源站，计数就不是 1。
func TestGet_ConcurrentPullsCoalesceIntoOneOriginFetch(t *testing.T) {
	convey.Convey("同一对象的并发回源只打一次上游", t, func() {
		const body = "shared layer"
		release := make(chan struct{})
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			<-release
			_, _ = io.WriteString(w, body)
		})
		svc, _, _ := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})

		const n = 20
		var wg sync.WaitGroup
		results := make([]string, n)
		errs := make([]error, n)
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r, _, err := svc.Get(context.Background(), target("deb.debian.org", "/pool/base.deb"))
				if err != nil {
					errs[i] = err
					return
				}
				b, err := io.ReadAll(r)
				errs[i] = err
				results[i] = string(b)
				_ = r.Close()
			}()
		}
		// 等所有请求都进到「正在等这次下载」的状态，再让源站开口。
		time.Sleep(200 * time.Millisecond)
		close(release)
		wg.Wait()

		for i := range n {
			convey.So(errs[i], convey.ShouldBeNil)
			convey.So(results[i], convey.ShouldEqual, body)
		}
		convey.So(o.hits.Load(), convey.ShouldEqual, 1)
	})
}

// TestGet_ClientDisconnectStillFinishesCacheWrite 失败与降级：回源过程中客户端断开，
// 已下载的部分仍要写完缓存——下一个请求就能命中，否则一次断线就浪费掉整趟回源。
func TestGet_ClientDisconnectStillFinishesCacheWrite(t *testing.T) {
	convey.Convey("客户端中途断开，缓存仍然写完", t, func() {
		const body = "0123456789abcdefghij"
		release := make(chan struct{})
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, body[:4])
			w.(http.Flusher).Flush()
			<-release
			_, _ = io.WriteString(w, body[4:])
		})
		svc, repo, _ := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})

		// 客户端的 context 在读到前 4 个字节之后就被取消——这就是「断开」。
		ctx, cancel := context.WithCancel(context.Background())
		r, _, err := svc.Get(ctx, target("deb.debian.org", "/pool/partial.deb"))
		convey.So(err, convey.ShouldBeNil)
		head := make([]byte, 4)
		_, err = io.ReadFull(r, head)
		convey.So(err, convey.ShouldBeNil)
		cancel()
		_ = r.Close()

		close(release)
		// 断开之后没有任何客户端在读了，缓存记录仍应出现。
		row := waitForKey(repo, "/pool/partial.deb", 3*time.Second)
		convey.So(row, convey.ShouldNotBeNil)
		convey.So(row.Size, convey.ShouldEqual, int64(len(body)))

		o.srv.Close()
		again, _, err := svc.Get(context.Background(), target("deb.debian.org", "/pool/partial.deb"))
		convey.So(err, convey.ShouldBeNil)
		got, err := io.ReadAll(again)
		convey.So(err, convey.ShouldBeNil)
		convey.So(again.Close(), convey.ShouldBeNil)
		convey.So(string(got), convey.ShouldEqual, body)
		convey.So(o.hits.Load(), convey.ShouldEqual, 1)
	})
}

// TestGet_MutableObjectUsesShortTTL 决策 7 的另一档：tag、InRelease 这类会变的对象
// 只能短 TTL 缓存，过期后必须重新回源，否则镜像站会持续发出过期内容。
func TestGet_MutableObjectUsesShortTTL(t *testing.T) {
	convey.Convey("可变对象按 TTL 过期", t, func() {
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "InRelease v1")
		})
		up := staticUpstream("deb.debian.org")
		up.MutableTTLSeconds = 60
		svc, repo, _ := setupSvc(t, o, up, Options{})
		ctx := context.Background()
		key := "/dists/stable/InRelease"

		r, _, err := svc.Get(ctx, target("deb.debian.org", key))
		convey.So(err, convey.ShouldBeNil)
		_, _ = io.ReadAll(r)
		convey.So(r.Close(), convey.ShouldBeNil)

		row := repo.byKey(key)
		convey.So(row, convey.ShouldNotBeNil)
		// 不在不可变模式里，所以走可变档：有过期时刻，且用的是上游自己的 TTL。
		convey.So(row.Immutable, convey.ShouldBeFalse)
		convey.So(row.ExpiresAt, convey.ShouldBeGreaterThan, time.Now().Unix())
		convey.So(row.ExpiresAt, convey.ShouldBeLessThanOrEqualTo, time.Now().Unix()+60)

		convey.Convey("没过期时不回源", func() {
			r2, _, err := svc.Get(ctx, target("deb.debian.org", key))
			convey.So(err, convey.ShouldBeNil)
			_, _ = io.ReadAll(r2)
			convey.So(r2.Close(), convey.ShouldBeNil)
			convey.So(o.hits.Load(), convey.ShouldEqual, 1)
		})

		convey.Convey("过期之后重新回源", func() {
			repo.expire(key)
			r2, _, err := svc.Get(ctx, target("deb.debian.org", key))
			convey.So(err, convey.ShouldBeNil)
			_, _ = io.ReadAll(r2)
			convey.So(r2.Close(), convey.ShouldBeNil)
			convey.So(o.hits.Load(), convey.ShouldEqual, 2)
			// 过期重取不应留下第二条记录，否则同一条路径会在表里越堆越多。
			convey.So(len(repo.all()), convey.ShouldEqual, 1)
		})
	})
}

// TestGet_CorruptedCopyIsDiscardedAndRefetched 缓存：读取时校验完整性，
// 校验失败就丢掉该副本并回源。静默自愈不行——这类失败意味着磁盘或写入路径坏了。
func TestGet_CorruptedCopyIsDiscardedAndRefetched(t *testing.T) {
	convey.Convey("盘上的副本和记录对不上时丢弃并回源", t, func() {
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "full content")
		})
		svc, repo, store := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})
		ctx := context.Background()
		key := "/pool/broken.deb"

		r, _, err := svc.Get(ctx, target("deb.debian.org", key))
		convey.So(err, convey.ShouldBeNil)
		_, _ = io.ReadAll(r)
		convey.So(r.Close(), convey.ShouldBeNil)

		// 把盘上的副本截短，模拟磁盘出问题：记录说 12 字节，实际只有 3 字节。
		truncateBlob(t, store, repo, key, 3)

		r2, meta, err := svc.Get(ctx, target("deb.debian.org", key))
		convey.So(err, convey.ShouldBeNil)
		got, err := io.ReadAll(r2)
		convey.So(err, convey.ShouldBeNil)
		convey.So(r2.Close(), convey.ShouldBeNil)
		// 坏副本没有被发给客户端，而是回了一次源。
		convey.So(string(got), convey.ShouldEqual, "full content")
		convey.So(meta.StatusCode, convey.ShouldEqual, http.StatusOK)
		convey.So(o.hits.Load(), convey.ShouldEqual, 2)

		convey.Convey("字节数没变、内容被改写的副本靠摘要认出来", func() {
			// 这一类损坏躲得过大小比对：读的时候一路算摘要，到末尾才发现不对。
			// 发现之后要丢掉这条记录，下一次拉取才会回源，而不是反复发出坏字节。
			corruptBlob(t, store, repo, key)
			bad, _, err := svc.Get(ctx, target("deb.debian.org", key))
			convey.So(err, convey.ShouldBeNil)
			_, err = io.ReadAll(bad)
			convey.So(errors.Is(err, ErrCacheCorrupted), convey.ShouldBeTrue)
			convey.So(bad.Close(), convey.ShouldBeNil)
			convey.So(repo.byKey(key), convey.ShouldBeNil)

			fresh, _, err := svc.Get(ctx, target("deb.debian.org", key))
			convey.So(err, convey.ShouldBeNil)
			got, err := io.ReadAll(fresh)
			convey.So(err, convey.ShouldBeNil)
			convey.So(fresh.Close(), convey.ShouldBeNil)
			convey.So(string(got), convey.ShouldEqual, "full content")
		})
	})
}

// TestGet_UncacheableResponsesArePassThrough 失败与降级：上游 4xx 原样透传且不缓存；
// Range 请求拿到的是半截内容，缓存它等于把半截当成整份。
func TestGet_UncacheableResponsesArePassThrough(t *testing.T) {
	convey.Convey("不该缓存的响应只透传", t, func() {
		convey.Convey("上游 404 不进缓存", func() {
			o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, "not found")
			})
			svc, repo, _ := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})
			r, meta, err := svc.Get(context.Background(), target("deb.debian.org", "/pool/missing.deb"))
			convey.So(err, convey.ShouldBeNil)
			_, _ = io.ReadAll(r)
			convey.So(r.Close(), convey.ShouldBeNil)
			convey.So(meta.StatusCode, convey.ShouldEqual, http.StatusNotFound)
			convey.So(len(repo.all()), convey.ShouldEqual, 0)
		})

		convey.Convey("带 Range 的请求不进缓存", func() {
			o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Range", "bytes 0-3/12")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.WriteString(w, "full")
			})
			svc, repo, _ := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})
			tg := target("deb.debian.org", "/pool/part.deb")
			tg.Header.Set("Range", "bytes=0-3")
			r, meta, err := svc.Get(context.Background(), tg)
			convey.So(err, convey.ShouldBeNil)
			got, _ := io.ReadAll(r)
			convey.So(r.Close(), convey.ShouldBeNil)
			convey.So(string(got), convey.ShouldEqual, "full")
			convey.So(meta.StatusCode, convey.ShouldEqual, http.StatusPartialContent)
			convey.So(len(repo.all()), convey.ShouldEqual, 0)
		})

		convey.Convey("HEAD 不进缓存", func() {
			o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", "12")
			})
			svc, repo, _ := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})
			tg := target("deb.debian.org", "/pool/head.deb")
			tg.Method = http.MethodHead
			r, _, err := svc.Get(context.Background(), tg)
			convey.So(err, convey.ShouldBeNil)
			convey.So(r.Close(), convey.ShouldBeNil)
			convey.So(len(repo.all()), convey.ShouldEqual, 0)
		})
	})
}

// TestGet_DegradesToPassThroughWithoutStore 失败与降级：缓存目录不可写时降级为
// 纯透传，服务继续可用——缓存是优化，它坏掉不该让拉取整体失败。
func TestGet_DegradesToPassThroughWithoutStore(t *testing.T) {
	convey.Convey("没有可用的缓存目录时仍能拉取", t, func() {
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "still works")
		})
		up := staticUpstream("deb.debian.org")
		_, repo, _ := setupSvc(t, o, up, Options{})
		// 这一个 svc 没有磁盘可用，正是缓存目录建不出来时 main 装配出来的形态。
		svc := New(nil, Options{})

		r, meta, err := svc.Get(context.Background(), target("deb.debian.org", "/pool/x.deb"))
		convey.So(err, convey.ShouldBeNil)
		got, err := io.ReadAll(r)
		convey.So(err, convey.ShouldBeNil)
		convey.So(r.Close(), convey.ShouldBeNil)
		convey.So(string(got), convey.ShouldEqual, "still works")
		convey.So(meta.StatusCode, convey.ShouldEqual, http.StatusOK)
		convey.So(len(repo.all()), convey.ShouldEqual, 0)

		convey.Convey("第二次照样透传，不会因为没有缓存而报错", func() {
			r2, _, err := svc.Get(context.Background(), target("deb.debian.org", "/pool/x.deb"))
			convey.So(err, convey.ShouldBeNil)
			_, _ = io.ReadAll(r2)
			convey.So(r2.Close(), convey.ShouldBeNil)
			convey.So(o.hits.Load(), convey.ShouldEqual, 2)
		})
	})
}

// TestGet_UnknownUpstreamIsStillRejected 缓存层不能把白名单这道闸放松：
// 不在上游表里的主机照旧是 ErrUpstreamNotAllowed。
func TestGet_UnknownUpstreamIsStillRejected(t *testing.T) {
	convey.Convey("未知上游在缓存层之后仍然被拒", t, func() {
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "should not happen")
		})
		svc, _, _ := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})

		_, _, err := svc.Get(context.Background(), target("evil.example.com", "/pool/x.deb"))
		convey.So(err, convey.ShouldNotBeNil)
		convey.So(strings.Contains(err.Error(), "evil.example.com"), convey.ShouldBeFalse)
		convey.So(o.hits.Load(), convey.ShouldEqual, 0)
	})
}

// TestGet_OriginFailureIsNotCached 上游不可达时不能留下任何缓存记录，
// 否则一次网络抖动会被固化成「这个对象是空的」。
func TestGet_OriginFailureIsNotCached(t *testing.T) {
	convey.Convey("回源失败不写缓存", t, func() {
		o := newOrigin(t, func(http.ResponseWriter, *http.Request) {})
		svc, repo, _ := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})
		o.srv.Close()

		_, _, err := svc.Get(context.Background(), target("deb.debian.org", "/pool/x.deb"))
		convey.So(err, convey.ShouldNotBeNil)
		convey.So(len(repo.all()), convey.ShouldEqual, 0)
	})
}

func waitForKey(repo *fakeRepo, key string, timeout time.Duration) *cache_entity.CacheObject {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if row := repo.byKey(key); row != nil {
			return row
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}
