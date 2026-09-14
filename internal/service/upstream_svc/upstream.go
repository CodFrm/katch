// Package upstream_svc 是上游的业务层。
package upstream_svc

import (
	"context"
	"time"

	"github.com/cago-frame/cago/pkg/i18n"

	"github.com/CodFrm/katch/internal/api/admin"
	api_upstream "github.com/CodFrm/katch/internal/api/upstream"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/pkg/code"
	"github.com/CodFrm/katch/internal/repository/rule_repo"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
)

// UpstreamSvc 上游的业务操作。
type UpstreamSvc interface {
	// FindByHost 供代理层判定白名单用：不存在或已停用时返回 (nil, nil)，
	// 调用方无需再自己看 Enabled——「停用」和「没有这条记录」对外必须是同一件事。
	FindByHost(ctx context.Context, host string) (*upstream_entity.Upstream, error)
	List(ctx context.Context, req *admin.ListUpstreamsRequest) (*admin.ListUpstreamsResponse, error)
	// PublicList 供首页用：只给启用中的上游，且只给站点名片级的字段。
	// 过滤放在业务层而不是交给调用方：让每个调用方各筛一次，迟早有一个忘了筛。
	PublicList(ctx context.Context, req *api_upstream.ListRequest) (*api_upstream.ListResponse, error)
	// Save 新登记一条上游。
	Save(ctx context.Context, req *admin.SaveUpstreamRequest) (*admin.SaveUpstreamResponse, error)
	// Update 整条替换一条已存在的上游，含启停。不存在的 id 是 404 而不是新增：
	// 「这条记录必须已经存在」正是更新与新增之间的那点区别。
	Update(ctx context.Context, req *admin.UpdateUpstreamRequest) (*admin.UpdateUpstreamResponse, error)
	Delete(ctx context.Context, req *admin.DeleteUpstreamRequest) (*admin.DeleteUpstreamResponse, error)
}

type upstreamSvc struct{}

var defaultUpstream = &upstreamSvc{}

// Upstream 返回上游业务层。
func Upstream() UpstreamSvc {
	return defaultUpstream
}

func (u *upstreamSvc) FindByHost(ctx context.Context, host string) (*upstream_entity.Upstream, error) {
	upstream, err := upstream_repo.Upstream().FindByHost(ctx, host)
	if err != nil {
		return nil, err
	}
	if upstream == nil || !upstream.Enabled {
		return nil, nil
	}
	return upstream, nil
}

func (u *upstreamSvc) List(ctx context.Context, _ *admin.ListUpstreamsRequest) (*admin.ListUpstreamsResponse, error) {
	list, err := upstream_repo.Upstream().List(ctx)
	if err != nil {
		return nil, err
	}
	resp := &admin.ListUpstreamsResponse{List: make([]*admin.UpstreamItem, 0, len(list))}
	for _, v := range list {
		resp.List = append(resp.List, toItem(v))
	}
	return resp, nil
}

func (u *upstreamSvc) PublicList(ctx context.Context, _ *api_upstream.ListRequest) (*api_upstream.ListResponse, error) {
	list, err := upstream_repo.Upstream().List(ctx)
	if err != nil {
		return nil, err
	}
	resp := &api_upstream.ListResponse{List: make([]*api_upstream.Item, 0, len(list))}
	for _, v := range list {
		if !v.Enabled {
			continue
		}
		resp.List = append(resp.List, &api_upstream.Item{
			Host:              v.Host,
			Kind:              v.Kind,
			LibraryCompletion: v.LibraryCompletion,
		})
	}
	return resp, nil
}

func (u *upstreamSvc) Save(ctx context.Context, req *admin.SaveUpstreamRequest) (*admin.SaveUpstreamResponse, error) {
	id, err := u.write(ctx, 0, req.Spec())
	if err != nil {
		return nil, err
	}
	return &admin.SaveUpstreamResponse{ID: id}, nil
}

func (u *upstreamSvc) Update(ctx context.Context, req *admin.UpdateUpstreamRequest) (*admin.UpdateUpstreamResponse, error) {
	id, err := u.write(ctx, req.ID, req.Spec())
	if err != nil {
		return nil, err
	}
	return &admin.UpdateUpstreamResponse{ID: id}, nil
}

