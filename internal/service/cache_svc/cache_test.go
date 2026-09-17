package cache_svc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/smartystreets/goconvey/convey"

	"github.com/CodFrm/katch/internal/metrics"
	"github.com/CodFrm/katch/internal/model/entity/cache_entity"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
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
			// 这一类损坏躲得过大小比对，只有摘要认得出。规格要的是
			// 「校验失败时丢弃该副本并回源」——回源必须发生在**这一次**请求上，
			// 而不是等客户端自己重试：一边把坏字节发出去一边在背后丢记录，
			// 客户端拿到的是一个 200、Content-Length 还对得上的完整坏响应，
			// 它没有任何理由去重试。
			corruptBlob(t, store, repo, key)
			before := o.hits.Load()

			r3, meta3, err := svc.Get(ctx, target("deb.debian.org", key))
			convey.So(err, convey.ShouldBeNil)
			got, err := io.ReadAll(r3)
			convey.So(err, convey.ShouldBeNil)
			convey.So(r3.Close(), convey.ShouldBeNil)

			// 客户端拿到的是好内容，且它来自上游而不是那份坏副本。
			convey.So(string(got), convey.ShouldEqual, "full content")
			convey.So(meta3.StatusCode, convey.ShouldEqual, http.StatusOK)
			convey.So(o.hits.Load(), convey.ShouldEqual, before+1)
			// 坏副本被丢掉之后又按回源的结果重新写了一条，所以记录还在，
			// 但盘上那份坏字节已经不是它指向的内容了。
			fixed := repo.byKey(key)
			convey.So(fixed, convey.ShouldNotBeNil)
			convey.So(fixed.Digest, convey.ShouldEqual, digestOfString("full content"))
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

// TestGet_RewriteDropsStaleValidators 失败与恢复：上游重写同一路径且这次没带
// validator 时，上一份内容的校验值必须跟着覆盖掉。
//
// 留着旧的 ETag，客户端会拿一个对不上的强校验符去做条件请求；上游说「不知道这个
// 标识」，而我们的命中却回放它，等于替上游背书了一份它从未声明过的事实。
func TestGet_RewriteDropsStaleValidators(t *testing.T) {
	convey.Convey("重写内容时上一份的 validator 不能留下", t, func() {
		var served atomic.Int64
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			if served.Add(1) == 1 {
				w.Header().Set("Etag", `"v1"`)
				w.Header().Set("Last-Modified", "Wed, 21 Oct 2015 07:28:00 GMT")
				_, _ = io.WriteString(w, "v1")
				return
			}
			// 第二份没有 validator：旧的必须被清掉。
			_, _ = io.WriteString(w, "v2")
		})
		svc, repo, _ := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})
		ctx := context.Background()
		// 不在 /pool/ 里，按 TTL 可变（staticUpstream 的不可变模式只有 /pool/）。
		const key = "/dists/stable/InRelease"

		r, _, err := svc.Get(ctx, target("deb.debian.org", key))
		convey.So(err, convey.ShouldBeNil)
		_, _ = io.ReadAll(r)
		convey.So(r.Close(), convey.ShouldBeNil)
		_, firstHit := pullWith(t, svc, target("deb.debian.org", key))
		convey.So(firstHit.Header.Get("Etag"), convey.ShouldEqual, `"v1"`)
		first := repo.byKey(key)
		convey.So(first, convey.ShouldNotBeNil)
		convey.So(first.ETag, convey.ShouldEqual, `"v1"`)
		convey.So(first.LastModified, convey.ShouldEqual, "Wed, 21 Oct 2015 07:28:00 GMT")

		// 把这条记录拨到过期，让它必须回源重取。
		repo.expire(key)
		r2, _, err := svc.Get(ctx, target("deb.debian.org", key))
		convey.So(err, convey.ShouldBeNil)
		body, err := io.ReadAll(r2)
		convey.So(err, convey.ShouldBeNil)
		convey.So(r2.Close(), convey.ShouldBeNil)
		convey.So(string(body), convey.ShouldEqual, "v2")
		convey.So(o.hits.Load(), convey.ShouldEqual, 2)
		// 过期重取不留下第二条记录。
		convey.So(len(repo.all()), convey.ShouldEqual, 1)

		_, secondHit := pullWith(t, svc, target("deb.debian.org", key))
		convey.So(secondHit.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusHit)
		convey.So(secondHit.Header.Get("Etag"), convey.ShouldBeEmpty)
		convey.So(secondHit.Header.Get("Last-Modified"), convey.ShouldBeEmpty)
		// 库里也不能留着上一份的校验值。
		rewritten := repo.byKey(key)
		convey.So(rewritten.ETag, convey.ShouldBeEmpty)
		convey.So(rewritten.LastModified, convey.ShouldBeEmpty)
		convey.So(rewritten.Digest, convey.ShouldEqual, digestOfString("v2"))
	})
}

