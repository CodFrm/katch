package cache_svc

import (
	"cmp"
	"context"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/cago-frame/cago/pkg/i18n"

	"github.com/CodFrm/katch/internal/model/entity/cache_entity"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/pkg/code"
	"github.com/CodFrm/katch/internal/proxy/registry"
	"github.com/CodFrm/katch/internal/repository/cache_repo"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
)

// 容器镜像视图（决策 7–11）：协议含 registry 的上游下的缓存对象按仓库归并。
//
// 仓库名按 registry 协议的最后一个动词段切出，开了 library_completion 的上游把
// 不含 / 的仓库名归并到 library/<名>——两条规则都直接用回源那一份
// （registry.SplitRepository / registry.CompleteLibrary），不在这里另写一遍。
//
// 「最后一个动词段」在 sqlite 与 MySQL 上没有共同的 SQL 写法（两边没有同一个反向
// 查找函数），所以镜像列表在 Go 里聚合：按主键分批扫 registry 上游的记录（只取
// 聚合用得上的列），内存里只留每个仓库一份合计和每个 manifest 引用一份合计。
// tag 列表与删除则先用任务 1 的前缀查询缩小范围，再按切分规则精确过滤。

const (
	// imageScanBatch 列镜像时一次从库里取多少条记录。
	imageScanBatch = 1000
	// imagePageSize 镜像列表默认每页的个数。
	imagePageSize = 50
	// maxImagePageSize 镜像列表每页的上限。
	maxImagePageSize = 200
	// maxImageRepositoryLen 仓库名的上限：键是 VARCHAR(500)，更长的仓库名不可能有缓存。
	maxImageRepositoryLen = 500
	// maxImageReferenceLen tag 或摘要的上限。
	maxImageReferenceLen = 256
)

// ImagesRequest 列镜像。UpstreamID 为 0 表示全部 registry 上游。
type ImagesRequest struct {
	UpstreamID int64
	Keyword    string
	Offset     int
	Size       int
}

// ImageTag 一个 manifest 引用（tag 或摘要）合并全部变体之后的一行。
type ImageTag struct {
	Reference string
	ByDigest  bool
	// Digest 最近访问的那个变体的内容摘要。
	Digest string
	// Variants 不同变体段的个数（含无变体），ObjectCount 是记录条数：
	// 同一变体在 x 与 library/x 两种写法下各有一条记录。
	Variants    int
	ObjectCount int64
	// PinnedCount 其中被 pin 的记录条数：删除 tag 的确认要写明「将清除的对象数（不含 pin）」。
	PinnedCount  int64
	Pinned       bool
	Expired      bool
	HitCount     int64
	LastAccessAt int64
}

// Image 一个仓库下全部缓存对象的合计。
type Image struct {
	UpstreamID   int64
	Host         string
	Repository   string
	TagCount     int
	ObjectCount  int64
	PinnedCount  int64
	Size         int64
	HitCount     int64
	LastAccessAt int64
	// Tags 只在关键字命中 tag（而不是仓库名）时给出命中的那些，否则为空。
	Tags []*ImageTag
}

// ImagesResponse 一页镜像。
type ImagesResponse struct {
	Total      int64
	HasMore    bool
	NextOffset int
	List       []*Image
}

// ImageTagsRequest 列一个镜像的 tag。
type ImageTagsRequest struct {
	UpstreamID int64
	Repository string
	Keyword    string
}

// ImageTagsResponse tag 行，按最后访问倒序。
type ImageTagsResponse struct {
	List []*ImageTag
}

// ImagePurgeRequest 删除镜像（Reference 为空）或删除一个 tag。
type ImagePurgeRequest struct {
	UpstreamID int64
	Repository string
	Reference  string
}

// ImagePurgeResponse 清掉了几条，以及因为被 pin 而跳过了几条。
type ImagePurgeResponse struct {
	Removed int64
	Skipped int64
}

// imageKey 一条缓存键在镜像视图里的归属。
type imageKey struct {
	repository string
	// reference 非空表示这是一条 /<仓库>/manifests/<引用> 记录。
	reference string
	variant   string
}