// write 落一条上游，id 为 0 时新增、否则整条替换那一条，返回它的 id。
//
// 新增与更新共用这一段而不是各写一份：两者的差别只有「要不要先把库里那条捞出来」，
// 余下的重名判定、默认策略兜底与字段映射一模一样，分成两份迟早会只改其中一份。
func (u *upstreamSvc) write(ctx context.Context, id int64, spec *admin.UpstreamSpec) (int64, error) {
	now := time.Now().Unix()
	upstream := &upstream_entity.Upstream{Createtime: now}
	if id != 0 {
		exist, err := upstream_repo.Upstream().Find(ctx, id)
		if err != nil {
			return 0, err
		}
		if exist == nil {
			return 0, i18n.NewNotFoundError(ctx, code.UpstreamNotFound)
		}
		// 以库里那条为底再覆盖字段，而不是拿一个空实体去 Save：后者会把
		// createtime 这类请求里没有的字段一起清零。
		upstream = exist
	}
	// host 是白名单的 key，重复注册必须挡住：同一个 host 有两条记录时，
	// 分发命中哪条取决于查询顺序，停用其中一条看起来会毫无效果。
	byHost, err := upstream_repo.Upstream().FindByHost(ctx, spec.Host)
	if err != nil {
		return 0, err
	}
	if byHost != nil && byHost.ID != id {
		return 0, i18n.NewError(ctx, code.UpstreamHostExists)
	}
	policy := spec.DefaultPolicy
	if policy == "" {
		policy = upstream_entity.PolicyAllowAll
	}
	upstream.Host = spec.Host
	upstream.Kind = spec.Kind
	upstream.Origin = spec.Origin
	// 整条替换，所以 enabled 原样照抄：这里任何一处「false 就不覆盖」的写法，
	// 都会让停用这个动作在库里什么也没发生。
	upstream.Enabled = spec.Enabled
	upstream.ImmutablePatterns = upstream_entity.PatternList(spec.ImmutablePatterns)
	upstream.MutableTTLSeconds = spec.MutableTTLSeconds
	upstream.DefaultPolicy = policy
	upstream.LibraryCompletion = spec.LibraryCompletion
	upstream.Note = spec.Note
	upstream.Updatetime = now
	// 写库这一步会顺手把拉取路径上的那份进程内快照掀掉（proxy_svc 的
	// cachedUpstreamRepo 包在这个接口外面），停用因此在下一个请求上就生效。
	if err := upstream_repo.Upstream().Save(ctx, upstream); err != nil {
		return 0, err
	}
	return upstream.ID, nil
}

func (u *upstreamSvc) Delete(ctx context.Context, req *admin.DeleteUpstreamRequest) (*admin.DeleteUpstreamResponse, error) {
	exist, err := upstream_repo.Upstream().Find(ctx, req.ID)
	if err != nil {
		return nil, err
	}
	if exist == nil {
		// 不存在的 id 不能当成删除成功：界面上「删掉了」和「这条根本不在」是
		// 两件事，后者通常意味着调用方拿的是一份过期的列表。
		return nil, i18n.NewNotFoundError(ctx, code.UpstreamNotFound)
	}
	// 先连带删掉这个上游名下的规则，再删上游本身。
	//
	// 留着的孤儿规则不会立刻出事——求值只看全局那层和当前上游那层，id 不在表里
	// 时谁都匹配不到。出事的是自增 id 被复用之后：下一个拿到这个 id 的上游会
	// 毫无征兆地继承一批本该消失的规则。写入侧已经有对称的一半（rule_svc.Save
	// 拒绝把规则挂到不存在的上游上），删除侧不能缺这一半。
	//
	// 顺序不能反：先删上游再删规则的话，规则那步一失败就正好落成上面那个缺陷，
	// 而且上游已经不在了，重试都找不到该清哪一批。级联做在这一层而不是数据库的
	// ON DELETE CASCADE：迁移只追加不修改，且要同时对 sqlite 和 MySQL 成立。
	if err := rule_repo.AccessRule().DeleteByUpstream(ctx, req.ID); err != nil {
		return nil, err
	}
	if err := upstream_repo.Upstream().Delete(ctx, req.ID); err != nil {
		return nil, err
	}
	return &admin.DeleteUpstreamResponse{}, nil
}

// toItem 把实体映射成对外结构。模式列表转成 []string，不把存储形态泄漏出去。
func toItem(v *upstream_entity.Upstream) *admin.UpstreamItem {
	patterns := make([]string, 0, len(v.ImmutablePatterns))
	patterns = append(patterns, v.ImmutablePatterns...)
	return &admin.UpstreamItem{
		ID:                v.ID,
		Host:              v.Host,
		Kind:              v.Kind,
		Origin:            v.Origin,
		Enabled:           v.Enabled,
		ImmutablePatterns: patterns,
		MutableTTLSeconds: v.MutableTTLSeconds,
		DefaultPolicy:     v.DefaultPolicy,
		LibraryCompletion: v.LibraryCompletion,
		Note:              v.Note,
		Createtime:        v.Createtime,
		Updatetime:        v.Updatetime,
	}
}