// TestPut_LeavesValidatorsEmpty 直接写缓存的管理/内部路径没有上游响应头，
// 落库的 validator 必须为空，命中也不许回放一个从未存在过的校验符。
func TestPut_LeavesValidatorsEmpty(t *testing.T) {
	convey.Convey("直接写入的缓存对象没有 validator", t, func() {
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Etag", `"should-not-leak"`)
			_, _ = io.WriteString(w, "origin")
		})
		svc, repo, _ := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})
		const key = "/pool/manual.deb"
		convey.So(svc.Put(context.Background(), &PutRequest{
			UpstreamID: 7, Key: key,
			Content: strings.NewReader("manual"), ContentType: "text/plain", Immutable: true,
		}), convey.ShouldBeNil)
		put := repo.byKey(key)
		convey.So(put, convey.ShouldNotBeNil)
		convey.So(put.ETag, convey.ShouldBeEmpty)
		convey.So(put.LastModified, convey.ShouldBeEmpty)

		_, hitMeta := pullWith(t, svc, target("deb.debian.org", key))
		convey.So(hitMeta.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusHit)
		convey.So(hitMeta.Header.Get("Etag"), convey.ShouldBeEmpty)
		convey.So(hitMeta.Header.Get("Last-Modified"), convey.ShouldBeEmpty)
		// 命中由磁盘服务，没有回源。
		convey.So(o.hits.Load(), convey.ShouldEqual, 0)
	})
}

// TestGet_HeadServedFromDisk 持有新鲜完整副本时，普通 HEAD 由本地 200 应答。
//
// 「与 GET 相同的元数据」是这条的全部意义：HEAD 是客户端在不下载正文的前提下
// 核对一份内容的那条路，元数据只要少一个（长度、类型、validator），客户端就会
// 得出一个与 GET 不同的结论——而它据此决定要不要接着 GET。
func TestGet_HeadServedFromDisk(t *testing.T) {
	convey.Convey("新鲜副本的普通 HEAD 本地命中且不发响应体", t, func() {
		const body = "package bytes"
		const etag = `"v1"`
		const lastModified = "Wed, 21 Oct 2015 07:28:00 GMT"
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/vnd.debian.binary-package")
			w.Header().Set("Etag", etag)
			w.Header().Set("Last-Modified", lastModified)
			_, _ = io.WriteString(w, body)
		})
		const path = "/pool/main/n/nginx.deb"
		svc, repo, _ := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})
		pullWith(t, svc, target("deb.debian.org", path))

		head := target("deb.debian.org", path)
		head.Method = http.MethodHead
		got, meta, err := svc.Get(context.Background(), head)
		convey.So(err, convey.ShouldBeNil)
		payload, err := io.ReadAll(got)
		convey.So(err, convey.ShouldBeNil)
		convey.So(got.Close(), convey.ShouldBeNil)
		convey.So(string(payload), convey.ShouldBeEmpty)
		convey.So(meta.StatusCode, convey.ShouldEqual, http.StatusOK)
		convey.So(meta.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusHit)
		convey.So(meta.Header.Get("Content-Type"), convey.ShouldEqual, "application/vnd.debian.binary-package")
		convey.So(meta.Header.Get("Content-Length"), convey.ShouldEqual, strconv.Itoa(len(body)))
		convey.So(meta.ContentLength, convey.ShouldEqual, int64(len(body)))
		convey.So(meta.Header.Get("Etag"), convey.ShouldEqual, etag)
		convey.So(meta.Header.Get("Last-Modified"), convey.ShouldEqual, lastModified)
		// 本地答完，没有回源。
		convey.So(o.hits.Load(), convey.ShouldEqual, 1)
		// HEAD 不该产生或改写任何记录。
		convey.So(len(repo.all()), convey.ShouldEqual, 1)
	})
}

// TestGet_HeadWithoutCopyPassesThrough 没有可用副本时 HEAD 继续透传，也不写缓存。
//
// HEAD 没有响应体，把它「下」进缓存只会留下一条零字节、却声称自己是一份完整
// 对象的记录。
func TestGet_HeadWithoutCopyPassesThrough(t *testing.T) {
	convey.Convey("无副本的 HEAD 透传且不创建记录", t, func() {
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "12")
		})
		svc, repo, _ := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})
		head := target("deb.debian.org", "/pool/main/n/nginx.deb")
		head.Method = http.MethodHead
		got, meta, err := svc.Get(context.Background(), head)
		convey.So(err, convey.ShouldBeNil)
		convey.So(got.Close(), convey.ShouldBeNil)
		convey.So(meta.StatusCode, convey.ShouldEqual, http.StatusOK)
		convey.So(meta.Header.Get(cacheStatusHeader), convey.ShouldNotEqual, cacheStatusHit)
		convey.So(o.hits.Load(), convey.ShouldEqual, 1)
		convey.So(len(repo.all()), convey.ShouldEqual, 0)
	})
}