// parseImageKey 拆出键所属的仓库（已按上游开关补全）、manifest 引用与变体段。
// 拿不出仓库名的键（/_catalog 之类）不属于任何镜像。
func parseImageKey(key string, libraryCompletion bool) (imageKey, bool) {
	body, variant, _ := strings.Cut(key, variantMarker)
	// 查询串里可以有 /，甚至可以有 /manifests/，切分只看它之前的路径。
	path, _, _ := strings.Cut(body, "?")
	repository, tail := registry.SplitRepository(path)
	if repository == "" {
		return imageKey{}, false
	}
	ret := imageKey{repository: registry.CompleteLibrary(repository, libraryCompletion), variant: variant}
	if ref, ok := strings.CutPrefix(tail, "/manifests/"); ok && ref != "" && !strings.Contains(ref, "/") {
		if decoded, err := url.PathUnescape(ref); err == nil {
			ref = decoded
		}
		ret.reference = ref
	}
	return ret, true
}

// tagAggregate 一个引用下全部变体记录的合计。
type tagAggregate struct {
	tag      ImageTag
	variants map[string]bool
	latest   *cache_entity.CacheObject
}

func (a *tagAggregate) add(object *cache_entity.CacheObject, variant string) {
	a.variants[variant] = true
	a.tag.ObjectCount++
	a.tag.HitCount += object.HitCount
	if object.Pinned {
		a.tag.PinnedCount++
		a.tag.Pinned = true
	}
	if a.latest == nil || object.LastAccessAt > a.latest.LastAccessAt ||
		(object.LastAccessAt == a.latest.LastAccessAt && object.ID > a.latest.ID) {
		a.latest = object
	}
}

func (a *tagAggregate) result(now int64) *ImageTag {
	tag := a.tag
	tag.ByDigest = registryDigestReference(tag.Reference)
	tag.Variants = len(a.variants)
	tag.Digest = a.latest.Digest
	tag.LastAccessAt = a.latest.LastAccessAt
	tag.Expired = a.latest.Expired(now)
	return &tag
}

// tagSet 按引用合并的 tag 行。
type tagSet map[string]*tagAggregate

func (s tagSet) add(object *cache_entity.CacheObject, key imageKey) {
	agg, ok := s[key.reference]
	if !ok {
		agg = &tagAggregate{tag: ImageTag{Reference: key.reference}, variants: map[string]bool{}}
		s[key.reference] = agg
	}
	agg.add(object, key.variant)
}

// list 按最后访问倒序给出 tag 行；keyword 非空时只给引用含它（不区分大小写）的。
func (s tagSet) list(keyword string) []*ImageTag {
	now := time.Now().Unix()
	keyword = strings.ToLower(keyword)
	list := make([]*ImageTag, 0, len(s))
	for _, agg := range s {
		if keyword != "" && !strings.Contains(strings.ToLower(agg.tag.Reference), keyword) {
			continue
		}
		list = append(list, agg.result(now))
	}
	slices.SortFunc(list, func(a, b *ImageTag) int {
		if a.LastAccessAt != b.LastAccessAt {
			return cmp.Compare(b.LastAccessAt, a.LastAccessAt)
		}
		return strings.Compare(a.Reference, b.Reference)
	})
	return list
}

// imageAggregate 一个仓库的合计（决策 10：体积是仓库下全部对象之和）。
type imageAggregate struct {
	image Image
	tags  tagSet
}

