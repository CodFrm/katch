// Package upstream_ctr 处理上游管理接口的请求。
package upstream_ctr

import (
	"context"

	"github.com/CodFrm/katch/internal/api/admin"
	api_upstream "github.com/CodFrm/katch/internal/api/upstream"
	"github.com/CodFrm/katch/internal/model/entity/event_entity"
	"github.com/CodFrm/katch/internal/service/event_svc"
	"github.com/CodFrm/katch/internal/service/stat_svc"
	"github.com/CodFrm/katch/internal/service/upstream_svc"
)

// Upstream 上游管理控制器。
type Upstream struct{}

// NewUpstream 构造上游管理控制器。
func NewUpstream() *Upstream {
	return &Upstream{}
}

// List 列出全部上游。
func (u *Upstream) List(ctx context.Context, req *admin.ListUpstreamsRequest) (*admin.ListUpstreamsResponse, error) {
	return upstream_svc.Upstream().List(ctx, req)
}

// PublicList 列出公开的上游。不要密钥，是否可见由「公开首页」设置决定，
// 那道闸在路由层，见 internal/api/router.go。
//
// 名片字段来自 upstream_svc，命中率与状态来自 stat_svc，两者在这里合并而不是
// 在 upstream_svc 里：stat_svc 已经依赖 upstream_svc（它要把 upstream_id 翻成
// 主机名），反过来再依赖回去就是一个导入环。
func (u *Upstream) PublicList(ctx context.Context, req *api_upstream.ListRequest) (*api_upstream.ListResponse, error) {
	resp, err := upstream_svc.Upstream().PublicList(ctx, req)
	if err != nil {
		return nil, err
	}
	stats, err := stat_svc.Stat().PublicUpstreams(ctx)
	if err != nil {
		// 统计读不出来就整个失败，而不是给一张命中率全是 0 的表：0% 命中率
		// 看起来就是「这台镜像站没在工作」，那比少一张表更误导人。
		return nil, err
	}
	for _, item := range resp.List {
		// 统计里没有这一行只可能是两次查询之间刚加了一个上游：它确实还没有
		// 任何流量，也不在退避里，normal 加 0 命中率就是它此刻的真实状态。
		item.Status = api_upstream.StatusNormal
		if stat, ok := stats[item.Host]; ok {
			item.HitRate = stat.HitRate
			item.CacheBytes = stat.CacheBytes
			item.Status = stat.Status
		}
	}
	return resp, nil
}

// Save 新登记一条上游。
//
// 写成功之后往事件流里记一条：一个上游是谁在什么时候加进来的，和它是谁停掉的
// 一样，事后只能从这条时间线上查——界面上只剩下现在的状态。
func (u *Upstream) Save(ctx context.Context, req *admin.SaveUpstreamRequest) (*admin.SaveUpstreamResponse, error) {
	resp, err := upstream_svc.Upstream().Save(ctx, req)
	if err != nil {
		return nil, err
	}
	u.record(ctx, event_entity.KindUpstreamCreated, resp.ID, req.Spec())
	return resp, nil
}

// Update 整条替换一条已存在的上游，含启停。
//
// 「拉取突然全挂了」之后第一个要查的就是这条上游是谁在什么时候停掉的，而那时
// 界面上只剩下现在的状态，所以启停必须和其余改动一样落进事件流。
func (u *Upstream) Update(ctx context.Context, req *admin.UpdateUpstreamRequest) (*admin.UpdateUpstreamResponse, error) {
	resp, err := upstream_svc.Upstream().Update(ctx, req)
	if err != nil {
		return nil, err
	}
	u.record(ctx, event_entity.KindUpstreamUpdated, resp.ID, req.Spec())
	return resp, nil
}

// record 把一次上游写入记进事件流。
//
// 细节里带上 enabled：启停和改登记信息是同一个端点，少了这个字段，时间线上就分不
// 出「改了回源地址」和「把它停了」——而后者才是运维要找的那一条。
func (u *Upstream) record(ctx context.Context, kind string, id int64, spec *admin.UpstreamSpec) {
	event_svc.Event().Record(ctx, &event_svc.RecordInput{
		Kind: kind, Actor: event_entity.ActorAdmin, UpstreamID: id,
		Detail: map[string]any{
			"host":    spec.Host,
			"kind":    spec.Kind,
			"origin":  spec.Origin,
			"enabled": spec.Enabled,
		},
	})
}

// Delete 删除一条上游。
func (u *Upstream) Delete(ctx context.Context, req *admin.DeleteUpstreamRequest) (*admin.DeleteUpstreamResponse, error) {
	// 主机名要在删之前取：删完这条记录就没了，而事件里只剩一个 id 的话，
	// 界面上这条「上游已删除」永远指不出删的是哪一个。
	host := u.hostOf(ctx, req.ID)
	resp, err := upstream_svc.Upstream().Delete(ctx, req)
	if err != nil {
		return nil, err
	}
	event_svc.Event().Record(ctx, &event_svc.RecordInput{
		Kind: event_entity.KindUpstreamDeleted, Actor: event_entity.ActorAdmin,
		UpstreamID: req.ID, Detail: map[string]any{"host": host},
	})
	return resp, nil
}

// hostOf 查这个 id 的主机名，查不到时给空串。
//
// 借 List 而不是另开一个按 id 查的方法：上游是人工维护的白名单，规模是几十条，
// 而这条路径只在管理接口删上游时走一次。取不到不是错误——事件记不上也不许
// 让这次删除失败，少一个主机名比少一次操作好。
func (u *Upstream) hostOf(ctx context.Context, id int64) string {
	list, err := upstream_svc.Upstream().List(ctx, &admin.ListUpstreamsRequest{})
	if err != nil {
		return ""
	}
	for _, item := range list.List {
		if item.ID == id {
			return item.Host
		}
	}
	return ""
}
