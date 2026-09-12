// Package rollup_repo 是流量分钟桶的数据访问层。
package rollup_repo

import (
	"context"

	"github.com/cago-frame/cago/database/db"

	"github.com/CodFrm/katch/internal/model/entity/rollup_entity"
)

//go:generate mockgen -source traffic_rollup.go -destination mock/traffic_rollup.go -package mock_rollup_repo

// sumColumns 区间聚合要求的那几列。
//
// COALESCE 不能省：空区间上的 SUM 是 NULL，扫进 int64 会报错，而「这 24 小时
// 一次请求也没有」是一个新装的镜像站的正常状态。
const sumColumns = "COALESCE(SUM(requests), 0) AS requests," +
	"COALESCE(SUM(hits), 0) AS hits," +
	"COALESCE(SUM(denied), 0) AS denied," +
	"COALESCE(SUM(origin_errors), 0) AS origin_errors," +
	"COALESCE(SUM(bytes_served), 0) AS bytes_served," +
	"COALESCE(SUM(bytes_origin), 0) AS bytes_origin"

// TrafficRollupRepo 分钟桶的存取。
//
// 区间一律是左闭右开 [from, to)：相邻的两个区间各自聚合时，边界那一分钟只能被
// 算进一边，否则「24 小时」和「7 天」的总量永远对不上。
type TrafficRollupRepo interface {
	// FindByBucket 取某个上游某一整分钟的那一行。没有不是错误，返回 (nil, nil)。
	FindByBucket(ctx context.Context, upstreamID, bucket int64) (*rollup_entity.TrafficRollup, error)
	Save(ctx context.Context, row *rollup_entity.TrafficRollup) error
	// Sum 全站区间聚合。
	Sum(ctx context.Context, from, to int64) (*rollup_entity.Totals, error)
	// SumByUpstream 按上游分组的区间聚合。
	SumByUpstream(ctx context.Context, from, to int64) ([]*rollup_entity.Totals, error)
	// Prune 裁掉 bucket 小于 before 的行，返回裁掉多少行。
	Prune(ctx context.Context, before int64) (int64, error)
}

var defaultTrafficRollup TrafficRollupRepo

// TrafficRollup 返回已注册的实现。
func TrafficRollup() TrafficRollupRepo {
	return defaultTrafficRollup
}

// RegisterTrafficRollup 注册实现，由 main 装配、由测试注入 mock。
func RegisterTrafficRollup(i TrafficRollupRepo) {
	defaultTrafficRollup = i
}

type trafficRollupRepo struct{}

// NewTrafficRollup 构造基于 gorm 的实现。
func NewTrafficRollup() TrafficRollupRepo {
	return &trafficRollupRepo{}
}

func (t *trafficRollupRepo) FindByBucket(ctx context.Context, upstreamID, bucket int64) (*rollup_entity.TrafficRollup, error) {
	ret := &rollup_entity.TrafficRollup{}
	if err := db.Ctx(ctx).Where("upstream_id=? AND bucket=?", upstreamID, bucket).First(ret).Error; err != nil {
		if db.RecordNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return ret, nil
}

func (t *trafficRollupRepo) Save(ctx context.Context, row *rollup_entity.TrafficRollup) error {
	return db.Ctx(ctx).Save(row).Error
}

func (t *trafficRollupRepo) Sum(ctx context.Context, from, to int64) (*rollup_entity.Totals, error) {
	ret := &rollup_entity.Totals{}
	if err := db.Ctx(ctx).Model(&rollup_entity.TrafficRollup{}).
		Select(sumColumns).
		Where("bucket >= ? AND bucket < ?", from, to).
		Scan(ret).Error; err != nil {
		return nil, err
	}
	return ret, nil
}

func (t *trafficRollupRepo) SumByUpstream(ctx context.Context, from, to int64) ([]*rollup_entity.Totals, error) {
	list := make([]*rollup_entity.Totals, 0)
	if err := db.Ctx(ctx).Model(&rollup_entity.TrafficRollup{}).
		Select("upstream_id,"+sumColumns).
		Where("bucket >= ? AND bucket < ?", from, to).
		Group("upstream_id").
		Scan(&list).Error; err != nil {
		return nil, err
	}
	return list, nil
}

func (t *trafficRollupRepo) Prune(ctx context.Context, before int64) (int64, error) {
	tx := db.Ctx(ctx).Where("bucket < ?", before).Delete(&rollup_entity.TrafficRollup{})
	if tx.Error != nil {
		return 0, tx.Error
	}
	return tx.RowsAffected, nil
}
