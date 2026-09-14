// Package cache 是缓存的磁盘存放层：对象本体按**内容寻址**存放，写入原子。
//
// 它只认字节，不认上游、不认路径，也不碰数据库——「哪个上游的哪条路径对应哪份
// 内容」是 cache_repo 那张表的事。两件事分开，这一层才能被单独验到「进程被杀之后
// 磁盘上剩下什么」。
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"strings"
)

// digestAlgorithm 内容寻址用的算法。摘要形如 sha256:<hex>，与 registry 的写法一致，
// 将来 registry 适配器可以直接拿上游给的 digest 来比对。
const digestAlgorithm = "sha256"

// Store 一个缓存目录。
//
// 目录里只有两个子目录：blobs/ 放已提交的内容，tmp/ 放正在下载的临时文件。
// 两者同在一个文件系统下，提交时的 rename 因此是原子的——跨文件系统的 rename
// 会退化成复制，那就不再有「要么完整要么没有」这个性质了。
type Store struct {
	blobs string
	tmp   string
}

// NewStore 打开（必要时创建）一个缓存目录。
//
// 目录建不出来就返回错误，由调用方降级为纯透传：缓存是优化，它坏掉不该让拉取
// 整体失败。这道判定放在启动时，而不是等第一次拉取才发现写不进去。
func NewStore(root string) (*Store, error) {
	s := &Store{
		blobs: filepath.Join(root, "blobs"),
		tmp:   filepath.Join(root, "tmp"),
	}
	for _, dir := range []string{s.blobs, s.tmp} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("cache: 缓存目录不可用：%w", err)
		}
	}
	// 上一次进程被杀时留下的临时文件在这里清掉。它们既不可能被读到（文件名不是
	// 内容摘要），也不会有人再来提交，留着就是每崩一次漏一份磁盘。
	if err := s.cleanTemp(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) cleanTemp() error {
	entries, err := os.ReadDir(s.tmp)
	if err != nil {
		return fmt.Errorf("cache: 读取临时目录失败：%w", err)
	}
	for _, e := range entries {
		if err := os.Remove(filepath.Join(s.tmp, e.Name())); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("cache: 清理临时文件失败：%w", err)
		}
	}
	return nil
}

// blobPath 摘要到路径。按摘要前两位分桶：一个目录下堆几十万个文件，
// 无论哪种文件系统的目录项查找都会开始变慢。
func (s *Store) blobPath(digest string) (string, error) {
	alg, hex, ok := strings.Cut(digest, ":")
	if !ok || alg != digestAlgorithm || len(hex) != 64 {
		return "", fmt.Errorf("cache: 不是合法的内容摘要：%q", digest)
	}
	for _, c := range hex {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", fmt.Errorf("cache: 不是合法的内容摘要：%q", digest)
		}
	}
	return filepath.Join(s.blobs, alg, hex[:2], hex), nil
}

