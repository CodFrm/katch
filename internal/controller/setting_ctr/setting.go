// Package setting_ctr 处理运行时设置与管理密钥轮换的请求。
package setting_ctr

import (
	"context"
	"sort"

	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/model/entity/event_entity"
	"github.com/CodFrm/katch/internal/service/event_svc"
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
	resp, err := setting_svc.Setting().Save(ctx, req)
	if err != nil {
		return nil, err
	}
	// 只记键名，不记值：设置页本来就读得到当前值，而一条只追加的记录里躺着的
	// 历史值会随着日后往设置表里加一项带凭据的东西一起被永久留下来。
	keys := make([]string, 0, len(req.Settings))
	for key := range req.Settings {
		keys = append(keys, key)
	}
	// 排序：map 的遍历顺序每次都不一样，不排的话同一次保存在时间线上的写法
	// 每次刷新都在变。
	sort.Strings(keys)
	event_svc.Event().Record(ctx, &event_svc.RecordInput{
		Kind: event_entity.KindSettingChanged, Actor: event_entity.ActorAdmin,
		Detail: map[string]any{"keys": keys},
	})
	return resp, nil
}

// RotateAdminKey 轮换管理密钥。
//
// 轮换是设置的一部分而不是另开一套鉴权：它和别的管理接口一样要当前密钥，
// 换完之后旧密钥在下一个请求上就不再被接受（决策 18）。
func (s *Setting) RotateAdminKey(ctx context.Context, req *admin.RotateAdminKeyRequest) (*admin.RotateAdminKeyResponse, error) {
	resp, err := setting_svc.Setting().RotateAdminKey(ctx, req)
	if err != nil {
		return nil, err
	}
	// 细节是空的，这是刻意的：轮换要记的是「什么时候被换过」这个事实，
	// 新旧密钥都不许落进一条只追加的记录里。
	event_svc.Event().Record(ctx, &event_svc.RecordInput{
		Kind: event_entity.KindAdminKeyRotated, Actor: event_entity.ActorAdmin,
	})
	return resp, nil
}