func (c *cacheSvc) Images(ctx context.Context, req *ImagesRequest) (*ImagesResponse, error) {
	if len([]rune(req.Keyword)) > maxTreeKeywordLen {
		return nil, i18n.NewError(ctx, code.CacheTreeKeywordInvalid)
	}
	offset, size := max(req.Offset, 0), req.Size
	if size <= 0 || size > maxImagePageSize {
		size = imagePageSize
	}
	resp := &ImagesResponse{List: []*Image{}, NextOffset: offset}
	upstreams, err := registryUpstreams(ctx, req.UpstreamID)
	if err != nil {
		return nil, err
	}
	keyword := strings.ToLower(req.Keyword)
	all := make([]*Image, 0)
	for _, upstream := range upstreams {
		images, err := scanImages(ctx, upstream)
		if err != nil {
			return nil, err
		}
		for _, agg := range images {
			image := agg.image
			image.TagCount = len(agg.tags)
			image.Tags = []*ImageTag{}
			if keyword != "" && !strings.Contains(strings.ToLower(image.Repository), keyword) {
				// 仓库名没命中时只剩 tag 能命中；命中仓库名时不带 tag，由界面展开时取全部。
				image.Tags = agg.tags.list(keyword)
				if len(image.Tags) == 0 {
					continue
				}
			}
			all = append(all, &image)
		}
	}
	slices.SortFunc(all, func(a, b *Image) int {
		if a.LastAccessAt != b.LastAccessAt {
			return cmp.Compare(b.LastAccessAt, a.LastAccessAt)
		}
		if a.Host != b.Host {
			return strings.Compare(a.Host, b.Host)
		}
		return strings.Compare(a.Repository, b.Repository)
	})
	resp.Total = int64(len(all))
	if offset < len(all) {
		resp.List = all[offset:min(offset+size, len(all))]
	}
	resp.NextOffset = offset + len(resp.List)
	resp.HasMore = resp.NextOffset < len(all)
	return resp, nil
}

// registryUpstreams 镜像视图覆盖的上游：id 为 0 时是全部协议含 registry 的上游，
// 否则是那一条（不存在或不是 registry 时为空）。
func registryUpstreams(ctx context.Context, id int64) ([]*upstream_entity.Upstream, error) {
	var list []*upstream_entity.Upstream
	if id > 0 {
		upstream, err := upstream_repo.Upstream().Find(ctx, id)
		if err != nil {
			return nil, err
		}
		if upstream != nil {
			list = append(list, upstream)
		}
	} else {
		var err error
		if list, err = upstream_repo.Upstream().List(ctx); err != nil {
			return nil, err
		}
	}
	ret := make([]*upstream_entity.Upstream, 0, len(list))
	for _, upstream := range list {
		if upstream.Protocols.Has(upstream_entity.ProtocolRegistry) {
			ret = append(ret, upstream)
		}
	}
	return ret, nil
}

// scanImages 分批扫一个上游的全部记录，按仓库合计。
func scanImages(ctx context.Context, upstream *upstream_entity.Upstream) (map[string]*imageAggregate, error) {
	images := map[string]*imageAggregate{}
	afterID := int64(0)
	for {
		batch, err := cache_repo.CacheObject().ScanByUpstream(ctx, upstream.ID, afterID, imageScanBatch)
		if err != nil {
			return nil, err
		}
		for _, object := range batch {
			afterID = max(afterID, object.ID)
			key, ok := parseImageKey(object.Key, upstream.LibraryCompletion)
			if !ok {
				continue
			}
			agg, ok := images[key.repository]
			if !ok {
				agg = &imageAggregate{
					image: Image{UpstreamID: upstream.ID, Host: upstream.Host, Repository: key.repository},
					tags:  tagSet{},
				}
				images[key.repository] = agg
			}
			agg.image.ObjectCount++
			agg.image.Size += object.Size
			agg.image.HitCount += object.HitCount
			agg.image.LastAccessAt = max(agg.image.LastAccessAt, object.LastAccessAt)
			if object.Pinned {
				agg.image.PinnedCount++
			}
			if key.reference != "" {
				agg.tags.add(object, key)
			}
		}
		if len(batch) < imageScanBatch {
			return images, nil
		}
	}
}

func (c *cacheSvc) ImageTags(ctx context.Context, req *ImageTagsRequest) (*ImageTagsResponse, error) {
	if err := validateImageRepository(ctx, req.Repository); err != nil {
		return nil, err
	}
	if len([]rune(req.Keyword)) > maxTreeKeywordLen {
		return nil, i18n.NewError(ctx, code.CacheTreeKeywordInvalid)
	}
	resp := &ImageTagsResponse{List: []*ImageTag{}}
	upstream, err := findRegistryUpstream(ctx, req.UpstreamID)
	if err != nil || upstream == nil {
		return resp, err
	}
	objects, err := imageObjects(ctx, upstream, req.Repository, "manifests/")
	if err != nil {
		return nil, err
	}
	tags := tagSet{}
	for _, object := range objects {
		if object.key.reference != "" {
			tags.add(object.CacheObject, object.key)
		}
	}
	resp.List = tags.list(req.Keyword)
	return resp, nil
}

