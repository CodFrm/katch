// Package log_ctr 处理结构化日志的读取请求。
package log_ctr

import (
	"context"

	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/service/log_svc"
)

// Log 日志控制器。
type Log struct{}

// NewLog 构造日志控制器。
func NewLog() *Log {
	return &Log{}
}

// RecentRequests 某个上游的最近请求。要密钥：逐条请求比按小时的量还细，
// 里面是谁在拉什么，是运营数据。
func (l *Log) RecentRequests(ctx context.Context, req *admin.RecentRequestsRequest) (
	*admin.RecentRequestsResponse, error) {
	return log_svc.Log().RecentRequests(ctx, req)
}
