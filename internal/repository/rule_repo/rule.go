// Package rule_repo 是访问规则的数据访问层。
package rule_repo

import (
	"context"

	"github.com/cago-frame/cago/database/db"

	"github.com/CodFrm/katch/internal/model/entity/rule_entity"
)

//go:generate mockgen -source rule.go -destination mock/rule.go -package mock_rule_repo

// AccessRuleRepo 访问规则的存取。
//
// 只有整表的 List，没有「按上游查」：规则是人工维护的策略，规模是几十条，而
// 求值要的是「全局那一层加这个上游那一层」——两次查询换不来什么，反而让上层
// 多一个「两次查询之间规则被改了」的中间态。
//
// 「不存在」不是错误：Find 在没查到时返回 (nil, nil)。
type AccessRuleRepo interface {
	Find(ctx context.Context, id int64) (*rule_entity.AccessRule, error)
	List(ctx context.Context) ([]*rule_entity.AccessRule, error)
	Save(ctx context.Context, rule *rule_entity.AccessRule) error
	Delete(ctx context.Context, id int64) error
	// DeleteByUpstream 删掉一个上游名下的全部规则，供上游被删除时连带清理。
	// upstreamID 为 0 时什么都不做，见实现里的说明。
	DeleteByUpstream(ctx context.Context, upstreamID int64) error
}

// defaultAccessRule 出厂就是基于 gorm 的那份实现。
//
// 和上游仓储不同，这里不等 main 注册：规则闸挂在拉取路径上，忘了装配的表现是
// 每一次拉取都 panic，而那是一个只在生产上才看得见的空指针。gorm 的实现本身
// 不持有连接（db.Ctx 在调用时才取），提前构造没有代价。
var defaultAccessRule AccessRuleRepo = NewAccessRule()

// AccessRule 返回已注册的实现。
func AccessRule() AccessRuleRepo {
	return defaultAccessRule
}

// RegisterAccessRule 注册实现，由测试注入 mock、由 main 按需替换。
func RegisterAccessRule(i AccessRuleRepo) {
	defaultAccessRule = i
}

type accessRuleRepo struct{}

// NewAccessRule 构造基于 gorm 的实现。
func NewAccessRule() AccessRuleRepo {
	return &accessRuleRepo{}
}

func (a *accessRuleRepo) Find(ctx context.Context, id int64) (*rule_entity.AccessRule, error) {
	ret := &rule_entity.AccessRule{}
	if err := db.Ctx(ctx).Where("id=?", id).First(ret).Error; err != nil {
		if db.RecordNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return ret, nil
}

// List 列出全部规则。这个排序是给管理界面看的——求值时的顺序由具体度定序决定，
// 与库里的顺序无关（决策 15：规则没有人工顺序）。
func (a *accessRuleRepo) List(ctx context.Context) ([]*rule_entity.AccessRule, error) {
	list := make([]*rule_entity.AccessRule, 0)
	if err := db.Ctx(ctx).Order("upstream_id asc,pattern asc").Find(&list).Error; err != nil {
		return nil, err
	}
	return list, nil
}

// Save 主键为空时插入，否则整行更新。
//
// 用 Save 而不是 Updates(struct)：后者跳过零值字段，于是「把一条上游内规则
// 改成全局规则」（upstream_id 改回 0）会被静默丢掉。
func (a *accessRuleRepo) Save(ctx context.Context, rule *rule_entity.AccessRule) error {
	return db.Ctx(ctx).Save(rule).Error
}

func (a *accessRuleRepo) Delete(ctx context.Context, id int64) error {
	return db.Ctx(ctx).Where("id=?", id).Delete(&rule_entity.AccessRule{}).Error
}

// DeleteByUpstream 删掉一个上游名下的全部规则。
//
// upstreamID 为 0 时直接返回：0 在这张表里是「全局规则」这个正常取值，不是
// 「没有上游」。真把它拼进 WHERE，一次误传就会把全站的全局规则一起删光，而那
// 批规则正是站点最要紧的那道闸。挡在这一层而不是只靠调用方自觉：这个方法删的
// 是一整批行，代价和「少删一次」完全不对称。
func (a *accessRuleRepo) DeleteByUpstream(ctx context.Context, upstreamID int64) error {
	if upstreamID == 0 {
		return nil
	}
	return db.Ctx(ctx).Where("upstream_id=?", upstreamID).
		Delete(&rule_entity.AccessRule{}).Error
}
