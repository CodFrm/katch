package git_svc

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/go-git/go-billy/v5"
)

// bounds 一次建镜像的边界闸。
//
// 越界之后它让**盘上的读写**报错，而不是取消一个 context：go-git 的 delta
// 解析阶段整个不看 ctx（remote.go 的 packfile.UpdateObjectStorage 一路到
// plumbing/format/packfile/common.go 都没有 ctx 参数），基于取消的闸在那一段
// 上形同虚设，而实测的卡死正发生在那里。能中止它的只有让它正在写的文件或
// 正在读的文件报错。
//
// 闸一旦合上就不再打开：写侧停下之后，与下载并发跑着的那个解析 goroutine
// 还在读同一个临时 pack，它要在下一次读上就地结束，而不是把已经落盘的那段
// 解析完再说。
type bounds struct {
	mu sync.Mutex
	// maxBytes 这份镜像最多能往盘上写多少字节，0 表示不限。
	maxBytes int64
	// stall 上游多久不送字节算停滞，0 表示不限。
	stall time.Duration
	// total 整段墙钟的上限，0 表示不限。
	total time.Duration
	// startedAt 这趟建镜像是什么时候开始计时的。
	startedAt time.Time
	// lastByte 最后一次收到上游字节的时刻。
	lastByte time.Time
	// fetched fetch 阶段已经收工。收工之后停滞闸不再计时：解析阶段网络上本来
	// 就静默而 CPU 满载（实测那 19 分钟），把它当成一次停滞是误杀（决策 2）。
	fetched bool
	// written 已经写进去多少。算的是累计写入而不是当下的目录体积：闸要在
	// 字节落盘**之前**判，事后去量就又回到了「先下完再说」。
	written int64
	// tripped 闸合上时的原因，nil 表示还开着。
	tripped error
	// refusedReads 越界之后被拦下的那些读，记的是文件名。
	//
	// 留着它是为了能断言「解析 goroutine 是被读侧掐断的」：只看建镜像返回了
	// 什么，分不出它是当场结束还是等解析跑完才结束。
	refusedReads []string
}

func newBounds(maxBytes int64, stall, total time.Duration) *bounds {
	now := time.Now()
	return &bounds{
		maxBytes: maxBytes, stall: stall, total: total,
		startedAt: now, lastByte: now,
	}
}

// reserve 为一次写入要 n 个字节的额度，要不到就合闸。
//
// 越界的那一次写整块都不落盘，而不是写够上限再停：盘上峰值因此顶多是上限
// 加调用方手里那一个缓冲，而不是上限加一整块。
func (b *bounds) reserve(n int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tripped != nil {
		return b.tripped
	}
	if b.maxBytes > 0 && b.written+n > b.maxBytes {
		b.tripped = fmt.Errorf("%w：已收到 %d 字节，超过 %d 字节的上限",
			ErrRepoTooLarge, b.written+n, b.maxBytes)
		return b.tripped
	}
	b.written += n
	// 落盘的这些字节就是刚从上游收来的那些：一次 clone 的字节几乎全在那个临时
	// pack 上，go-git 是边收边写的（io.Copy 从连接读、往 PackWriter 写）。
	// 停滞闸据此知道上游还在说话。
	b.lastByte = time.Now()
	return nil
}

// trip 从外面把闸合上，闸已经合过就不动它。
//
// 两道时间闸走的就是这里：合上之后盘上的读写一起报错，和体积闸落在同一道闸上。
// 这是唯一一条在 go-git 的 delta 解析阶段仍然有效的路——那一段整个不看 ctx。
func (b *bounds) trip(cause error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tripped == nil {
		b.tripped = cause
	}
}

// fetchDone fetch 阶段收工，停滞闸从此不再计时。
func (b *bounds) fetchDone() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fetched = true
}

// overdue 此刻有没有哪一道时间闸该合上了，没有就给 nil。
func (b *bounds) overdue(now time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tripped != nil {
		return nil
	}
	// 总时限先判：两道同时到点时，「整体太久」比「刚好这一小段没字节」更说明
	// 问题——站长该调的是总时限那一项。
	if b.total > 0 && now.Sub(b.startedAt) >= b.total {
		return fmt.Errorf("%w：建镜像已经跑了 %s，超过 %s 的总时限",
			ErrBuildTimedOut, now.Sub(b.startedAt).Round(time.Millisecond), b.total)
	}
	if b.stall > 0 && !b.fetched && now.Sub(b.lastByte) >= b.stall {
		return fmt.Errorf("%w：已经 %s 没有收到上游的字节，超过 %s 的停滞时限",
			ErrBuildStalled, now.Sub(b.lastByte).Round(time.Millisecond), b.stall)
	}
	return nil
}

// watchTick 看门狗多久看一眼。时限本身可以很长（出厂 120/1800 秒），
// 但用例里是毫秒级的，取十分之一既不让它空转，也不让它迟到太多。
func watchTick(stall, total time.Duration) time.Duration {
	tick := total
	if stall > 0 && (tick == 0 || stall < tick) {
		tick = stall
	}
	tick /= 10
	if tick > time.Second {
		return time.Second
	}
	if tick < time.Millisecond {
		return time.Millisecond
	}
	return tick
}