// TestGet_IfNoneMatchIsEvaluatedLocally If-None-Match 的弱比较、逗号列表与星号。
//
// 匹配返回 304、不匹配返回缓存的 200，两条都不能回源：回源一次就抵消了条件
// 请求省下来的那次传输，而 304 的意义正是「不必再传一遍」。
func TestGet_IfNoneMatchIsEvaluatedLocally(t *testing.T) {
	convey.Convey("If-None-Match 在本地求值", t, func() {
		const body = "package bytes"
		const etag = `"v1"`
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Etag", etag)
			_, _ = io.WriteString(w, body)
		})
		const path = "/pool/main/n/nginx.deb"
		svc, _, _ := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})
		pullWith(t, svc, target("deb.debian.org", path))

		cases := []struct {
			name   string
			value  string
			status int
			body   string
			method string
		}{
			{"强比较命中", `"v1"`, http.StatusNotModified, "", ""},
			{"弱 tag 命中", `W/"v1"`, http.StatusNotModified, "", ""},
			{"列表里任一项命中", `"a", W/"v1", "b"`, http.StatusNotModified, "", ""},
			{"星号命中", "*", http.StatusNotModified, "", ""},
			{"均不匹配返回缓存的 200", `"a", "b"`, http.StatusOK, body, ""},
			{"语法无效按未提供处理", "v1", http.StatusOK, body, ""},
			{"HEAD 同样按弱比较求值", `W/"v1"`, http.StatusNotModified, "", http.MethodHead},
		}
		for _, c := range cases {
			convey.Convey(c.name, func() {
				tg := target("deb.debian.org", path)
				if c.method != "" {
					tg.Method = c.method
				}
				tg.Header.Set("If-None-Match", c.value)
				got, meta, err := svc.Get(context.Background(), tg)
				convey.So(err, convey.ShouldBeNil)
				payload, err := io.ReadAll(got)
				convey.So(err, convey.ShouldBeNil)
				convey.So(got.Close(), convey.ShouldBeNil)
				convey.So(meta.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusHit)
				convey.So(meta.StatusCode, convey.ShouldEqual, c.status)
				convey.So(string(payload), convey.ShouldEqual, c.body)
				if c.status == http.StatusNotModified {
					// 304 不带实体，也就不声明实体长度与类型。
					convey.So(meta.Header.Get("Content-Length"), convey.ShouldBeEmpty)
					convey.So(meta.Header.Get("Content-Type"), convey.ShouldBeEmpty)
				}
			})
		}
		// 所有条件都由本地答完，一次都没有回源。
		convey.Convey("条件请求不回源", func() {
			convey.So(o.hits.Load(), convey.ShouldEqual, 1)
		})
	})
}

// TestGet_IfModifiedSinceOnlyWithoutIfNoneMatch If-Modified-Since 只在没有
// If-None-Match 时求值，且资源未晚于请求时间才返回 304。
func TestGet_IfModifiedSinceOnlyWithoutIfNoneMatch(t *testing.T) {
	convey.Convey("If-Modified-Since 的优先级与日期比较", t, func() {
		const body = "package bytes"
		const etag = `"v1"`
		const lastModified = "Wed, 21 Oct 2015 07:28:00 GMT"
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Etag", etag)
			w.Header().Set("Last-Modified", lastModified)
			_, _ = io.WriteString(w, body)
		})
		const path = "/pool/main/n/nginx.deb"
		svc, _, _ := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})
		pullWith(t, svc, target("deb.debian.org", path))

		cases := []struct {
			name   string
			inm    string
			ims    string
			status int
			body   string
		}{
			{"资源时间等于请求时间", "", lastModified, http.StatusNotModified, ""},
			{"资源早于请求时间", "", "Thu, 22 Oct 2015 07:28:00 GMT", http.StatusNotModified, ""},
			{"资源较新", "", "Tue, 20 Oct 2015 07:28:00 GMT", http.StatusOK, body},
			{"请求日期无效按未提供处理", "", "not a date", http.StatusOK, body},
			{"If-None-Match 不匹配压过 If-Modified-Since", `"other"`, lastModified, http.StatusOK, body},
			{"If-None-Match 命中压过日期", etag, "Tue, 20 Oct 2015 07:28:00 GMT", http.StatusNotModified, ""},
		}
		for _, c := range cases {
			convey.Convey(c.name, func() {
				tg := target("deb.debian.org", path)
				if c.inm != "" {
					tg.Header.Set("If-None-Match", c.inm)
				}
				if c.ims != "" {
					tg.Header.Set("If-Modified-Since", c.ims)
				}
				got, meta, err := svc.Get(context.Background(), tg)
				convey.So(err, convey.ShouldBeNil)
				payload, err := io.ReadAll(got)
				convey.So(err, convey.ShouldBeNil)
				convey.So(got.Close(), convey.ShouldBeNil)
				convey.So(meta.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusHit)
				convey.So(meta.StatusCode, convey.ShouldEqual, c.status)
				convey.So(string(payload), convey.ShouldEqual, c.body)
				if c.status == http.StatusNotModified {
					convey.So(meta.Header.Get("Content-Length"), convey.ShouldBeEmpty)
				}
			})
		}
		convey.Convey("条件请求不回源", func() {
			convey.So(o.hits.Load(), convey.ShouldEqual, 1)
		})
	})
}

// TestGet_IfModifiedSinceNeedsStoredDate 有效条件但副本没有可解析的 Last-Modified
// 时继续透传，而不是猜一个结论。
func TestGet_IfModifiedSinceNeedsStoredDate(t *testing.T) {
	convey.Convey("缺 Last-Modified 的副本遇到有效 If-Modified-Since 时回源", t, func() {
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "body")
		})
		const path = "/pool/main/n/nginx.deb"
		svc, _, _ := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})
		pullWith(t, svc, target("deb.debian.org", path))

		tg := target("deb.debian.org", path)
		tg.Header.Set("If-Modified-Since", "Wed, 21 Oct 2015 07:28:00 GMT")
		got, meta, err := svc.Get(context.Background(), tg)
		convey.So(err, convey.ShouldBeNil)
		_, _ = io.ReadAll(got)
		convey.So(got.Close(), convey.ShouldBeNil)
		convey.So(meta.Header.Get(cacheStatusHeader), convey.ShouldNotEqual, cacheStatusHit)
		convey.So(o.hits.Load(), convey.ShouldEqual, 2)
	})
}

