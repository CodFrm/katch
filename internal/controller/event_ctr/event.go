// Package event_ctr 处理事件流管理接口的请求。
package event_ctr

import (
	"context"

	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/service/event_svc"
)

// Event 事件流控制器。
type Event struct{}

// NewEvent 构造事件流控制器。
func NewEvent() *Event {
	return &Event{}
}

// List 按时间倒序给出最近的事件。
func (e *Event) List(ctx context.Context, req *admin.ListEventsRequest) (*admin.ListEventsResponse, error) {
	return event_svc.Event().List(ctx, req)
}
