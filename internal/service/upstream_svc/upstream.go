// Package upstream_svc 是上游的业务层。
package upstream_svc

import (
	"context"
	"time"

	"github.com/cago-frame/cago/pkg/i18n"

	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/pkg/code"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
)

// UpstreamSvc 上游的业务操作。
type UpstreamSvc interface {
	// FindByHost 供代理层判定白名单用：不存在或已停用时返回 (nil, nil)，
	// 调用方无需再自己看 Enabled——「停用」和「没有这条记录」对外必须是同一件事。
	FindByHost(ctx context.Context, host string) (*upstream_entity.Upstream, error)
	List(ctx context.Context, req *admin.ListUpstreamsRequest) (*admin.ListUpstreamsResponse, error)
	Save(ctx context.Context, req *admin.SaveUpstreamRequest) (*admin.SaveUpstreamResponse, error)
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

func (u *upstreamSvc) Save(ctx context.Context, req *admin.SaveUpstreamRequest) (*admin.SaveUpstreamResponse, error) {
	now := time.Now().Unix()
	upstream := &upstream_entity.Upstream{Createtime: now}
	if req.ID != 0 {
		exist, err := upstream_repo.Upstream().Find(ctx, req.ID)
		if err != nil {
			return nil, err
		}
		if exist == nil {
			return nil, i18n.NewNotFoundError(ctx, code.UpstreamNotFound)
		}
		// 以库里那条为底再覆盖字段，而不是拿一个空实体去 Save：后者会把
		// createtime 这类请求里没有的字段一起清零。
		upstream = exist
	}
	// host 是白名单的 key，重复注册必须挡住：同一个 host 有两条记录时，
	// 分发命中哪条取决于查询顺序，停用其中一条看起来会毫无效果。
	byHost, err := upstream_repo.Upstream().FindByHost(ctx, req.Host)
	if err != nil {
		return nil, err
	}
	if byHost != nil && byHost.ID != req.ID {
		return nil, i18n.NewError(ctx, code.UpstreamHostExists)
	}
	policy := req.DefaultPolicy
	if policy == "" {
		policy = upstream_entity.PolicyAllowAll
	}
	upstream.Host = req.Host
	upstream.Kind = req.Kind
	upstream.Origin = req.Origin
	upstream.Enabled = req.Enabled
	upstream.ImmutablePatterns = upstream_entity.PatternList(req.ImmutablePatterns)
	upstream.MutableTTLSeconds = req.MutableTTLSeconds
	upstream.DefaultPolicy = policy
	upstream.LibraryCompletion = req.LibraryCompletion
	upstream.Note = req.Note
	upstream.Updatetime = now
	if err := upstream_repo.Upstream().Save(ctx, upstream); err != nil {
		return nil, err
	}
	return &admin.SaveUpstreamResponse{ID: upstream.ID}, nil
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