// TestGet_ConditionalWithoutValidatorPassesThrough 有效条件存在但副本缺少对应
// validator 时，判断交回上游——历史记录因此不会得到一个推测出来的 304。
func TestGet_ConditionalWithoutValidatorPassesThrough(t *testing.T) {
	convey.Convey("缺 validator 的条件请求透传", t, func() {
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "body")
		})
		const path = "/pool/main/n/nginx.deb"
		svc, repo, _ := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})
		pullWith(t, svc, target("deb.debian.org", path))

		get := target("deb.debian.org", path)
		get.Header.Set("If-None-Match", `"v1"`)
		body, meta, err := svc.Get(context.Background(), get)
		convey.So(err, convey.ShouldBeNil)
		_, _ = io.ReadAll(body)
		convey.So(body.Close(), convey.ShouldBeNil)
		convey.So(meta.Header.Get(cacheStatusHeader), convey.ShouldNotEqual, cacheStatusHit)
		convey.So(o.hits.Load(), convey.ShouldEqual, 2)

		head := target("deb.debian.org", path)
		head.Method = http.MethodHead
		head.Header.Set("If-None-Match", `"v1"`)
		body, _, err = svc.Get(context.Background(), head)
		convey.So(err, convey.ShouldBeNil)
		convey.So(body.Close(), convey.ShouldBeNil)
		convey.So(o.hits.Load(), convey.ShouldEqual, 3)

		// 透传不写缓存：记录的 validator 仍是空，数量也没有变。
		convey.So(len(repo.all()), convey.ShouldEqual, 1)
		convey.So(repo.byKey(path).ETag, convey.ShouldBeEmpty)
	})
}

// TestGet_NonWritablePassthroughOverridesUpstreamHit 验证本跳直接回源时，不能继承
// 上游 katch 的缓存归因。
func TestGet_NonWritablePassthroughOverridesUpstreamHit(t *testing.T) {
	convey.Convey("HEAD 与条件请求的上游 HIT 必须覆盖为本跳 MISS", t, func() {
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set(cacheStatusHeader, cacheStatusHit)
			_, _ = io.WriteString(w, "body")
		})
		svc, repo, _ := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})

		head := target("deb.debian.org", "/pool/head.deb")
		head.Method = http.MethodHead
		body, meta, err := svc.Get(context.Background(), head)
		convey.So(err, convey.ShouldBeNil)
		convey.So(body.Close(), convey.ShouldBeNil)
		convey.So(meta.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusMiss)
		convey.So(meta.Header.Get(metrics.MissHeader), convey.ShouldEqual, string(metrics.MissFirst))

		conditional := target("deb.debian.org", "/pool/conditional.deb")
		conditional.Header.Set("If-None-Match", `"v1"`)
		body, meta, err = svc.Get(context.Background(), conditional)
		convey.So(err, convey.ShouldBeNil)
		_, _ = io.ReadAll(body)
		convey.So(body.Close(), convey.ShouldBeNil)
		convey.So(meta.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusMiss)
		convey.So(meta.Header.Get(metrics.MissHeader), convey.ShouldEqual, string(metrics.MissFirst))
		convey.So(len(repo.all()), convey.ShouldEqual, 0)
		convey.So(o.hits.Load(), convey.ShouldEqual, 2)
	})
}

// TestGet_IfRangePassesThrough If-Range 与 Range 一样完整透传，且不写对象缓存。
func TestGet_IfRangePassesThrough(t *testing.T) {
	convey.Convey("带 If-Range 的请求不进缓存", t, func() {
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "full body")
		})
		svc, repo, _ := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})
		tg := target("deb.debian.org", "/pool/part.deb")
		tg.Header.Set("If-Range", `"v1"`)
		body, meta, err := svc.Get(context.Background(), tg)
		convey.So(err, convey.ShouldBeNil)
		got, _ := io.ReadAll(body)
		convey.So(body.Close(), convey.ShouldBeNil)
		convey.So(string(got), convey.ShouldEqual, "full body")
		convey.So(meta.StatusCode, convey.ShouldEqual, http.StatusOK)
		convey.So(len(repo.all()), convey.ShouldEqual, 0)
	})
}

