package cache_svc

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/CodFrm/katch/internal/repository/cache_repo"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
)

// 这两条守的是**用例夹具自己**，不是被测代码。
//
// 起因是一次 CI 上的红：cache_svc 的一个后台 pump 协程还停在 enforceQuota 里
// （它在那里读进程全局的 cache_repo），下一个用例的 setupSvc 已经在改同一个全局，
// race detector 当场抓到。TestQuiesce_WaitsForBackgroundCacheWrite 的注释里早就
// 把这个形状写下来了——Quiesce 本来就是为它造的，只是夹具一直没用上。
//
// 这类缺陷只在 CI 那台机器的调度下才现形（darwin/arm64 上怎么跑都是绿的），
// 所以不去追那个时序，而是把「夹具必须把自己收干净」写成两条不依赖运气的用例。

// TestHarness_RestoresProcessGlobalsAfterTest setupSvc 必须把它改过的进程全局还原。
//
// cache_repo 和 upstream_repo 的注册都是进程级的一份。夹具只写不还原的话，用例之间
// 就靠「谁后跑谁说了算」联系在一起：前一个用例留下的后台协程会拿着**后一个**用例的
// 仓储去读写，而两边的断言各自看起来都还成立。internal/web 那边的夹具是还原的
// （cache_test.go 里的 t.Cleanup(RegisterCacheObject(nil))），这里漏了。
func TestHarness_RestoresProcessGlobalsAfterTest(t *testing.T) {
	beforeCache := cache_repo.CacheObject()
	beforeUpstream := upstream_repo.Upstream()

	t.Run("用一次完整的夹具", func(st *testing.T) {
		o := newOrigin(st, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "full content")
		})
		svc, _, _ := setupSvc(st, o, staticUpstream("deb.debian.org"), Options{})
		r, _, err := svc.Get(context.Background(), target("deb.debian.org", "/pool/x.deb"))
		if err != nil {
			st.Fatalf("回源失败：%v", err)
		}
		if _, err := io.ReadAll(r); err != nil {
			st.Fatalf("读响应体失败：%v", err)
		}
		if err := r.Close(); err != nil {
			st.Fatalf("关闭响应体失败：%v", err)
		}
	})

	if got := cache_repo.CacheObject(); got != beforeCache {
		t.Error("用例结束了，cache_repo 还指着它注册的那个 mock——" +
			"下一个用例的后台协程会写进这份已经没人看的表里")
	}
	if got := upstream_repo.Upstream(); got != beforeUpstream {
		t.Error("用例结束了，upstream_repo 还指着它注册的那一份")
	}
}

// TestHarness_WaitsForBackgroundWorkBeforeTestEnds 用过 setupSvc 的用例，
// 在它起的后台下载收尾之前不能结束。
//
// 缓存写入是脱离客户端跑的（客户端断开也要写完），所以「客户端收完了」并不等于
// 「这趟活干完了」：pump 在 finish 之后还要跑一趟 enforceQuota，而那里读的是进程
// 全局的 cache_repo。用例就此结束的话，那个协程会活到下一个用例里，拿着下一个
// 用例的全局仓储接着干——CI 上的 DATA RACE 就是这么来的。
//
// 按住的是 TotalSize：它只有 enforceQuota 一个调用方，而 enforceQuota 只跑在 pump
// 上，所以这是「后台这件事还没做完」唯一一个不靠睡眠就按得住的缝（见 fakeRepo）。
func TestHarness_WaitsForBackgroundWorkBeforeTestEnds(t *testing.T) {
	gate := make(chan struct{})
	subDone := make(chan struct{})

	go func() {
		defer close(subDone)
		t.Run("后台回收被按住时", func(st *testing.T) {
			o := newOrigin(st, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "full content")
			})
			svc, repo, _ := setupSvc(st, o, staticUpstream("deb.debian.org"), Options{})
			repo.mu.Lock()
			repo.totalSizeGate = gate
			repo.mu.Unlock()

			r, _, err := svc.Get(context.Background(), target("deb.debian.org", "/pool/slow.deb"))
			if err != nil {
				st.Fatalf("回源失败：%v", err)
			}
			// 读到 EOF 就意味着字节已经提交、记录已经落库（pump 里 saveRecord 排在
			// finish 之前）。客户端这一侧到此为止，但 pump 还欠一趟 enforceQuota。
			if _, err := io.ReadAll(r); err != nil {
				st.Fatalf("读响应体失败：%v", err)
			}
			if err := r.Close(); err != nil {
				st.Fatalf("关闭响应体失败：%v", err)
			}
		})
	}()

	select {
	case <-subDone:
		close(gate)
		t.Fatal("后台协程还停在配额回收上，用例就结束了——" +
			"它会活到下一个用例里去读写同一份进程全局")
	case <-time.After(200 * time.Millisecond):
		// 正是要的：用例卡在 Cleanup 的 Quiesce 上等着。
	}

	close(gate)
	select {
	case <-subDone:
	case <-time.After(10 * time.Second):
		t.Fatal("闸放开之后用例仍然没结束")
	}
}
