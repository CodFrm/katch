package cache_svc

import (
	"context"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/smartystreets/goconvey/convey"

	"github.com/CodFrm/katch/internal/metrics"
	"github.com/CodFrm/katch/internal/service/proxy_svc"
)

// 界面上的「回源原因分解」要的是每一次未命中的归因，而这个判断只有缓存层做得出来：
// 拉取路径最外层那个中间件看到的只是一个 200 加一句 MISS，它分不出「从来没缓存过」
// 和「缓存过但被淘汰了」。所以归因在这一层判、写在响应头上，由中间件按请求收进
// 分钟桶（决策 16：每请求写库会把热路径拖进事务）。
//
// 用真磁盘 + 真假源站走一遍生产路径，而不是直接调判定函数：归因的每一个分支
// 都由「表里有没有记录、记录过没过期、记录是被谁收走的」决定，这些状态只有
// 沿着 Get 真的跑一遍才会自然出现。

// missReasonOf 这次响应上留下的回源原因，命中时是空串。
func missReasonOf(meta *proxy_svc.Meta) string {
	return meta.Header.Get(metrics.MissHeader)
}

// pull 拉一次并把响应体读完。
//
// 必须读完再关：回源那趟 pump 要等响应体读到 EOF 才落库，半途关掉的话下一次
// 拉取看见的是一个还没写完的缓存，归因就成了下载快慢的函数。
func pull(t *testing.T, svc CacheSvc, host, path string) *proxy_svc.Meta {
	t.Helper()
	body, meta, err := svc.Get(context.Background(), target(host, path))
	if err != nil {
		t.Fatalf("拉取 %s 失败：%v", path, err)
	}
	if _, err := io.ReadAll(body); err != nil {
		t.Fatalf("读取 %s 的响应体失败：%v", path, err)
	}
	if err := body.Close(); err != nil {
		t.Fatalf("关闭 %s 的响应体失败：%v", path, err)
	}
	return meta
}

// TestGet_MissReasonFirstPull 第一次拉一个从没缓存过的对象，归因是「首次拉取」；
// 紧接着的第二次是命中，一个原因都不该落下。
func TestGet_MissReasonFirstPull(t *testing.T) {
	convey.Convey("从没缓存过的对象记成首次拉取，命中不记任何原因", t, func() {
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "deb package bytes")
		})
		svc, _, _ := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})

		first := pull(t, svc, "deb.debian.org", "/pool/main/n/nginx.deb")
		convey.So(missReasonOf(first), convey.ShouldEqual, string(metrics.MissFirst))

		second := pull(t, svc, "deb.debian.org", "/pool/main/n/nginx.deb")
		convey.So(second.Header.Get("X-Katch-Cache"), convey.ShouldEqual, "HIT")
		// 命中一个原因都不落：四个原因之和就是未命中数，命中掺一个进去，
		// 界面上那条占比条的分母立刻和命中率的分子打架。
		convey.So(missReasonOf(second), convey.ShouldBeEmpty)
	})
}

// TestGet_MissReasonTTLExpired 可变对象的 TTL 过期之后回源，归因是「TTL 过期」。
func TestGet_MissReasonTTLExpired(t *testing.T) {
	convey.Convey("TTL 过期之后的回源记成 TTL 过期", t, func() {
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "InRelease")
		})
		svc, repo, _ := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})
		key := "/dists/stable/InRelease"

		convey.So(missReasonOf(pull(t, svc, "deb.debian.org", key)),
			convey.ShouldEqual, string(metrics.MissFirst))

		repo.expire(key)
		convey.So(missReasonOf(pull(t, svc, "deb.debian.org", key)),
			convey.ShouldEqual, string(metrics.MissTTL))
		convey.So(o.hits.Load(), convey.ShouldEqual, 2)

		convey.Convey("记录已经被清理收走，归因也还是 TTL 过期", func() {
			// 清理把过期记录连同字节一起收走（Sweep），表里于是什么都不剩——
			// 单看「查不到记录」会把它记成首次拉取，那条占比条就会在一个
			// 全是可变对象的上游上显示成「几乎都是首次拉取」。
			repo.expire(key)
			removed, err := svc.Sweep(context.Background())
			convey.So(err, convey.ShouldBeNil)
			convey.So(removed, convey.ShouldEqual, 1)
			convey.So(repo.byKey(key), convey.ShouldBeNil)

			convey.So(missReasonOf(pull(t, svc, "deb.debian.org", key)),
				convey.ShouldEqual, string(metrics.MissTTL))
		})
	})
}