// TestGet_RangeAndIfRangePassThroughMarkMiss 带 Range 或 If-Range 的请求完整透传，
// 但仍是真实回源：X-Katch-Cache 必须标成 MISS，手上有新鲜完整副本时也一样。
//
// 客户端只看得到「没有 HIT」时无法判定这一次回了源，运维验证与指标归因都少一档；
// 只标 MISS 而不读不写那份副本，才能让同一 URL 的普通 GET 继续命中。
func TestGet_RangeAndIfRangePassThroughMarkMiss(t *testing.T) {
	const payload = "hello world"
	origin := func() *originStub {
		return newOrigin(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			if r.Header.Get("Range") != "" {
				w.Header().Set("Content-Range", "bytes 0-4/11")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.WriteString(w, "hello")
				return
			}
			_, _ = io.WriteString(w, payload)
		})
	}

	convey.Convey("上游 HIT 不能冒充本地命中", t, func() {
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set(cacheStatusHeader, cacheStatusHit)
			_, _ = io.WriteString(w, payload)
		})
		svc, _, _ := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})
		tg := target("deb.debian.org", "/pool/upstream-hit.deb")
		tg.Header.Set("Range", "bytes=0-4")

		body, meta, err := svc.Get(context.Background(), tg)
		convey.So(err, convey.ShouldBeNil)
		_, _ = io.ReadAll(body)
		convey.So(body.Close(), convey.ShouldBeNil)
		// 这台 katch 确实回了源；上游自己的缓存状态不能改变本跳归因。
		convey.So(meta.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusMiss)
		convey.So(o.hits.Load(), convey.ShouldEqual, 1)
	})

	convey.Convey("没有副本时 Range 与 If-Range 就标 MISS", t, func() {
		o := origin()
		svc, repo, _ := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})
		const path = "/pool/part.deb"

		rangeTg := target("deb.debian.org", path)
		rangeTg.Header.Set("Range", "bytes=0-4")
		body, meta, err := svc.Get(context.Background(), rangeTg)
		convey.So(err, convey.ShouldBeNil)
		got, _ := io.ReadAll(body)
		convey.So(body.Close(), convey.ShouldBeNil)
		convey.So(string(got), convey.ShouldEqual, "hello")
		convey.So(meta.StatusCode, convey.ShouldEqual, http.StatusPartialContent)
		convey.So(meta.Header.Get("Content-Range"), convey.ShouldEqual, "bytes 0-4/11")
		convey.So(meta.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusMiss)

		ifRangeTg := target("deb.debian.org", path)
		ifRangeTg.Header.Set("If-Range", `"v1"`)
		body, meta, err = svc.Get(context.Background(), ifRangeTg)
		convey.So(err, convey.ShouldBeNil)
		got, _ = io.ReadAll(body)
		convey.So(body.Close(), convey.ShouldBeNil)
		convey.So(string(got), convey.ShouldEqual, payload)
		convey.So(meta.StatusCode, convey.ShouldEqual, http.StatusOK)
		convey.So(meta.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusMiss)

		// 两次都越过对象缓存：没有留下任何记录。
		convey.So(len(repo.all()), convey.ShouldEqual, 0)
		convey.So(o.hits.Load(), convey.ShouldEqual, 2)
	})

	convey.Convey("有新鲜完整副本时 Range 本地命中且不动副本", t, func() {
		o := origin()
		svc, repo, _ := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})
		const path = "/pool/part.deb"

		// 先落一份新鲜完整副本，后续范围选择只读取这份完整内容。
		_, first := pullWith(t, svc, target("deb.debian.org", path))
		convey.So(first.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusMiss)
		row := repo.byKey(path)
		convey.So(row, convey.ShouldNotBeNil)

		// 没有 Range 时 If-Range 没有语义，按普通完整缓存命中处理。
		ifRangeTg := target("deb.debian.org", path)
		ifRangeTg.Header.Set("If-Range", `"v1"`)
		body, meta, err := svc.Get(context.Background(), ifRangeTg)
		convey.So(err, convey.ShouldBeNil)
		got, _ := io.ReadAll(body)
		convey.So(body.Close(), convey.ShouldBeNil)
		convey.So(string(got), convey.ShouldEqual, payload)
		convey.So(meta.StatusCode, convey.ShouldEqual, http.StatusOK)
		convey.So(meta.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusHit)

		rangeTg := target("deb.debian.org", path)
		rangeTg.Header.Set("Range", "bytes=0-4")
		body, meta, err = svc.Get(context.Background(), rangeTg)
		convey.So(err, convey.ShouldBeNil)
		got, _ = io.ReadAll(body)
		convey.So(body.Close(), convey.ShouldBeNil)
		convey.So(string(got), convey.ShouldEqual, "hello")
		convey.So(meta.StatusCode, convey.ShouldEqual, http.StatusPartialContent)
		convey.So(meta.Header.Get("Content-Range"), convey.ShouldEqual, "bytes 0-4/11")
		convey.So(meta.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusHit)

		convey.So(len(repo.all()), convey.ShouldEqual, 1)
		convey.So(repo.byKey(path).Digest, convey.ShouldEqual, row.Digest)
		_, hit := pullWith(t, svc, target("deb.debian.org", path))
		convey.So(hit.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusHit)
		convey.So(o.hits.Load(), convey.ShouldEqual, 1)
	})
}

// TestGet_ConditionalOnAbsentStaleOrBrokenCopyPassesThrough 条件请求遇到没有可用
// 副本的三种形态时一律透传：无副本、已过期、磁盘上的字节已损坏。
//
// 本地拿不出一份可信的副本时硬答一个 304，就是告诉客户端「你手上那份就是最新的」——
// 而这句话没有任何根据。
func TestGet_ConditionalOnAbsentStaleOrBrokenCopyPassesThrough(t *testing.T) {
	convey.Convey("条件请求在无可用副本时透传", t, func() {
		const etag = `"v1"`
		origin := func() *originStub {
			return newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				w.Header().Set("Etag", etag)
				_, _ = io.WriteString(w, "body")
			})
		}
		convey.Convey("无副本", func() {
			o := origin()
			svc, repo, _ := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})
			tg := target("deb.debian.org", "/pool/main/n/nginx.deb")
			tg.Header.Set("If-None-Match", etag)
			got, meta, err := svc.Get(context.Background(), tg)
			convey.So(err, convey.ShouldBeNil)
			_, _ = io.ReadAll(got)
			convey.So(got.Close(), convey.ShouldBeNil)
			convey.So(meta.Header.Get(cacheStatusHeader), convey.ShouldNotEqual, cacheStatusHit)
			convey.So(o.hits.Load(), convey.ShouldEqual, 1)
			convey.So(len(repo.all()), convey.ShouldEqual, 0)
		})

		convey.Convey("已过期", func() {
			o := origin()
			svc, repo, _ := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})
			// 不在 /pool/ 下，按 TTL 可变。
			const key = "/dists/stable/InRelease"
			pullWith(t, svc, target("deb.debian.org", key))
			repo.expire(key)
			tg := target("deb.debian.org", key)
			tg.Header.Set("If-None-Match", etag)
			got, meta, err := svc.Get(context.Background(), tg)
			convey.So(err, convey.ShouldBeNil)
			_, _ = io.ReadAll(got)
			convey.So(got.Close(), convey.ShouldBeNil)
			convey.So(meta.Header.Get(cacheStatusHeader), convey.ShouldNotEqual, cacheStatusHit)
			convey.So(o.hits.Load(), convey.ShouldEqual, 2)
		})

		convey.Convey("副本损坏", func() {
			o := origin()
			svc, repo, store := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})
			const key = "/pool/main/n/broken.deb"
			pullWith(t, svc, target("deb.debian.org", key))
			corruptBlob(t, store, repo, key)
			tg := target("deb.debian.org", key)
			tg.Header.Set("If-None-Match", etag)
			got, meta, err := svc.Get(context.Background(), tg)
			convey.So(err, convey.ShouldBeNil)
			_, _ = io.ReadAll(got)
			convey.So(got.Close(), convey.ShouldBeNil)
			convey.So(meta.Header.Get(cacheStatusHeader), convey.ShouldNotEqual, cacheStatusHit)
			convey.So(o.hits.Load(), convey.ShouldEqual, 2)
			// 坏记录已被丢弃，不会再冒充一份可用的副本。
			convey.So(len(repo.all()), convey.ShouldEqual, 0)
		})
	})
}

