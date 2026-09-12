// Package cache_ctr 处理缓存对象管理接口的请求。
package cache_ctr

import (
	"context"

	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/model/entity/cache_entity"
	"github.com/CodFrm/katch/internal/service/cache_svc"
)

// Cache 缓存对象管理控制器。
type Cache struct{}

// NewCache 构造缓存对象管理控制器。
func NewCache() *Cache {
	return &Cache{}
}

// Search 按关键字分页搜索缓存对象。
//
// 这里做请求与实体的互译，而不是让 cache_svc 收发 admin 包的结构体：缓存层同时
// 长在拉取路径上（Get/Put），让它依赖一组管理接口的 DTO 会把两条路径绑在一起。
func (c *Cache) Search(ctx context.Context, req *admin.SearchCacheObjectsRequest) (*admin.SearchCacheObjectsResponse, error) {
	result, err := cache_svc.Cache().Search(ctx, &cache_svc.SearchRequest{
		UpstreamID: req.UpstreamID,
		Keyword:    req.Keyword,
		Page:       req.Page,
		Size:       req.Size,
	})
	if err != nil {
		return nil, err
	}
	resp := &admin.SearchCacheObjectsResponse{
		List:  make([]*admin.CacheObjectItem, 0, len(result.List)),
		Total: result.Total,
		Page:  result.Page,
		Size:  result.Size,
	}
	for _, object := range result.List {
		resp.List = append(resp.List, toItem(object))
	}
	return resp, nil
}

// Purge 清缓存：给 id 清一条，给 upstream_id 清整个上游（pin 过的会被跳过）。
func (c *Cache) Purge(ctx context.Context, req *admin.PurgeCacheRequest) (*admin.PurgeCacheResponse, error) {
	result, err := cache_svc.Cache().Purge(ctx, &cache_svc.PurgeRequest{
		ID:         req.ID,
		UpstreamID: req.UpstreamID,
	})
	if err != nil {
		return nil, err
	}
	return &admin.PurgeCacheResponse{Removed: result.Removed, Skipped: result.Skipped}, nil
}

// Pin 钉住或放开一个缓存对象。钉住的对象既不参与淘汰，也不被批量清除带走。
func (c *Cache) Pin(ctx context.Context, req *admin.PinCacheObjectRequest) (*admin.PinCacheObjectResponse, error) {
	if err := cache_svc.Cache().Pin(ctx, &cache_svc.PinRequest{
		ID:     req.ID,
		Pinned: req.Pinned,
	}); err != nil {
		return nil, err
	}
	return &admin.PinCacheObjectResponse{}, nil
}

func toItem(object *cache_entity.CacheObject) *admin.CacheObjectItem {
	return &admin.CacheObjectItem{
		ID:           object.ID,
		UpstreamID:   object.UpstreamID,
		Key:          object.Key,
		Digest:       object.Digest,
		Size:         object.Size,
		Immutable:    object.Immutable,
		Pinned:       object.Pinned,
		ExpiresAt:    object.ExpiresAt,
		LastAccessAt: object.LastAccessAt,
		HitCount:     object.HitCount,
		Createtime:   object.Createtime,
		Updatetime:   object.Updatetime,
	}
}
