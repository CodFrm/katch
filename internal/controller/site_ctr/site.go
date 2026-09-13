// Package site_ctr 处理站点名片的请求。
package site_ctr

import (
	"context"

	"github.com/CodFrm/katch/internal/api/site"
	"github.com/CodFrm/katch/internal/service/setting_svc"
)

// Site 站点名片控制器。
type Site struct{}

// NewSite 构造站点名片控制器。
func NewSite() *Site {
	return &Site{}
}

// Info 站点名称与拉取命令里用的站点地址。
func (s *Site) Info(ctx context.Context, req *site.InfoRequest) (*site.InfoResponse, error) {
	return setting_svc.Setting().SiteInfo(ctx, req)
}