func TestGet_ConcurrentTransformedFillsShareGenerationKey(t *testing.T) {
	release := make(chan struct{})
	o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	})
	profile := testProfile{
		description:    packageprofile.Description{Profile: upstream_entity.PackageProfileNPM, Name: "test"},
		representation: packageprofile.Representation{Class: packageprofile.ClassMutable, Transform: true, MediaTypes: []string{"application/json"}},
		transform: func(_ context.Context, in packageprofile.TransformRequest) (*packageprofile.TransformResult, error) {
			return &packageprofile.TransformResult{Body: in.Body, ContentType: "application/json"}, nil
		},
	}
	up := staticUpstream("registry.example.com")
	up.PackageProfile = upstream_entity.PackageProfileNPM
	svc, _, _ := setupSvc(t, o, up, transformingOptions(t, profile, 23))

	const clients = 8
	var started sync.WaitGroup
	started.Add(clients)
	errs := make(chan error, clients)
	for range clients {
		go func() {
			started.Done()
			body, _, err := svc.Get(context.Background(), target(up.Host, "/metadata"))
			if err == nil {
				_, err = io.ReadAll(body)
				_ = body.Close()
			}
			errs <- err
		}()
	}
	started.Wait()
	time.Sleep(100 * time.Millisecond)
	close(release)
	for range clients {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if got := o.hits.Load(); got != 1 {
		t.Fatalf("canonical origin fills = %d, want 1", got)
	}
}

func TestGet_TransformedMetadataUsesCanonicalIdentityAndKatchHeaders(t *testing.T) {
	convey.Convey("transformable metadata uses one canonical identity representation", t, func() {
		var gotMethod string
		var gotHeader http.Header
		o := newOrigin(t, func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotHeader = r.Header.Clone()
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Etag", `"origin"`)
			w.Header().Set("Last-Modified", "Wed, 21 Oct 2015 07:28:00 GMT")
			_, _ = io.WriteString(w, `{"url":"origin"}`)
		})
		profile := testProfile{
			description: packageprofile.Description{Profile: upstream_entity.PackageProfileNPM, Name: "test"},
			representation: packageprofile.Representation{Class: packageprofile.ClassMutable, Transform: true,
				MediaTypes: []string{"application/json"}},
			transform: func(_ context.Context, in packageprofile.TransformRequest) (*packageprofile.TransformResult, error) {
				return &packageprofile.TransformResult{Body: bytes.ToUpper(in.Body), ContentType: "application/json"}, nil
			},
		}
		up := staticUpstream("registry.example.com")
		up.PackageProfile = upstream_entity.PackageProfileNPM
		svc, repo, _ := setupSvc(t, o, up, transformingOptions(t, profile, 17))

		tg := target(up.Host, "/metadata")
		tg.Method = http.MethodHead
		tg.Header.Set("Range", "bytes=0-3")
		tg.Header.Set("If-None-Match", `"client-copy"`)
		tg.Header.Set("Accept-Encoding", "gzip")
		body, meta, err := svc.Get(context.Background(), tg)
		convey.So(err, convey.ShouldBeNil)
		payload, err := io.ReadAll(body)
		convey.So(err, convey.ShouldBeNil)
		convey.So(body.Close(), convey.ShouldBeNil)
		convey.So(string(payload), convey.ShouldBeEmpty)
		convey.So(meta.StatusCode, convey.ShouldEqual, http.StatusOK)
		convey.So(gotMethod, convey.ShouldEqual, http.MethodGet)
		convey.So(gotHeader.Get("Accept-Encoding"), convey.ShouldEqual, "identity")
		convey.So(gotHeader.Get("Range"), convey.ShouldBeEmpty)
		convey.So(gotHeader.Get("If-None-Match"), convey.ShouldBeEmpty)

		transformed := `{"URL":"ORIGIN"}`
		sum := sha256.Sum256([]byte(transformed))
		wantETag := `"sha256:` + hex.EncodeToString(sum[:]) + `"`
		convey.So(meta.Header.Get("Etag"), convey.ShouldEqual, wantETag)
		convey.So(meta.Header.Get("Last-Modified"), convey.ShouldBeEmpty)
		convey.So(meta.Header.Get("Content-Encoding"), convey.ShouldBeEmpty)
		convey.So(meta.Header.Get("Content-Length"), convey.ShouldEqual, strconv.Itoa(len(transformed)))
		convey.So(meta.Header.Get("Content-Type"), convey.ShouldEqual, "application/json")
		convey.So(len(repo.all()), convey.ShouldEqual, 1)
		convey.So(repo.all()[0].Key, convey.ShouldContainSubstring, "generation=17")

		got, hit := pullWith(t, svc, target(up.Host, "/metadata"))
		convey.So(got, convey.ShouldEqual, transformed)
		convey.So(hit.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusHit)
		convey.So(o.hits.Load(), convey.ShouldEqual, int64(1))
	})
}

