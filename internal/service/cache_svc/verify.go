package cache_svc

import (
	"errors"
	"io"
	"os"

	"github.com/CodFrm/katch/internal/cache"
	"github.com/CodFrm/katch/internal/model/entity/cache_entity"
)

// ErrCacheCorrupted 盘上的副本和记录对不上。
var ErrCacheCorrupted = errors.New("缓存副本已损坏")

// verifyCopy 在把副本发出去**之前**核对它的完整性，核对完把读取位置拨回开头。
//
// 校验必须发生在第一个字节交出去之前，而不是一边发一边算、到末尾再报错：
// 规格要的是「校验失败时丢弃该副本并回源」，而一旦字节已经发出去，这一次请求
// 就回不了源了。更糟的是「大小一样、内容被改写」这一类损坏——响应是 200、
// Content-Length 还对得上，客户端拿到的是一个看起来完全正常的完整坏响应，
// 它没有任何理由去重试，于是这份坏副本会一直发到有人手工去清缓存为止。
//
// 代价是命中时多读一遍这个文件（第二遍基本落在页缓存上）。这是刻意换的：
// 规格里那条「首字节不必等整份下载完成」说的是**未命中**那条路径，
// 命中这一侧没有对应的约定，而「绝不发出已知是坏的字节」有。
//
// 只按摘要校验：内容寻址意味着每条记录都带着写入时算出的 sha256，
// 「无 digest 的按大小校验」那一支在这里不可能出现——大小在打开时已经比过了。
func verifyCopy(file *os.File, object *cache_entity.CacheObject) error {
	hash := cache.NewDigester()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if hash.Digest() != object.Digest {
		return ErrCacheCorrupted
	}
	// 拨回开头给真正的读取用。拨不回去就当这份副本不可用——交出一个读取位置
	// 停在末尾的文件，客户端会收到一个长度对得上却空无一物的响应。
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return nil
}
