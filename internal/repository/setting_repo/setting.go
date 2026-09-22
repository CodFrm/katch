// Package setting_repo 是运行时设置的数据访问层。
package setting_repo

import (
	"context"

	"github.com/cago-frame/cago/database/db"

	"github.com/CodFrm/katch/internal/model/entity/setting_entity"
)

//go:generate mockgen -source setting.go -destination mock/setting.go -package mock_setting_repo

// SettingRepo 设置的存取。键不存在时 Find 返回 (nil, nil)。
type SettingRepo interface {
	Find(ctx context.Context, key string) (*setting_entity.Setting, error)
	Save(ctx context.Context, setting *setting_entity.Setting) error
}

var defaultSetting SettingRepo

type cacheManagedSettingRepo interface {
	SettingRepo
	Uncached() SettingRepo
	Invalidate()
}

// Setting 返回已注册的实现。
func Setting() SettingRepo {
	return defaultSetting
}

// UncachedSetting 返回事务回调使用的底层仓储，避免在提交前失效进程缓存。
func UncachedSetting() SettingRepo {
	if managed, ok := defaultSetting.(cacheManagedSettingRepo); ok {
		return managed.Uncached()
	}
	return defaultSetting
}

// InvalidateSettingCache 在事务成功提交后失效进程缓存。
func InvalidateSettingCache() {
	if managed, ok := defaultSetting.(cacheManagedSettingRepo); ok {
		managed.Invalidate()
	}
}

// RegisterSetting 注册实现，由 main 装配、由测试注入 mock。
func RegisterSetting(i SettingRepo) {
	defaultSetting = i
}

type settingRepo struct{}

// NewSetting 构造基于 gorm 的实现。
func NewSetting() SettingRepo {
	return &settingRepo{}
}

func (s *settingRepo) Find(ctx context.Context, key string) (*setting_entity.Setting, error) {
	ret := &setting_entity.Setting{}
	if err := db.Ctx(ctx).Where("`key`=?", key).First(ret).Error; err != nil {
		if db.RecordNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return ret, nil
}

// Save 以 key 为主键写入。gorm 的 Save 在更新影响 0 行时会退化成插入，
// 于是「首次写入」和「轮换」是同一条代码路径。
func (s *settingRepo) Save(ctx context.Context, setting *setting_entity.Setting) error {
	return db.Ctx(ctx).Save(setting).Error
}
