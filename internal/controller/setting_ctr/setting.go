// Package setting_ctr 处理运行时设置与管理密钥轮换的请求。
package setting_ctr

import (
	"context"

	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/service/setting_svc"
)

// Setting 运行时设置的管理控制器。
type Setting struct{}

// NewSetting 构造运行时设置的管理控制器。
func NewSetting() *Setting {
	return &Setting{}
}

// List 读出全部运行时设置。
func (s *Setting) List(ctx context.Context, req *admin.ListSettingsRequest) (*admin.ListSettingsResponse, error) {
	return setting_svc.Setting().List(ctx, req)
}

// Save 写入若干运行时设置。
func (s *Setting) Save(ctx context.Context, req *admin.SaveSettingsRequest) (*admin.SaveSettingsResponse, error) {
	return setting_svc.Setting().Save(ctx, req)
}

// RotateAdminKey 轮换管理密钥。
//
// 轮换是设置的一部分而不是另开一套鉴权：它和别的管理接口一样要当前密钥，
// 换完之后旧密钥在下一个请求上就不再被接受（决策 18）。
func (s *Setting) RotateAdminKey(ctx context.Context, req *admin.RotateAdminKeyRequest) (*admin.RotateAdminKeyResponse, error) {
	return setting_svc.Setting().RotateAdminKey(ctx, req)
}
