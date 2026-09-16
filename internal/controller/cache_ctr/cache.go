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

// Tree 列缓存对象目录树的一层。
func (c *Cache) Tree(ctx context.Context, req *admin.CacheTreeRequest) (*admin.CacheTreeResponse, error) {
	result, err := cache_svc.Cache().Tree(ctx, &cache_svc.TreeRequest{Path: req.Path, Offset: req.Offset})
	if err != nil {
		return nil, err
	}
	resp := &admin.CacheTreeResponse{
		Path:        result.Path,
		TotalCount:  result.TotalCount,
		TotalPinned: result.TotalPinned,
		TotalSize:   result.TotalSize,
		Children:    make([]*admin.CacheTreeNode, 0, len(result.Children)),
		HasMore:     result.HasMore,
		NextOffset:  result.NextOffset,
	}
	for _, node := range result.Children {
		item := &admin.CacheTreeNode{
			Kind:         node.Kind,
			Name:         node.Name,
			Path:         node.Path,
			Count:        node.Count,
			PinnedCount:  node.PinnedCount,
			Size:         node.Size,
			LastAccessAt: node.LastAccessAt,
			Variant:      node.Variant,
		}
		if node.Object != nil {
			item.Object = toTreeItem(node.Object, node.Host)
		}
		resp.Children = append(resp.Children, item)
	}
	return resp, nil
}

// TreeSearch 在目录树的一个目录下搜索对象。
func (c *Cache) TreeSearch(ctx context.Context, req *admin.CacheTreeSearchRequest) (*admin.CacheTreeSearchResponse, error) {
	result, err := cache_svc.Cache().TreeSearch(ctx, &cache_svc.TreeSearchRequest{Path: req.Path, Keyword: req.Keyword})
	if err != nil {
		return nil, err
	}
	resp := &admin.CacheTreeSearchResponse{
		Path:      result.Path,
		Matched:   result.Matched,
		Truncated: result.Truncated,
		Objects:   make([]*admin.CacheTreeSearchObject, 0, len(result.Objects)),
		Dirs:      make([]*admin.CacheTreeSearchDir, 0, len(result.Dirs)),
	}
	for _, object := range result.Objects {
		resp.Objects = append(resp.Objects, &admin.CacheTreeSearchObject{
			Name:    object.Name,
			Path:    object.Path,
			Variant: object.Variant,
			Object:  toTreeItem(object.Object, object.Host),
		})
	}
	for _, dir := range result.Dirs {
		resp.Dirs = append(resp.Dirs, &admin.CacheTreeSearchDir{
			Path:         dir.Path,
			NameMatch:    dir.NameMatch,
			Count:        dir.Count,
			Size:         dir.Size,
			MatchedCount: dir.MatchedCount,
			MatchedSize:  dir.MatchedSize,
			LastAccessAt: dir.LastAccessAt,
		})
	}
	return resp, nil
}

// TreePurge 按目录路径清除缓存对象：跳过被 pin 的对象并报出跳过条数。
func (c *Cache) TreePurge(ctx context.Context, req *admin.CacheTreePurgeRequest) (*admin.CacheTreePurgeResponse, error) {
	result, err := cache_svc.Cache().TreePurge(ctx, &cache_svc.TreePurgeRequest{Path: req.Path})
	if err != nil {
		return nil, err
	}
	return &admin.CacheTreePurgeResponse{Removed: result.Removed, Skipped: result.Skipped}, nil
}

// Images 按仓库列出 registry 上游里缓存过的镜像。
func (c *Cache) Images(ctx context.Context, req *admin.ListCacheImagesRequest) (*admin.ListCacheImagesResponse, error) {
	result, err := cache_svc.Cache().Images(ctx, &cache_svc.ImagesRequest{
		UpstreamID: req.UpstreamID, Keyword: req.Keyword, Offset: req.Offset, Size: req.Size})
	if err != nil {
		return nil, err
	}
	resp := &admin.ListCacheImagesResponse{
		Total:      result.Total,
		HasMore:    result.HasMore,
		NextOffset: result.NextOffset,
		List:       make([]*admin.CacheImageItem, 0, len(result.List)),
	}
	for _, image := range result.List {
		resp.List = append(resp.List, &admin.CacheImageItem{
			UpstreamID:   image.UpstreamID,
			Host:         image.Host,
			Repository:   image.Repository,
			TagCount:     image.TagCount,
			ObjectCount:  image.ObjectCount,
			PinnedCount:  image.PinnedCount,
			Size:         image.Size,
			HitCount:     image.HitCount,
			LastAccessAt: image.LastAccessAt,
			Tags:         toImageTags(image.Tags),
		})
	}
	return resp, nil
}

// ImageTags 列出一个镜像的 tag。
func (c *Cache) ImageTags(ctx context.Context, req *admin.ListCacheImageTagsRequest) (*admin.ListCacheImageTagsResponse, error) {
	result, err := cache_svc.Cache().ImageTags(ctx, &cache_svc.ImageTagsRequest{
		UpstreamID: req.UpstreamID, Repository: req.Repository, Keyword: req.Keyword})
	if err != nil {
		return nil, err
	}
	return &admin.ListCacheImageTagsResponse{List: toImageTags(result.List)}, nil
}

// ImagePurge 删除镜像或删除一个 tag：跳过被 pin 的对象并报出跳过条数。
func (c *Cache) ImagePurge(ctx context.Context, req *admin.PurgeCacheImageRequest) (*admin.PurgeCacheImageResponse, error) {
	result, err := cache_svc.Cache().ImagePurge(ctx, &cache_svc.ImagePurgeRequest{
		UpstreamID: req.UpstreamID, Repository: req.Repository, Reference: req.Reference})
	if err != nil {
		return nil, err
	}
	return &admin.PurgeCacheImageResponse{Removed: result.Removed, Skipped: result.Skipped}, nil
}

func toImageTags(tags []*cache_svc.ImageTag) []*admin.CacheImageTag {
	ret := make([]*admin.CacheImageTag, 0, len(tags))
	for _, tag := range tags {
		ret = append(ret, &admin.CacheImageTag{
			Reference:    tag.Reference,
			ByDigest:     tag.ByDigest,
			Digest:       tag.Digest,
			Variants:     tag.Variants,
			ObjectCount:  tag.ObjectCount,
			PinnedCount:  tag.PinnedCount,
			Pinned:       tag.Pinned,
			Expired:      tag.Expired,
			HitCount:     tag.HitCount,
			LastAccessAt: tag.LastAccessAt,
		})
	}
	return ret
}

func toTreeItem(object *cache_entity.CacheObject, host string) *admin.CacheTreeObjectItem {
	return &admin.CacheTreeObjectItem{CacheObjectItem: *toItem(object), Host: host}
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
