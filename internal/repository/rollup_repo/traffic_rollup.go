// Package rollup_repo 是流量分钟桶的数据访问层。
package rollup_repo

import (
	"context"
	"errors"
	"fmt"

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

// seriesColumn 把整分钟折算回它所属的等宽时间桶（UTC）的起点。
//
// 在 SQL 里分组而不是把分钟桶捞回 Go 里分：14 天是 14×1440 个桶乘上游数，
// 光把它们扫进内存就比这次聚合本身贵得多，而首页是匿名就能打的。
//
// 桶宽用 Sprintf 拼进 SQL 而不是走占位符：它只来自本进程里的常量，不是外部
// 输入，而写死成字面量能让分组键与 ORDER BY 在两种桶宽下是同一条语句。
const seriesColumn = "(bucket - bucket %% %d) AS bucket_start"

// ErrInvalidWidth 桶宽不是正数。
//
// 提前挡住而不是交给数据库：`bucket % 0` 在 sqlite 上返回 NULL、在 MySQL 上
// 也返回 NULL，聚合会安安静静地把整个区间塌成一个桶。
var ErrInvalidWidth = errors.New("桶宽必须是正数")

// SeriesQuery 一次等宽时间桶序列的查询条件。
type SeriesQuery struct {
	// From、To 左闭右开的区间边界（秒）。
	From int64
	To   int64
	// Width 桶宽（秒）：一天是 86400，一小时是 3600。
	Width int64
	// UpstreamID 只看某一个上游；0 表示全站。
	UpstreamID int64
}

// SeriesTotals 一个等宽时间桶的聚合结果。
//
// 放在 repo 包而不是 rollup_entity：它不是一个实体，是这张表按某个桶宽读出来
// 的形状，bucket_start 这一列在表里根本不存在。
type SeriesTotals struct {
	// Bucket 这个桶的起点（秒，UTC）。
	Bucket       int64 `gorm:"column:bucket_start" json:"bucket"`
	Requests     int64 `gorm:"column:requests" json:"requests"`
	Hits         int64 `gorm:"column:hits" json:"hits"`
	Denied       int64 `gorm:"column:denied" json:"denied"`
	OriginErrors int64 `gorm:"column:origin_errors" json:"origin_errors"`
	BytesServed  int64 `gorm:"column:bytes_served" json:"bytes_served"`
	BytesOrigin  int64 `gorm:"column:bytes_origin" json:"bytes_origin"`
}

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
	// SumBySeries 按等宽时间桶（UTC）分组的区间聚合，由旧到新。
	// 只有有流量的桶会有行，补零是调用方的事——这一层不知道界面要画几个点。
	SumBySeries(ctx context.Context, q SeriesQuery) ([]*SeriesTotals, error)
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

func (t *trafficRollupRepo) SumBySeries(ctx context.Context, q SeriesQuery) ([]*SeriesTotals, error) {
	if q.Width <= 0 {
		return nil, ErrInvalidWidth
	}
	list := make([]*SeriesTotals, 0)
	tx := db.Ctx(ctx).Model(&rollup_entity.TrafficRollup{}).
		Select(fmt.Sprintf(seriesColumn, q.Width)+","+sumColumns).
		Where("bucket >= ? AND bucket < ?", q.From, q.To)
	if q.UpstreamID > 0 {
		tx = tx.Where("upstream_id = ?", q.UpstreamID)
	}
	if err := tx.Group("bucket_start").
		// 由旧到新：界面上是一条时间轴，顺序在这里定好，调用方就不必各排一次。
		Order("bucket_start asc").
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