// ImagePurge 删除镜像 = 清除仓库下全部对象；删除 tag = 清除该引用的全部变体记录，
// 不连带层（决策 11）。两者都跳过 pin 并报数，删除循环与按上游清除是同一个。
func (c *cacheSvc) ImagePurge(ctx context.Context, req *ImagePurgeRequest) (*ImagePurgeResponse, error) {
	if err := validateImageRepository(ctx, req.Repository); err != nil {
		return nil, err
	}
	if req.Reference != "" && (len(req.Reference) > maxImageReferenceLen ||
		strings.Contains(req.Reference, "/") || req.Reference == "." || req.Reference == "..") {
		return nil, i18n.NewError(ctx, code.CacheImageReferenceInvalid)
	}
	resp := &ImagePurgeResponse{}
	upstream, err := findRegistryUpstream(ctx, req.UpstreamID)
	if err != nil || upstream == nil {
		return resp, err
	}
	scope := ""
	if req.Reference != "" {
		scope = "manifests/"
	}
	objects, err := imageObjects(ctx, upstream, req.Repository, scope)
	if err != nil {
		return nil, err
	}
	targets := make([]*cache_entity.CacheObject, 0, len(objects))
	for _, object := range objects {
		if req.Reference != "" && object.key.reference != req.Reference {
			continue
		}
		targets = append(targets, object.CacheObject)
	}
	removed, skipped, err := c.purgeUnpinned(ctx, targets)
	if err != nil {
		return nil, err
	}
	resp.Removed, resp.Skipped = removed, skipped
	return resp, nil
}

// findRegistryUpstream 按 id 取一条 registry 上游，不存在或不是 registry 时为 nil。
func findRegistryUpstream(ctx context.Context, id int64) (*upstream_entity.Upstream, error) {
	if id <= 0 {
		return nil, nil
	}
	list, err := registryUpstreams(ctx, id)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return list[0], nil
}

// validateImageRepository 仓库名为空、以 / 开头或结尾、含空段或 ..、超长的一律按参数
// 错误拒绝（通用一节），不拿去拼任何查询。
func validateImageRepository(ctx context.Context, repository string) error {
	if repository == "" || len(repository) > maxImageRepositoryLen {
		return i18n.NewError(ctx, code.CacheImageRepositoryInvalid)
	}
	for _, segment := range strings.Split(repository, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return i18n.NewError(ctx, code.CacheImageRepositoryInvalid)
		}
	}
	return nil
}

// keyedObject 一条记录与它在镜像视图里的归属。
type keyedObject struct {
	*cache_entity.CacheObject
	key imageKey
}

// imageObjects 取一个仓库（含决策 8 归并进来的写法）下的记录，scope 非空时只取
// /<仓库>/<scope> 之下的。前缀查询会把挂在仓库之下的别的仓库（x/sub）也带回来，
// 按切分规则再过滤一遍才是这个仓库自己的。
func imageObjects(ctx context.Context, upstream *upstream_entity.Upstream, repository, scope string) ([]keyedObject, error) {
	canonical := registry.CompleteLibrary(repository, upstream.LibraryCompletion)
	prefixes := []string{"/" + canonical + "/" + scope}
	if short, ok := strings.CutPrefix(canonical, "library/"); ok && upstream.LibraryCompletion && !strings.Contains(short, "/") {
		prefixes = append(prefixes, "/"+short+"/"+scope)
	}
	ret := make([]keyedObject, 0)
	for _, prefix := range prefixes {
		list, err := cache_repo.CacheObject().ListByPrefix(ctx, upstream.ID, prefix)
		if err != nil {
			return nil, err
		}
		for _, object := range list {
			key, ok := parseImageKey(object.Key, upstream.LibraryCompletion)
			if ok && key.repository == canonical {
				ret = append(ret, keyedObject{CacheObject: object, key: key})
			}
		}
	}
	return ret, nil
}