// watch 起看门狗，到点就合闸。返回的函数把它停掉并等它收工。
//
// wake 用来叫醒一次**阻塞在上游连接上的读**：闸合上之后盘上的读写会立刻报错，
// 但一次停在网络 Read 上的 io.Copy 在闸这一侧根本看不见（与它并发的解析
// goroutine 此时也停在 syncedReader 的 <-news 上），只有取消能把它叫回来。
// 取消是叫醒用的，不是判据：是哪一道闸、以及解析阶段的中止，都由闸自己说了算。
func (b *bounds) watch(wake func()) func() {
	if b.stall <= 0 && b.total <= 0 {
		return func() {}
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(watchTick(b.stall, b.total))
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-ticker.C:
				if cause := b.overdue(now); cause != nil {
					b.trip(cause)
					wake()
					return
				}
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

// maxRefusedReads 最多记多少个被拦下的读。
//
// 记的是「读侧确实把闸传到了解析那一侧」这件事，几个名字就够说明问题；
// 闸合上之后到调用方收手之间还会有多少次读由 go-git 说了算，不封顶的话
// 这个切片的长度就由上游的字节流决定。
const maxRefusedReads = 8

// allowRead 闸还开着才让读下去。
func (b *bounds) allowRead(name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tripped == nil {
		return nil
	}
	if len(b.refusedReads) < maxRefusedReads {
		b.refusedReads = append(b.refusedReads, name)
	}
	return b.tripped
}

// err 闸合上的原因，还开着时给 nil。
func (b *bounds) err() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tripped
}

// refusedReadNames 越界之后被拦下的读都发生在哪些文件上。
func (b *bounds) refusedReadNames() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.refusedReads...)
}

// boundedFS 把一个 billy 文件系统套在闸后面。
//
// 套在文件系统这一层而不是套在 storer 上：go-git 的对象存储实现了
// storer.PackfileWriter，一次 clone 的字节几乎全都是那个临时 pack 文件，
// 而那个文件是 dotgit 自己开的，只有文件系统这一层拦得住它；同一层也拦得住
// 解析 goroutine 对它的读。
type boundedFS struct {
	billy.Filesystem
	bounds *bounds
}

func newBoundedFS(inner billy.Filesystem, b *bounds) *boundedFS {
	return &boundedFS{Filesystem: inner, bounds: b}
}

func (f *boundedFS) Create(filename string) (billy.File, error) {
	return f.wrap(f.Filesystem.Create(filename))
}

func (f *boundedFS) Open(filename string) (billy.File, error) {
	return f.wrap(f.Filesystem.Open(filename))
}

func (f *boundedFS) OpenFile(filename string, flag int, perm os.FileMode) (billy.File, error) {
	return f.wrap(f.Filesystem.OpenFile(filename, flag, perm))
}

func (f *boundedFS) TempFile(dir, prefix string) (billy.File, error) {
	return f.wrap(f.Filesystem.TempFile(dir, prefix))
}

func (f *boundedFS) Chroot(path string) (billy.Filesystem, error) {
	inner, err := f.Filesystem.Chroot(path)
	if err != nil {
		return nil, err
	}
	return newBoundedFS(inner, f.bounds), nil
}

// Chmod 透传下去。dotgit 把落地的 pack 与 idx 改成 0444，它是按 billy.Chmod
// 探测的——包一层之后不再透传的话，那两个文件会悄悄变成可写的。
func (f *boundedFS) Chmod(name string, mode os.FileMode) error {
	chmod, ok := f.Filesystem.(billy.Chmod)
	if !ok {
		return billy.ErrNotSupported
	}
	return chmod.Chmod(name, mode)
}

func (f *boundedFS) wrap(file billy.File, err error) (billy.File, error) {
	if err != nil {
		return nil, err
	}
	return &boundedFile{File: file, bounds: f.bounds}, nil
}

// boundedFile 闸后面的一个文件。
type boundedFile struct {
	billy.File
	bounds *bounds
}

func (f *boundedFile) Write(p []byte) (int, error) {
	if err := f.bounds.reserve(int64(len(p))); err != nil {
		return 0, err
	}
	return f.File.Write(p)
}

// Read 闸合上之后就报错。
//
// 光把写侧停下不够：go-git 的 PackWriter 在构造时就起了一个解析 goroutine
// （dotgit/writers.go 的 go writer.buildIndex），它和下载并发地读着同一个临时
// pack，收尾时建镜像要等它跑完。不在读上报错的话，闸只是让它少收了后面的
// 字节，已经落盘的那一段照旧要解析到底——实测卡死的正是这一段，而它不看 ctx。
func (f *boundedFile) Read(p []byte) (int, error) {
	if err := f.bounds.allowRead(f.Name()); err != nil {
		return 0, err
	}
	return f.File.Read(p)
}

// ReadAt 同 Read。解析阶段按偏移取对象走的是这一条。
func (f *boundedFile) ReadAt(p []byte, off int64) (int, error) {
	if err := f.bounds.allowRead(f.Name()); err != nil {
		return 0, err
	}
	return f.File.ReadAt(p, off)
}

// fetchWriter 包住 go-git 收 pack 用的那个 writer，只为知道 fetch 阶段什么时候
// 结束。
//
// 结束的时刻就是这个 Close：WritePackfileToObjectStorage 先 io.Copy 把上游的
// 字节收完，再 defer 关掉它（plumbing/format/packfile/common.go）。这个 Close
// 自己会一路等到解析跑完，所以停滞闸必须在**进去之前**收工，否则那段只读不写
// 的解析会被它当成一次停滞误杀（决策 2）。
type fetchWriter struct {
	io.WriteCloser
	bounds *bounds
}

func (w *fetchWriter) Close() error {
	w.bounds.fetchDone()
	return w.WriteCloser.Close()
}
