package cache_svc

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"sync"

	"github.com/CodFrm/katch/internal/cache"
	"github.com/CodFrm/katch/internal/metrics"
	"github.com/CodFrm/katch/internal/service/proxy_svc"
)

// errNotCoalescable 这次下载搭不上顺风车，调用方自己回源去。
//
// 它不是故障，而是「合并不成立」的正常出口：上游给的是 404、是 206、或者盘写不了，
// 这些响应都不该进缓存，也就没有一份可以被多个客户端共读的副本。
var errNotCoalescable = errors.New("cache: 这次回源不参与合并")

// flight 一次正在进行的回源下载，供同一对象的并发请求共读（决策 9）。
//
// 形态是「一个后台协程往临时文件里写，若干个读者跟在后面读」，而不是「谁先来谁
// 负责边下边发、其余人等他」：后者一旦那个客户端断开，下载就跟着断了，而这份下载
// 是所有等待者共用的。写在文件上还顺带满足了另外两条要求——读者不必等整份下载完成
// 就能拿到首字节，客户端全部断开之后下载也照样跑完。
type flight struct {
	store *cache.Store
	// ready 在元信息（状态码、是否可缓存）确定之后关闭。
	ready     chan struct{}
	meta      *proxy_svc.Meta
	startErr  error
	cacheable bool
	tmpPath   string

	// mu 护住下面这组进度状态，cond 用来叫醒追到文件末尾的读者。
	mu      sync.Mutex
	cond    *sync.Cond
	written int64
	done    bool
	failed  error
	digest  string
}

func newFlight(store *cache.Store) *flight {
	f := &flight{store: store, ready: make(chan struct{})}
	f.cond = sync.NewCond(&f.mu)
	return f
}

// startFailed 回源本身就没成功，等在这次下载上的请求一并拿到同一个错误。
//
// 让等待者跟着快速失败，而不是各自再去打一次上游：上游此刻不可达时，把并发原样
// 放大过去只会让它更不可达。
func (f *flight) startFailed(err error) {
	f.startErr = err
	close(f.ready)
}

// startUncacheable 这次响应不该进缓存，等待者各自回源。
func (f *flight) startUncacheable() {
	close(f.ready)
}

// start 元信息就绪，开始对外提供共读。
func (f *flight) start(meta *proxy_svc.Meta, tmpPath string) {
	f.meta = meta
	f.tmpPath = tmpPath
	f.cacheable = true
	close(f.ready)
}

func (f *flight) publish(n int64) {
	f.mu.Lock()
	f.written += n
	f.cond.Broadcast()
	f.mu.Unlock()
}

// attach 接上这次下载，拿到一个跟读的响应体。
func (f *flight) attach(ctx context.Context) (io.ReadCloser, *proxy_svc.Meta, error) {
	select {
	case <-f.ready:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	if f.startErr != nil {
		return nil, nil, f.startErr
	}
	if !f.cacheable {
		return nil, nil, errNotCoalescable
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	// 下载已经收尾：临时文件已经按内容摘要改名，直接从内容寻址的位置读。
	if f.done {
		if f.digest == "" {
			return nil, nil, errNotCoalescable
		}
		file, _, err := f.store.Open(f.digest)
		if err != nil {
			return nil, nil, errNotCoalescable
		}
		return file, f.metaFor(cacheStatusHit), nil
	}
	file, err := os.Open(f.tmpPath) // #nosec G304 -- 路径由 store 自己造的临时文件给出
	if err != nil {
		return nil, nil, errNotCoalescable
	}
	return newTailReader(ctx, f, file), f.metaFor(cacheStatusMiss), nil
}

// finish 收尾：done 之后读者才会读到 EOF。
//
// 提交与记录落库都在 done 之前完成，于是「客户端收完了这次响应」就蕴含
// 「这个对象已经在缓存里」——不然紧接着的第二次拉取会看见一个还没写完的缓存。
func (f *flight) finish(digest string, failure error) {
	f.mu.Lock()
	f.digest = digest
	f.failed = failure
	f.done = true
	f.cond.Broadcast()
	f.mu.Unlock()
}

// metaFor 给每个读者一份自己的元信息：响应头会被各自的处理器接着写，共享同一张
// map 会在并发下互相打架。
func (f *flight) metaFor(status string) *proxy_svc.Meta {
	header := make(http.Header, len(f.meta.Header)+1)
	for k, v := range f.meta.Header {
		header[k] = append([]string(nil), v...)
	}
	header.Set(cacheStatusHeader, status)
	if status == cacheStatusHit {
		header.Del(metrics.MissHeader)
	}
	return &proxy_svc.Meta{
		StatusCode:    f.meta.StatusCode,
		Header:        header,
		ContentLength: f.meta.ContentLength,
	}
}

// tailReader 跟在下载后面读同一个文件。
//
// 读到文件当前末尾就等，等到有新字节或下载收尾为止；它自己被关掉不会影响下载，
// 客户端断开时正是走这条路（已下载的部分仍要写完缓存）。
type tailReader struct {
	ctx  context.Context
	f    *flight
	file *os.File
	off  int64
	stop chan struct{}
	once sync.Once
}

func newTailReader(ctx context.Context, f *flight, file *os.File) *tailReader {
	r := &tailReader{ctx: ctx, f: f, file: file, stop: make(chan struct{})}
	// 客户端断开时把等在 cond 上的这个读者叫醒：cond.Wait 自己不认识 context，
	// 少了这个看门协程，一个已经走掉的客户端会一直占着处理器直到下载结束。
	go func() {
		select {
		case <-ctx.Done():
			f.mu.Lock()
			f.cond.Broadcast()
			f.mu.Unlock()
		case <-r.stop:
		}
	}()
	return r
}

func (r *tailReader) Read(p []byte) (int, error) {
	f := r.f
	f.mu.Lock()
	for r.off >= f.written && !f.done {
		if err := r.ctx.Err(); err != nil {
			f.mu.Unlock()
			return 0, err
		}
		f.cond.Wait()
	}
	avail := f.written - r.off
	failed := f.failed
	f.mu.Unlock()

	if avail <= 0 {
		if failed != nil {
			return 0, failed
		}
		return 0, io.EOF
	}
	if int64(len(p)) > avail {
		p = p[:avail]
	}
	n, err := r.file.Read(p)
	r.off += int64(n)
	return n, err
}

func (r *tailReader) Close() error {
	r.once.Do(func() { close(r.stop) })
	return r.file.Close()
}