func TestGet_TransformedMetadataRejectsInvalidOriginRepresentations(t *testing.T) {
	cases := []struct {
		name         string
		contentType  string
		encoding     string
		body         []byte
		transformErr error
	}{
		{name: "oversized", contentType: "application/json", body: bytes.Repeat([]byte("x"), maxTransformBytes+1)},
		{name: "wrong media", contentType: "text/plain", body: []byte(`{}`)},
		{name: "encoded", contentType: "application/json", encoding: "gzip", body: []byte(`{}`)},
		{name: "malformed", contentType: "application/json", body: []byte(`{`), transformErr: packageprofile.ErrInvalidMetadata},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var transforms atomic.Int64
			o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				if tc.encoding != "" {
					w.Header().Set("Content-Encoding", tc.encoding)
				}
				_, _ = w.Write(tc.body)
			})
			profile := testProfile{
				description: packageprofile.Description{Profile: upstream_entity.PackageProfileNPM, Name: "test"},
				representation: packageprofile.Representation{Class: packageprofile.ClassMutable, Transform: true,
					MediaTypes: []string{"application/json"}},
				transform: func(_ context.Context, in packageprofile.TransformRequest) (*packageprofile.TransformResult, error) {
					transforms.Add(1)
					if tc.transformErr != nil {
						return nil, tc.transformErr
					}
					return &packageprofile.TransformResult{Body: in.Body, ContentType: "application/json"}, nil
				},
			}
			up := staticUpstream("registry.example.com")
			up.PackageProfile = upstream_entity.PackageProfileNPM
			svc, repo, _ := setupSvc(t, o, up, transformingOptions(t, profile, 1))
			body, meta, err := svc.Get(context.Background(), target(up.Host, "/metadata"))
			if err != nil {
				t.Fatal(err)
			}
			defer body.Close()
			if meta.StatusCode != http.StatusBadGateway {
				t.Fatalf("status = %d", meta.StatusCode)
			}
			if len(repo.all()) != 0 {
				t.Fatal("invalid metadata was cached")
			}
			if tc.name != "malformed" && transforms.Load() != 0 {
				t.Fatal("transform ran before admission")
			}
		})
	}
}

func TestGet_VaryAdmissionAndCanonicalization(t *testing.T) {
	for _, tc := range []struct {
		name       string
		vary       string
		wantCached bool
		wantVary   string
	}{
		{name: "declared plus encoding", vary: "Accept-Encoding, Accept", wantCached: true, wantVary: "Accept"},
		{name: "undeclared", vary: "User-Agent"},
		{name: "wildcard", vary: "*"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Vary", tc.vary)
				_, _ = io.WriteString(w, "artifact")
			})
			profile := testProfile{
				description:    packageprofile.Description{Profile: upstream_entity.PackageProfileNPM, Name: "test"},
				representation: packageprofile.Representation{Class: packageprofile.ClassImmutable, Variants: []string{"Accept"}},
				transform: func(context.Context, packageprofile.TransformRequest) (*packageprofile.TransformResult, error) {
					return nil, errors.New("unexpected")
				},
			}
			up := staticUpstream("files.example.com")
			up.PackageProfile = upstream_entity.PackageProfileNPM
			svc, repo, _ := setupSvc(t, o, up, transformingOptions(t, profile, 1))
			tg := target(up.Host, "/artifact")
			tg.Header.Set("Accept", "application/octet-stream")
			_, first := pullWith(t, svc, tg)
			_, second := pullWith(t, svc, tg)
			if tc.wantCached {
				if o.hits.Load() != 1 || len(repo.all()) != 1 {
					t.Fatalf("hits=%d rows=%d", o.hits.Load(), len(repo.all()))
				}
				if second.Header.Get("Vary") != tc.wantVary {
					t.Fatalf("Vary = %q", second.Header.Get("Vary"))
				}
				if first.Header.Get("Vary") != tc.wantVary {
					t.Fatalf("miss Vary = %q", first.Header.Get("Vary"))
				}
			} else if o.hits.Load() != 2 || len(repo.all()) != 0 {
				t.Fatalf("uncacheable response: hits=%d rows=%d", o.hits.Load(), len(repo.all()))
			}
		})
	}
}