// TestGet_MissReasonEvicted 不可变对象被 LRU 淘汰之后再拉，归因是「被淘汰」。
//
// 这一条是四个原因里最值钱的：它和「首次拉取」在表上长得一模一样（记录都不在），
// 而它们说的是两件相反的事——首次拉取是缓存还没热起来，被淘汰后回源是配额太小。
func TestGet_MissReasonEvicted(t *testing.T) {
	convey.Convey("被淘汰的对象再被拉时记成被淘汰", t, func() {
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "0123456789")
		})
		// 配额 25 字节、回收到 80%：写进第三个 10 字节的对象就得腾地方。
		svc, repo, _ := setupSvc(t, o, staticUpstream("deb.debian.org"),
			Options{Runtime: newFakeRuntime(t, quotaOf(25, 80))})

		putObject(t, svc, "/pool/a.deb", "aaaaaaaaaa", true)
		putObject(t, svc, "/pool/b.deb", "bbbbbbbbbb", true)
		// 命中一次 A，B 于是成了最久未用的那一个。
		pull(t, svc, "deb.debian.org", "/pool/a.deb")
		putObject(t, svc, "/pool/c.deb", "cccccccccc", true)
		convey.So(repo.byKey("/pool/b.deb"), convey.ShouldBeNil)

		convey.So(missReasonOf(pull(t, svc, "deb.debian.org", "/pool/b.deb")),
			convey.ShouldEqual, string(metrics.MissEvicted))
	})
}

// TestGet_MissReasonChanged 上游给出的 digest 和手上那份对不上时，归因是「内容变更」。
//
// 这才是站长要区分的东西：一个 tag 每小时都真的换了内容，和一个 TTL 配得太短、
// 每次回源都拉回一模一样的字节，在「回源次数」上完全一样，在该不该调 TTL 上却相反。
func TestGet_MissReasonChanged(t *testing.T) {
	convey.Convey("上游的 digest 和手上那份对不上时记成内容变更", t, func() {
		var changed atomic.Bool
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			content := "manifest v1"
			if changed.Load() {
				content = "manifest v2"
			}
			// registry 的 manifest 就是这么回答「你手上那份还是不是我这份」的。
			w.Header().Set("Docker-Content-Digest", digestOfString(content))
			_, _ = io.WriteString(w, content)
		})
		svc, repo, _ := setupSvc(t, o, staticUpstream("registry.example"), Options{})
		key := "/v2/library/redis/manifests/7"

		convey.So(missReasonOf(pull(t, svc, "registry.example", key)),
			convey.ShouldEqual, string(metrics.MissFirst))

		convey.Convey("上游换了内容", func() {
			changed.Store(true)
			repo.expire(key)
			convey.So(missReasonOf(pull(t, svc, "registry.example", key)),
				convey.ShouldEqual, string(metrics.MissChanged))
		})

		convey.Convey("上游还是那份内容，就只是 TTL 到了", func() {
			// 同一个 digest 回来，说明这趟回源什么都没换到——TTL 配得太短的
			// 典型样子。把它也记成「内容变更」，占比条就再也指不出该调什么了。
			repo.expire(key)
			convey.So(missReasonOf(pull(t, svc, "registry.example", key)),
				convey.ShouldEqual, string(metrics.MissTTL))
		})
	})
}

// TestGet_MissReasonCorruptedCopy 盘上的副本坏掉时丢弃并回源，这次回源不算「内容变更」。
func TestGet_MissReasonCorruptedCopy(t *testing.T) {
	convey.Convey("坏副本引起的回源不冒充内容变更", t, func() {
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "full content")
		})
		svc, repo, store := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})
		key := "/pool/broken.deb"

		pull(t, svc, "deb.debian.org", key)
		corruptBlob(t, store, repo, key)

		// 上游什么都没变，变的是我们自己的盘。记成内容变更会让站长去查上游，
		// 而真正该看的是那条校验失败的 error 日志与 katch_cache_integrity_failures_total。
		convey.So(missReasonOf(pull(t, svc, "deb.debian.org", key)),
			convey.ShouldEqual, string(metrics.MissFirst))
	})
}
