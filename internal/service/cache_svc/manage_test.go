package cache_svc

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/smartystreets/goconvey/convey"
)

// putObject 借 Put 把一个对象塞进缓存，供淘汰与管理用例摆场景。
func putObject(t *testing.T, svc CacheSvc, key, content string, immutable bool) {
	t.Helper()
	err := svc.Put(context.Background(), &PutRequest{
		UpstreamID: 7,
		Key:        key,
		Content:    strings.NewReader(content),
		Immutable:  immutable,
	})
	if err != nil {
		t.Fatalf("写入缓存失败：%v", err)
	}
}

// TestEviction_EvictsLeastRecentlyUsedOverQuota 目标的第四条：超配额时淘汰最久未用的
// 未 pin 不可变对象。
//
// 「最久未用」的先后由命中顺序决定：先写 A、B、C，再命中一次 A，于是 B 成了最久未用的
// 那一个——淘汰按插入顺序做的实现会在这里选中 A，恰好相反。
func TestEviction_EvictsLeastRecentlyUsedOverQuota(t *testing.T) {
	convey.Convey("超配额时淘汰最久未访问的对象", t, func() {
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "0123456789")
		})
		// 配额 25 字节、回收到 80%（20 字节）：写进第三个 10 字节的对象就得腾地方。
		svc, repo, store := setupSvc(t, o, staticUpstream("deb.debian.org"),
			Options{Runtime: newFakeRuntime(t, quotaOf(25, 80))})

		putObject(t, svc, "/pool/a.deb", "aaaaaaaaaa", true)
		putObject(t, svc, "/pool/b.deb", "bbbbbbbbbb", true)
		// 命中一次 A：它于是比 B 新。
		r, _, err := svc.Get(context.Background(), target("deb.debian.org", "/pool/a.deb"))
		convey.So(err, convey.ShouldBeNil)
		_, _ = io.ReadAll(r)
		convey.So(r.Close(), convey.ShouldBeNil)

		putObject(t, svc, "/pool/c.deb", "cccccccccc", true)

		convey.So(repo.byKey("/pool/b.deb"), convey.ShouldBeNil)
		convey.So(repo.byKey("/pool/a.deb"), convey.ShouldNotBeNil)
		convey.So(repo.byKey("/pool/c.deb"), convey.ShouldNotBeNil)
		// 记录没了，盘上的内容也要跟着没，否则缓存目录只增不减。
		_, ok := store.Has(digestOfString("bbbbbbbbbb"))
		convey.So(ok, convey.ShouldBeFalse)
		_, ok = store.Has(digestOfString("aaaaaaaaaa"))
		convey.So(ok, convey.ShouldBeTrue)
	})
}

// TestEviction_SkipsPinnedAndMutable 淘汰只发生在不可变且未被 pin 的对象上：
// 可变对象由 TTL 自行过期，pin 的对象是人明确要求留下的。
func TestEviction_SkipsPinnedAndMutable(t *testing.T) {
	convey.Convey("pin 的与可变的对象不参与淘汰", t, func() {
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "0123456789")
		})
		svc, repo, _ := setupSvc(t, o, staticUpstream("deb.debian.org"),
			Options{Runtime: newFakeRuntime(t, quotaOf(25, 80))})

		putObject(t, svc, "/pool/pinned.deb", "pppppppppp", true)
		convey.So(svc.Pin(context.Background(), &PinRequest{
			ID: repo.byKey("/pool/pinned.deb").ID, Pinned: true}), convey.ShouldBeNil)
		putObject(t, svc, "/dists/mutable", "mmmmmmmmmm", false)
		putObject(t, svc, "/pool/evictable.deb", "eeeeeeeeee", true)

		// 总量 30 > 25，但唯一能淘汰的只有那条不可变且未 pin 的记录。
		convey.So(repo.byKey("/pool/evictable.deb"), convey.ShouldBeNil)
		convey.So(repo.byKey("/pool/pinned.deb"), convey.ShouldNotBeNil)
		convey.So(repo.byKey("/dists/mutable"), convey.ShouldNotBeNil)
	})
}

// TestEviction_KeepsSharedContent 内容寻址的直接后果：淘汰一条记录时，
// 若还有别的记录指着同一份内容，盘上的文件不能删。
func TestEviction_KeepsSharedContent(t *testing.T) {
	convey.Convey("还有别的记录引用同一份内容时不删文件", t, func() {
		o := newOrigin(t, func(http.ResponseWriter, *http.Request) {})
		svc, repo, store := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})

		putObject(t, svc, "/pool/one.deb", "same content", true)
		putObject(t, svc, "/pool/two.deb", "same content", true)

		_, err := svc.Purge(context.Background(), &PurgeRequest{ID: repo.byKey("/pool/one.deb").ID})
		convey.So(err, convey.ShouldBeNil)
		convey.So(repo.byKey("/pool/one.deb"), convey.ShouldBeNil)
		_, ok := store.Has(digestOfString("same content"))
		convey.So(ok, convey.ShouldBeTrue)

		convey.Convey("最后一条引用被删掉时文件才删", func() {
			_, err := svc.Purge(context.Background(), &PurgeRequest{ID: repo.byKey("/pool/two.deb").ID})
			convey.So(err, convey.ShouldBeNil)
			_, ok := store.Has(digestOfString("same content"))
			convey.So(ok, convey.ShouldBeFalse)
		})
	})
}