func TestGet_FreshnessAgeAndSafeHeaderReplay(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	clock := now
	o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Cache-Control", "public, max-age=30")
		w.Header().Set("Date", now.Add(-10*time.Second).Format(http.TimeFormat))
		w.Header().Set("Age", "5")
		w.Header().Set("Expires", now.Add(20*time.Second).Format(http.TimeFormat))
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Disposition", `attachment; filename="pkg.tgz"`)
		w.Header().Set("Docker-Content-Digest", "sha256:origin")
		w.Header().Set("X-Origin-Secret", "do-not-store")
		w.Header().Set("Set-Cookie", "session=secret")
		w.Header().Set("Ratelimit-Remaining", "1")
		_, _ = io.WriteString(w, "artifact")
	})
	profile := testProfile{
		description:    packageprofile.Description{Profile: upstream_entity.PackageProfileNPM, Name: "test"},
		representation: packageprofile.Representation{Class: packageprofile.ClassMutable},
		transform: func(context.Context, packageprofile.TransformRequest) (*packageprofile.TransformResult, error) {
			return nil, errors.New("unexpected")
		},
	}
	up := staticUpstream("files.example.com")
	up.PackageProfile = upstream_entity.PackageProfileNPM
	up.MutableTTLSeconds = 60
	opt := transformingOptions(t, profile, 1)
	opt.Now = func() time.Time { return clock }
	svc, repo, _ := setupSvc(t, o, up, opt)
	pullWith(t, svc, target(up.Host, "/artifact"))
	row := repo.byKey("/artifact")
	if row == nil {
		t.Fatal("cache row missing")
	}
	if row.ExpiresAt != now.Unix()+20 {
		t.Fatalf("expires_at = %d", row.ExpiresAt)
	}

	clock = clock.Add(5 * time.Second)
	_, hit := pullWith(t, svc, target(up.Host, "/artifact"))
	if hit.Header.Get("Age") != "15" {
		t.Fatalf("Age = %q", hit.Header.Get("Age"))
	}
	for name, want := range map[string]string{
		"Cache-Control": "public, max-age=30", "Date": now.Add(-10 * time.Second).Format(http.TimeFormat),
		"Expires": now.Add(20 * time.Second).Format(http.TimeFormat), "Accept-Ranges": "bytes",
		"Content-Disposition": `attachment; filename="pkg.tgz"`, "Docker-Content-Digest": "sha256:origin",
	} {
		if got := hit.Header.Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	for _, name := range []string{"X-Origin-Secret", "Set-Cookie", "Ratelimit-Remaining", "Content-Encoding"} {
		if got := hit.Header.Get(name); got != "" {
			t.Fatalf("unsafe %s replayed as %q", name, got)
		}
	}
}

func TestGet_MustRevalidateAllowsFreshReuseAndRejectsStale(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	clock := now
	var o *originStub
	o = newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=60, must-revalidate")
		w.Header().Set("Date", now.Format(http.TimeFormat))
		w.Header().Set("Age", "10")
		_, _ = io.WriteString(w, "body-"+strconv.FormatInt(o.hits.Load(), 10))
	})
	opt := Options{Now: func() time.Time { return clock }}
	svc, repo, _ := setupSvc(t, o, staticUpstream("files.example.com"), opt)
	tg := target("files.example.com", "/service-index.json")

	first, _ := pullWith(t, svc, tg)
	clock = clock.Add(49 * time.Second)
	second, hit := pullWith(t, svc, tg)
	if first != "body-1" || second != first {
		t.Fatalf("fresh bodies = %q, %q", first, second)
	}
	if got := o.hits.Load(); got != 1 {
		t.Fatalf("fresh origin hits = %d, want 1", got)
	}
	if got := hit.Header.Get("Cache-Control"); got != "public, max-age=60, must-revalidate" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if row := repo.byKey("/service-index.json"); row == nil || row.RequiresRevalidation {
		t.Fatalf("fresh must-revalidate row = %+v", row)
	}

	clock = clock.Add(time.Second)
	stale, _ := pullWith(t, svc, tg)
	if stale != "body-2" {
		t.Fatalf("stale response body = %q, want revalidated body", stale)
	}
	if got := o.hits.Load(); got != 2 {
		t.Fatalf("stale origin hits = %d, want 2", got)
	}
}

func TestGet_NoCacheStillRevalidatesEveryReuse(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=60, no-cache")
		w.Header().Set("Date", now.Format(http.TimeFormat))
		_, _ = io.WriteString(w, "body")
	})
	svc, repo, _ := setupSvc(t, o, staticUpstream("files.example.com"), Options{
		Now: func() time.Time { return now },
	})
	tg := target("files.example.com", "/service-index.json")

	pullWith(t, svc, tg)
	pullWith(t, svc, tg)
	pullWith(t, svc, tg)
	if got := o.hits.Load(); got != 3 {
		t.Fatalf("origin hits = %d, want 3", got)
	}
	if row := repo.byKey("/service-index.json"); row == nil || !row.RequiresRevalidation {
		t.Fatalf("no-cache row = %+v", row)
	}
}

func TestGet_CacheControlStoragePolicyAndAgeOverflow(t *testing.T) {
	for _, directive := range []string{"no-store", "private"} {
		t.Run(directive, func(t *testing.T) {
			o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Cache-Control", directive)
				_, _ = io.WriteString(w, "body")
			})
			svc, repo, _ := setupSvc(t, o, staticUpstream("files.example.com"), Options{})
			pullWith(t, svc, target("files.example.com", "/x"))
			pullWith(t, svc, target("files.example.com", "/x"))
			if len(repo.all()) != 0 || o.hits.Load() != 2 {
				t.Fatalf("rows=%d hits=%d", len(repo.all()), o.hits.Load())
			}
		})
	}
	t.Run("origin age overflow is stale", func(t *testing.T) {
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "public, max-age=60")
			w.Header().Set("Age", strconv.FormatInt(math.MaxInt64, 10))
			_, _ = io.WriteString(w, "body")
		})
		svc, repo, _ := setupSvc(t, o, staticUpstream("files.example.com"), Options{})
		pullWith(t, svc, target("files.example.com", "/x"))
		pullWith(t, svc, target("files.example.com", "/x"))
		if len(repo.all()) != 1 || o.hits.Load() != 2 {
			t.Fatalf("rows=%d hits=%d", len(repo.all()), o.hits.Load())
		}
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
