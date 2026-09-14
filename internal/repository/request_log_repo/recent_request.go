// Package request_log_repo 是「最近请求」明细的数据访问层。
//
// 只有三种读法：按上游取最近 N 条（面板）、按 at 裁剪（保留期）、批量插入
// （落库循环）。没有按区间聚合——那是 rollup_repo 的活，这张表只答顺序。
package request_log_repo

import (
	"context"

	"github.com/cago-frame/cago/database/db"

	"github.com/CodFrm/katch/internal/model/entity/request_log_entity"
)

//go:generate mockgen -source recent_request.go -destination mock/recent_request.go -package mock_request_log_repo

// RequestLogRepo 最近请求明细的存取。
type RequestLogRepo interface {
	// Save 一次写入一批。空的一批不是错误。
	Save(ctx context.Context, rows []*request_log_entity.RecentRequest) error
	// ListRecent 取一个上游最近的 limit 条，按 at 倒序、同一秒内按 id 倒序。
	ListRecent(ctx context.Context, upstreamID int64, limit int) ([]*request_log_entity.RecentRequest, error)
	// Prune 裁掉 at 早于 before 的行，返回裁掉多少行。
	Prune(ctx context.Context, before int64) (int64, error)
}

var defaultRequestLog RequestLogRepo

// RecentRequestLog 返回已注册的实现。
//
// 名字里带 Recent 是为了和实体（request_log_entity.RecentRequest）、表（recent_request）
// 对齐：这一层存的是明细，不是另一张叫 request_log 的表。
func RecentRequestLog() RequestLogRepo {
	return defaultRequestLog
}

// RegisterRequestLog 注册实现，由 main 装配、由测试注入 mock。
func RegisterRequestLog(i RequestLogRepo) {
	defaultRequestLog = i
}

type requestLogRepo struct{}

// NewRequestLog 构造基于 gorm 的实现。
func NewRequestLog() RequestLogRepo {
	return &requestLogRepo{}
}

func (r *requestLogRepo) Save(ctx context.Context, rows []*request_log_entity.RecentRequest) error {
	if len(rows) == 0 {
		// GORM 对空切片报 ErrEmptySlice。这一批没东西可写不是错误，放任它冒出去
		// 只会让落库循环在安静的时候每秒多一条 error。
		return nil
	}
	return db.Ctx(ctx).Create(rows).Error
}

func (r *requestLogRepo) ListRecent(ctx context.Context, upstreamID int64, limit int) ([]*request_log_entity.RecentRequest, error) {
	list := make([]*request_log_entity.RecentRequest, 0)
	if limit <= 0 {
		// GORM 对负的 limit 会把 LIMIT 整个省掉，查询会退化成对上游全部历史
		// 的全表扫。上限非正时直接给空，不去库。
		return list, nil
	}
	if err := db.Ctx(ctx).
		Where("upstream_id = ?", upstreamID).
		// 同一秒可能落进多行，id 跟着自增，所以它是同一秒内的第二排序键；少了它，
		// 两次查询里同一秒的先后可以互换，面板上的顺序会跳。
		Order("at DESC, id DESC").
		Limit(limit).
		Find(&list).Error; err != nil {
		return nil, err
	}
	return list, nil
}

func (r *requestLogRepo) Prune(ctx context.Context, before int64) (int64, error) {
	tx := db.Ctx(ctx).Where("at < ?", before).Delete(&request_log_entity.RecentRequest{})
	if tx.Error != nil {
		return 0, tx.Error
	}
	return tx.RowsAffected, nil
}
