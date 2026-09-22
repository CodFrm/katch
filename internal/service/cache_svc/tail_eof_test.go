package cache_svc

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"testing"
)

// TestGet_FullyDeliveredMissEndsWithEOFWhenTheClientHangsUp 声明的字节全交付了就是 EOF，
// 哪怕客户端在后台落库那一瞬挂断。
//
// pump 的次序是：字节发完 → Commit → 写缓存记录 → finish（置 done）。提交和落库要
// 落盘、写库，几十毫秒是常事，而客户端拿到 Content-Length 声明的最后一个字节之后就
// 可以关连接了。关掉的那一刻，追在后面的读者正停在「等新字节或等 done」上，于是最后
// 一次读拿到的是 ctx 取消而不是 io.EOF——转发处（web/embed.go 的 io.Copy）据此记一条
// 「转发响应体中断」。真机上约半数成功的 MISS 都带着这条 warn：字节一个不差，坏的是信号，
// 而值班时要靠它区分「客户端真的断了」和「一切正常」。
func TestGet_FullyDeliveredMissEndsWithEOFWhenTheClientHangsUp(t *testing.T) {
	const body = "一个完整交付的对象\n"
	o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = io.WriteString(w, body)
	})
	svc, repo, _ := setupSvc(t, o, staticUpstream("deb.debian.org"), Options{})

	// 把 pump 停在「字节全发完了、记录还没写完」这一步。
	gate := make(chan struct{})
	repo.mu.Lock()
	repo.saveGate = gate
	repo.mu.Unlock()
	defer close(gate)

	ctx, cancel := context.WithCancel(context.Background())
	reader, meta, err := svc.Get(ctx, target("deb.debian.org", "/pool/main/h/hangup.deb"))
	if err != nil {
		t.Fatalf("拉取失败：%v", err)
	}
	defer func() { _ = reader.Close() }()
	if meta.ContentLength != int64(len(body)) {
		t.Fatalf("Content-Length = %d，要的是 %d", meta.ContentLength, len(body))
	}
	payload := make([]byte, len(body))
	if _, err := io.ReadFull(reader, payload); err != nil {
		t.Fatalf("读完声明的字节失败：%v", err)
	}
	if string(payload) != body {
		t.Fatalf("内容不符：%q", payload)
	}

	// 客户端收齐了就挂断——这在 HTTP 上完全正常，它不欠这次响应任何东西。
	cancel()
	n, err := reader.Read(make([]byte, 1))
	if n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("交付完整的响应最后一次读拿到的是 (%d, %v)，要的是 (0, EOF)——"+
			"这一条会让一次成功的转发被记成「转发响应体中断」", n, err)
	}
}