func TestPurge_ByUpstream(t *testing.T) {
	convey.Convey("按上游清缓存", t, func() {
		o := newOrigin(t, func(http.ResponseWriter, *http.Request) {})
		svc, repo, store := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})

		putObject(t, svc, "/pool/a.deb", "aaa", true)
		putObject(t, svc, "/pool/b.deb", "bbb", true)

		resp, err := svc.Purge(context.Background(), &PurgeRequest{UpstreamID: 7})
		convey.So(err, convey.ShouldBeNil)
		convey.So(resp.Removed, convey.ShouldEqual, 2)
		convey.So(len(repo.all()), convey.ShouldEqual, 0)
		_, ok := store.Has(digestOfString("aaa"))
		convey.So(ok, convey.ShouldBeFalse)
	})
}

func TestSearch_ByUpstream(t *testing.T) {
	convey.Convey("搜索缓存对象", t, func() {
		o := newOrigin(t, func(http.ResponseWriter, *http.Request) {})
		svc, _, _ := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})

		putObject(t, svc, "/pool/redis.deb", "aaa", true)

		resp, err := svc.Search(context.Background(), &SearchRequest{UpstreamID: 7, Page: 1, Size: 10})
		convey.So(err, convey.ShouldBeNil)
		convey.So(resp.Total, convey.ShouldEqual, 1)
		convey.So(len(resp.List), convey.ShouldEqual, 1)
		convey.So(resp.List[0].Key, convey.ShouldEqual, "/pool/redis.deb")
	})
}

// TestPut_RejectsIncompleteRequest 键为空或没有内容的写入要当场拒绝：
// 前者会写出一条既查不到也淘汰不掉的记录，后者会直接在拷贝时崩掉。
func TestPut_RejectsIncompleteRequest(t *testing.T) {
	convey.Convey("缺少键或内容时拒绝写入", t, func() {
		o := newOrigin(t, func(http.ResponseWriter, *http.Request) {})
		svc, repo, _ := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})

		convey.So(svc.Put(context.Background(), &PutRequest{
			UpstreamID: 7, Content: strings.NewReader("x")}), convey.ShouldNotBeNil)
		convey.So(svc.Put(context.Background(), &PutRequest{
			UpstreamID: 7, Key: "/pool/x.deb"}), convey.ShouldNotBeNil)
		convey.So(len(repo.all()), convey.ShouldEqual, 0)
	})
}

// TestPut_WithoutStoreFails 没有磁盘时 Put 要明确失败，而不是假装写成功——
// 拉取路径可以降级为透传，但「我把它缓存了」这个回答不能是假的。
func TestPut_WithoutStoreFails(t *testing.T) {
	convey.Convey("缓存不可用时 Put 报错", t, func() {
		o := newOrigin(t, func(http.ResponseWriter, *http.Request) {})
		_, _, _ = setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})
		err := New(nil, Options{}).Put(context.Background(), &PutRequest{
			UpstreamID: 7, Key: "/pool/x.deb", Content: strings.NewReader("x"),
		})
		convey.So(err, convey.ShouldNotBeNil)
	})
}

// TestPurge_SkipsPinnedInBatch pin 的对象不参与按上游的批量清除：pin 表达的是
// 「这份内容要常驻」，它既挡住了 LRU 淘汰，也该挡住一次「清掉这个上游」的操作——
// 否则运维清一次缓存就把所有 pin 过的对象顺手抹了，而界面上看不出发生过这件事。
//
// 按 id 清单条不受这条约束：那是人指着这一条说「就清它」，见 Purge 的实现。
func TestPurge_SkipsPinnedInBatch(t *testing.T) {
	convey.Convey("按上游清缓存时跳过 pin 过的对象", t, func() {
		o := newOrigin(t, func(http.ResponseWriter, *http.Request) {})
		svc, repo, store := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})

		putObject(t, svc, "/pool/keep.deb", "keep", true)
		putObject(t, svc, "/pool/drop.deb", "drop", true)
		convey.So(svc.Pin(context.Background(), &PinRequest{
			ID: repo.byKey("/pool/keep.deb").ID, Pinned: true}), convey.ShouldBeNil)

		resp, err := svc.Purge(context.Background(), &PurgeRequest{UpstreamID: 7})
		convey.So(err, convey.ShouldBeNil)
		convey.So(resp.Removed, convey.ShouldEqual, 1)
		// 跳过的条数要报出去，否则界面只能说「清了 1 条」，看不出还有 1 条没清。
		convey.So(resp.Skipped, convey.ShouldEqual, 1)
		convey.So(repo.byKey("/pool/drop.deb"), convey.ShouldBeNil)
		convey.So(repo.byKey("/pool/keep.deb"), convey.ShouldNotBeNil)
		// 记录留下了，盘上那份内容也必须留着，否则命中时会读到一个空文件。
		_, ok := store.Has(digestOfString("keep"))
		convey.So(ok, convey.ShouldBeTrue)
	})
}

// TestPin_RejectsMissingObject pin 一个不存在的 id 不能当作成功：SetPinned 更新
// 0 行也返回 nil，直接透传等于对着一个空操作回 200。
func TestPin_RejectsMissingObject(t *testing.T) {
	convey.Convey("pin 不存在的对象时报错", t, func() {
		o := newOrigin(t, func(http.ResponseWriter, *http.Request) {})
		svc, _, _ := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})
		convey.So(svc.Pin(context.Background(), &PinRequest{ID: 404, Pinned: true}),
			convey.ShouldNotBeNil)
	})
}
