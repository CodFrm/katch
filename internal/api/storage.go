package api

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net"
	"net/http"

	"github.com/cago-frame/cago/pkg/i18n"
	"gorm.io/gorm"

	"github.com/CodFrm/katch/internal/pkg/code"
)

// storageAware 把「存储不可用」翻成 503（失败与降级一节：
// 「数据库不可用：读路径用进程内的上游/规则/设置缓存继续服务，管理接口返回 503」）。
//
// 503 和 500 不是同一句话：503 告诉运维和前面的反向代理「存储临时挂了，待会儿
// 再来」，500 说的是「katch 自己坏了」。规格把这一条和「拉取不该因为管理面的存储
// 故障而中断」写在一起——库挂掉时这台站点应该表现成「只有后台暂时用不了」。
//
// 装在这一层而不是仓储或服务里：cago 的 Bind 直接把控制器返回的 error 交给
// httputils.HandleError，中间没有可挂的钩子，而 error 的类型信息到了 gin 中间件
// 那里已经被写成响应了。包在绑定处则只有一个地方，且天然只盖住管理接口——
// 公开接口与拉取路径都不经过它，它们各有自己的答法（404 / 502）。
func storageAware[Req, Resp any](
	f func(context.Context, *Req) (*Resp, error),
) func(context.Context, *Req) (*Resp, error) {
	return func(ctx context.Context, req *Req) (*Resp, error) {
		resp, err := f(ctx, req)
		if err != nil && storageUnavailable(err) {
			return resp, i18n.NewErrorWithStatus(ctx,
				http.StatusServiceUnavailable, code.StorageUnavailable)
		}
		return resp, err
	}
}

// storageUnavailable 判断这个错误是不是「库连不上」。
//
// 只认连接这一层的信号，不按错误文本猜：一次查询写错了列名同样会从仓储里冒上来，
// 那是代码自己的 bug，必须留在 500。认错方向的代价是实打实的——把 bug 报成 503
// 会让监控把一个永远不会自愈的故障当成临时抖动一直等下去。
func storageUnavailable(err error) bool {
	// 拨号/读写套接字失败：MySQL 挂掉、网络断了都是这一类。
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	// 连接池把一条坏连接交了出来，或者连接已经被关掉。
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) {
		return true
	}
	// gorm 手上根本没有一个可用的 *sql.DB。
	return errors.Is(err, gorm.ErrInvalidDB)
}
