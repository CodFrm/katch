// Package event_svc 是事件流的业务层：后台概览上那条把自动告警与人为变更
// 放在一起的时间线。
//
// 它只做两件事——记一条、给出最近 N 条。记录是**旁路**的：写不进去只留一条日志，
// 绝不把被观察的那次操作一起拖垮（一次改规则不该因为事件表满了而失败）。
package event_svc

import (
	"context"
	"encoding/json"
	"time"

	"github.com/cago-frame/cago/pkg/logger"
	"go.uber.org/zap"

	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/model/entity/event_entity"
	"github.com/CodFrm/katch/internal/repository/event_repo"
)

const (
	// defaultLimit 不给 limit 时给几条。概览上那条时间线一屏装得下的量级。
	defaultLimit = 50
	// maxLimit 一次最多给几条，和请求结构体上的校验同一个数。
	maxLimit = 200
	// emptyDetail 没有细节、或者细节序列化不出来时落库的值。
	//
	// 不落空串：Detail 在接口上是一段 JSON，空串会让整个响应不是合法 JSON。
	emptyDetail = "{}"
)

// RecordInput 一条要记下来的事件。
type RecordInput struct {
	// Kind 事件类别，取 event_entity 的 Kind* 常量。
	Kind string
	// Actor 操作人，取 event_entity 的 Actor* 常量。
	Actor string
	// UpstreamID 这条事件属于哪个上游，0 表示与具体上游无关。
	UpstreamID int64
	// Detail 「改了什么」，任何能序列化成 JSON 对象的值。
	//
	// 这里要的是字段，不是一句拼好的话：句子既翻译不了也筛选不了，而界面本来
	// 就要按自己的语言组织文案。
	Detail any
}

// EventSvc 事件流的业务操作。
type EventSvc interface {
	// Record 记一条事件。
	//
	// **不返回错误**，这是刻意的：调用点全在别的操作的成功路径上（改完规则、
	// 淘汰完对象、上游刚进退避），给一个错误回来只会诱使调用方把它往上抛，
	// 于是一条记不上的旁路日志就变成了一次失败的管理写入。
	Record(ctx context.Context, in *RecordInput)
	List(ctx context.Context, req *admin.ListEventsRequest) (*admin.ListEventsResponse, error)
}

type eventSvc struct{}

var defaultEvent EventSvc = &eventSvc{}

// Event 返回事件流业务层。
func Event() EventSvc {
	return defaultEvent
}

// Register 注册实现，由测试注入。
func Register(svc EventSvc) {
	defaultEvent = svc
}

func (e *eventSvc) Record(ctx context.Context, in *RecordInput) {
	repo := event_repo.Event()
	if repo == nil {
		// 仓储没装配（用例里、或者 main 漏了一行）。丢掉这条事件，但要说出来：
		// 安静地什么都不做会让整条时间线在生产上空着，而没人知道为什么。
		logger.Ctx(ctx).Warn("事件仓储未装配，丢弃事件", zap.String("kind", in.Kind))
		return
	}
	event := &event_entity.Event{
		Kind:       in.Kind,
		Actor:      in.Actor,
		UpstreamID: in.UpstreamID,
		Detail:     marshalDetail(ctx, in.Detail),
		Createtime: time.Now().Unix(),
	}
	if err := repo.Create(ctx, event); err != nil {
		logger.Ctx(ctx).Error("记录事件失败", zap.String("kind", in.Kind), zap.Error(err))
	}
}

// marshalDetail 把细节序列化成 JSON 对象。
//
// 序列化不出来只丢细节，不丢事件：「上游进了退避」这件事本身比它的细节要紧。
func marshalDetail(ctx context.Context, detail any) string {
	if detail == nil {
		return emptyDetail
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		logger.Ctx(ctx).Error("事件细节序列化失败", zap.Error(err))
		return emptyDetail
	}
	return string(raw)
}

func (e *eventSvc) List(ctx context.Context, req *admin.ListEventsRequest) (*admin.ListEventsResponse, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	repo := event_repo.Event()
	if repo == nil {
		// 读也一样不炸：仓储没装配时时间线是空的，而不是一个打不开的页面。
		logger.Ctx(ctx).Warn("事件仓储未装配，事件流为空")
		return &admin.ListEventsResponse{List: []*admin.EventItem{}}, nil
	}
	list, err := repo.List(ctx, limit)
	if err != nil {
		return nil, err
	}
	resp := &admin.ListEventsResponse{List: make([]*admin.EventItem, 0, len(list))}
	for _, v := range list {
		resp.List = append(resp.List, toItem(v))
	}
	return resp, nil
}

// toItem 把实体映射成对外结构。
func toItem(event *event_entity.Event) *admin.EventItem {
	detail := json.RawMessage(event.Detail)
	if !json.Valid(detail) {
		// 库里躺着一段读不懂的细节时给一个空对象，而不是把坏数据原样吐出去——
		// 那会让整个响应不是合法 JSON，一条坏记录就能让整页时间线打不开。
		detail = json.RawMessage(emptyDetail)
	}
	return &admin.EventItem{
		ID:         event.ID,
		Kind:       event.Kind,
		Actor:      event.Actor,
		UpstreamID: event.UpstreamID,
		Detail:     detail,
		Createtime: event.Createtime,
	}
}
