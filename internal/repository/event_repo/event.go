// Package event_repo 是事件流的数据访问层。
package event_repo

import (
	"context"

	"github.com/cago-frame/cago/database/db"

	"github.com/CodFrm/katch/internal/model/entity/event_entity"
)

//go:generate mockgen -source event.go -destination mock/event.go -package mock_event_repo

// EventRepo 事件的存取。
//
// 只有追加和「最近 N 条」：事件是只追加的观察记录，没有更新也没有删除——
// 一条改得动的历史记录不再是历史。
type EventRepo interface {
	Create(ctx context.Context, event *event_entity.Event) error
	// List 按时间倒序返回最近 limit 条。
	List(ctx context.Context, limit int) ([]*event_entity.Event, error)
}

// defaultEvent 出厂是 nil：事件记录挂在缓存回收、退避转换这些拉取路径上的缝里，
// 没装配就往一个取不到连接的 gorm 实现上写，会把一次拉取变成 panic。
// 服务层遇到 nil 是丢掉这条事件并留一条日志，见 event_svc。
var defaultEvent EventRepo

// Event 返回已注册的实现，未装配时为 nil。
func Event() EventRepo {
	return defaultEvent
}

// RegisterEvent 注册实现，由 main 装配、由测试注入 mock。
func RegisterEvent(i EventRepo) {
	defaultEvent = i
}

type eventRepo struct{}

// NewEvent 构造基于 gorm 的实现。
func NewEvent() EventRepo {
	return &eventRepo{}
}

func (e *eventRepo) Create(ctx context.Context, event *event_entity.Event) error {
	return db.Ctx(ctx).Create(event).Error
}

// List 最近 limit 条，新的在前。
//
// 按 id 倒序而不是 createtime：createtime 是秒，同一秒里落的几条事件在它上面
// 分不出先后，而「改完设置紧接着轮换密钥」恰恰就在同一秒里。id 自增，它给的
// 就是写入顺序。
func (e *eventRepo) List(ctx context.Context, limit int) ([]*event_entity.Event, error) {
	list := make([]*event_entity.Event, 0)
	if err := db.Ctx(ctx).Order("id desc").Limit(limit).Find(&list).Error; err != nil {
		return nil, err
	}
	return list, nil
}
