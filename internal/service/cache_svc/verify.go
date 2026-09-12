package cache_svc

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/cago-frame/cago/pkg/logger"
	"go.uber.org/zap"

	"github.com/CodFrm/katch/internal/cache"
	"github.com/CodFrm/katch/internal/model/entity/cache_entity"
)

// ErrCacheCorrupted 盘上的副本和记录对不上。
var ErrCacheCorrupted = errors.New("缓存副本已损坏")

// verifyReader 读缓存副本时顺带校验完整性。
//
// 大小在打开时就比过了（对不上根本不会走到这里），这里补的是「大小一样、内容被
// 改写」那一类：一路算摘要，读到末尾再比对。发现不符就丢掉这份副本并报错，
// 让客户端重试时回源——静默把坏字节发出去，之后每次命中都会重复同一个错误。
type verifyReader struct {
	ctx    context.Context
	svc    *cacheSvc
	file   *os.File
	object *cache_entity.CacheObject
	hash   *cache.Digester
	read   int64
}

func newVerifyReader(ctx context.Context, svc *cacheSvc, file *os.File, object *cache_entity.CacheObject) io.ReadCloser {
	return &verifyReader{ctx: ctx, svc: svc, file: file, object: object, hash: cache.NewDigester()}
}

func (r *verifyReader) Read(p []byte) (int, error) {
	n, err := r.file.Read(p)
	if n > 0 {
		_, _ = r.hash.Write(p[:n])
		r.read += int64(n)
	}
	if !errors.Is(err, io.EOF) {
		return n, err
	}
	if got := r.hash.Digest(); got != r.object.Digest || r.read != r.object.Size {
		logger.Ctx(r.ctx).Error("缓存副本校验失败",
			zap.String("key", r.object.Key), zap.String("want", r.object.Digest),
			zap.String("got", got), zap.Int64("size", r.read))
		r.svc.dropRecord(r.ctx, r.object)
		return n, ErrCacheCorrupted
	}
	return n, err
}

func (r *verifyReader) Close() error {
	return r.file.Close()
}
