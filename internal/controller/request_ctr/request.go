// Package request_ctr 处理「最近请求」的读取请求。
package request_ctr

import (
	"context"

	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/service/request_svc"
)

// Request 最近请求控制器。
type Request struct{}

// NewRequest 构造最近请求控制器。
func NewRequest() *Request {
	return &Request{}
}

// RecentRequests 某个上游的最近请求。要密钥：逐条请求比按小时的量还细，
// 里面是谁在拉什么，是运营数据。
func (r *Request) RecentRequests(ctx context.Context, req *admin.RecentRequestsRequest) (
	*admin.RecentRequestsResponse, error) {
	return request_svc.Request().RecentRequests(ctx, req)
}
