// Package upstream_ctr 处理上游管理接口的请求。
package upstream_ctr

import (
	"context"

	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/service/upstream_svc"
)

// Upstream 上游管理控制器。
type Upstream struct{}

// NewUpstream 构造上游管理控制器。
func NewUpstream() *Upstream {
	return &Upstream{}
}

// List 列出全部上游。
func (u *Upstream) List(ctx context.Context, req *admin.ListUpstreamsRequest) (*admin.ListUpstreamsResponse, error) {
	return upstream_svc.Upstream().List(ctx, req)
}

// Save 新增或更新一条上游。
func (u *Upstream) Save(ctx context.Context, req *admin.SaveUpstreamRequest) (*admin.SaveUpstreamResponse, error) {
	return upstream_svc.Upstream().Save(ctx, req)
}

// Delete 删除一条上游。
func (u *Upstream) Delete(ctx context.Context, req *admin.DeleteUpstreamRequest) (*admin.DeleteUpstreamResponse, error) {
	return upstream_svc.Upstream().Delete(ctx, req)
}