// Has 返回这份内容在不在盘上，以及它的字节数。
func (s *Store) Has(digest string) (int64, bool) {
	path, err := s.blobPath(digest)
	if err != nil {
		return 0, false
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	return info.Size(), true
}

// Open 打开一份已提交的内容，同时给出它此刻在盘上的字节数。
//
// 字节数由调用方拿去和记录里的 size 比对：对不上说明这个副本已经坏了
// （磁盘或写入路径出了问题），该丢掉重新回源，而不是发给客户端。
func (s *Store) Open(digest string) (*os.File, int64, error) {
	path, err := s.blobPath(digest)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(path) // #nosec G304 -- 路径由内容摘要拼出，上面已校验字符集
	if err != nil {
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	return f, info.Size(), nil
}

// Remove 删掉一份内容。已经不在了不算错：淘汰和清理可能同时盯上同一份。
func (s *Store) Remove(digest string) error {
	path, err := s.blobPath(digest)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Create 开一个新的写入。内容先落进 tmp/，提交时才按摘要改名进 blobs/。
func (s *Store) Create() (*Writer, error) {
	f, err := os.CreateTemp(s.tmp, "dl-")
	if err != nil {
		return nil, fmt.Errorf("cache: 创建临时文件失败：%w", err)
	}
	return &Writer{store: s, file: f, hash: sha256.New()}, nil
}

// Writer 一次写入。
//
// 未提交的写入在磁盘上只是 tmp/ 下一个随机名字的文件：它的名字不是内容摘要，
// 因此任何读路径都不可能把它当成缓存命中——「半个文件被当成完整缓存」这件事
// 从命名规则上就不成立，而不是靠某处记得去清理（决策 8）。
type Writer struct {
	store     *Store
	file      *os.File
	hash      hash.Hash
	size      int64
	digest    string
	committed bool
	closed    bool
}

// Name 临时文件的路径。下载期间的读者直接 tail 这个文件，
// 从而不必等整份下载完成就能拿到首字节。
func (w *Writer) Name() string {
	return w.file.Name()
}

func (w *Writer) Write(p []byte) (int, error) {
	n, err := w.file.Write(p)
	if n > 0 {
		w.size += int64(n)
		// hash.Hash 的 Write 从不返回错误。
		_, _ = w.hash.Write(p[:n])
	}
	return n, err
}

// Commit 落盘并按内容摘要改名，返回摘要与字节数。
//
// 改名之前先 fsync：不同步就改名，机器掉电后可能出现「名字已经是内容摘要、
// 内容却还没写完」的文件——那正是决策 8 要防的那种会被当成完整缓存的半截货。
func (w *Writer) Commit() (string, int64, error) {
	if w.committed {
		return w.digest, w.size, nil
	}
	if err := w.file.Sync(); err != nil {
		return "", 0, fmt.Errorf("cache: 同步缓存文件失败：%w", err)
	}
	if err := w.file.Close(); err != nil {
		return "", 0, fmt.Errorf("cache: 关闭缓存文件失败：%w", err)
	}
	digest := digestAlgorithm + ":" + hex.EncodeToString(w.hash.Sum(nil))
	path, err := w.store.blobPath(digest)
	if err != nil {
		return "", 0, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", 0, fmt.Errorf("cache: 创建内容目录失败：%w", err)
	}
	// 已经有同样内容了就把临时文件丢掉：内容寻址的意义就是同一份只存一份。
	if _, err := os.Stat(path); err == nil {
		if err := os.Remove(w.file.Name()); err != nil && !os.IsNotExist(err) {
			return "", 0, err
		}
	} else if err := os.Rename(w.file.Name(), path); err != nil {
		return "", 0, fmt.Errorf("cache: 提交缓存文件失败：%w", err)
	}
	w.committed = true
	w.digest = digest
	return digest, w.size, nil
}

// Close 结束这次写入。没提交就把临时文件删掉——回源中途失败、客户端断开又碰上
// 上游也断了，都会走到这里，留着就是漏磁盘。
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if w.committed {
		return nil
	}
	err := w.file.Close()
	if rmErr := os.Remove(w.file.Name()); rmErr != nil && !os.IsNotExist(rmErr) {
		err = errors.Join(err, rmErr)
	}
	return err
}

// Digester 按同一套规则算内容摘要，供读路径校验副本用。
//
// 读缓存时要能问一句「盘上这份字节还是不是当初那份」，而算摘要的规则只应有一处
// 出处——写入和校验各写一遍，迟早会在换算法的那天分叉。
type Digester struct {
	h hash.Hash
}

// NewDigester 开一个摘要计算。
func NewDigester() *Digester {
	return &Digester{h: sha256.New()}
}

func (d *Digester) Write(p []byte) (int, error) {
	return d.h.Write(p)
}

// Digest 返回目前为止写进去的字节的摘要。
func (d *Digester) Digest() string {
	return digestAlgorithm + ":" + hex.EncodeToString(d.h.Sum(nil))
}
