package cache_svc

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/cago-frame/cago/pkg/i18n"

	"github.com/CodFrm/katch/internal/model/entity/cache_entity"
	"github.com/CodFrm/katch/internal/pkg/code"
	"github.com/CodFrm/katch/internal/repository/cache_repo"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
)

// 目录树（决策 1、3、4、5）：层级不落表，按请求现算。
//
// 对外的路径不以 / 开头，第一段是上游主机名，其余各段对应缓存键里查询串之前的
// 路径部分；键本身以 / 开头，所以 host/a/b 这个目录对应的键前缀是 /a/b/。
// 主机名不在键里，要经上游表换成 upstream_id。这里直接读 upstream_repo 而不是
// upstream_svc.FindByHost：后者只认启用中的上游，而停用上游的缓存对象照样占着盘。

const (
	// TreeKindDir 目录节点。
	TreeKindDir = "dir"
	// TreeKindObject 对象节点。
	TreeKindObject = "object"

	// maxTreePathLen 路径的上限：主机名 253 加上 key 的 500，留出余量。
	maxTreePathLen = 1024
	// maxTreeKeywordLen 搜索词的上限，与对象搜索一致。
	maxTreeKeywordLen = 256
	// treePageSize 一层每次最多返回的子项数（决策 4）。
	treePageSize = 200
	// treeSearchLimit 一次搜索最多返回的对象数（决策 5）。
	treeSearchLimit = 200
)

// TreeRequest 列目录树的一层。
type TreeRequest struct {
	Path   string
	Offset int
}

// TreeNode 一层里的一个子项。
type TreeNode struct {
	Kind         string
	Name         string
	Path         string
	Count        int64
	PinnedCount  int64
	Size         int64
	LastAccessAt int64
	Variant      bool
	Host         string
	Object       *cache_entity.CacheObject
}

// TreeResponse 一层的子项与当前目录的合计。
type TreeResponse struct {
	Path        string
	TotalCount  int64
	TotalPinned int64
	TotalSize   int64
	Children    []*TreeNode
	HasMore     bool
	NextOffset  int
}

// TreeSearchRequest 在目录下搜索。
type TreeSearchRequest struct {
	Path    string
	Keyword string
}

// TreeSearchObject 一个匹配的对象。
type TreeSearchObject struct {
	Name    string
	Path    string
	Variant bool
	Host    string
	Object  *cache_entity.CacheObject
}

// TreeSearchDir 匹配对象的一个上级目录的合计。
type TreeSearchDir struct {
	Path         string
	NameMatch    bool
	Count        int64
	Size         int64
	MatchedCount int64
	MatchedSize  int64
	LastAccessAt int64
}

// TreeSearchResponse 搜索结果。
type TreeSearchResponse struct {
	Path      string
	Matched   int64
	Truncated bool
	Objects   []*TreeSearchObject
	Dirs      []*TreeSearchDir
}

func (c *cacheSvc) Tree(ctx context.Context, req *TreeRequest) (*TreeResponse, error) {
	segments, err := parseTreePath(ctx, req.Path)
	if err != nil {
		return nil, err
	}
	offset := max(req.Offset, 0)
	resp := &TreeResponse{Path: req.Path, Children: []*TreeNode{}, NextOffset: offset}
	if len(segments) == 0 {
		return c.treeRoot(ctx, resp, offset)
	}
	upstream, err := upstream_repo.Upstream().FindByHost(ctx, segments[0])
	if err != nil {
		return nil, err
	}
	if upstream == nil {
		// 指向不存在的目录的链接（例如刚被清空）显示空状态，而不是报错。
		return resp, nil
	}
	repo := cache_repo.CacheObject()
	prefix := treeKeyPrefix(segments)
	stat, err := repo.StatByPrefix(ctx, upstream.ID, prefix)
	if err != nil {
		return nil, err
	}
	resp.TotalCount, resp.TotalPinned, resp.TotalSize = stat.Count, stat.Pinned, stat.Size
	// 目录在前、对象在后，偏移跨两段连续计：先把目录那段吃完，剩下的名额给对象。
	if int64(offset) < stat.Dirs {
		dirs, err := repo.ListTreeDirs(ctx, &cache_entity.TreeOption{
			UpstreamID: upstream.ID, Prefix: prefix, Offset: offset, Limit: treePageSize})
		if err != nil {
			return nil, err
		}
		for _, dir := range dirs {
			resp.Children = append(resp.Children, &TreeNode{
				Kind: TreeKindDir, Name: dir.Name, Path: req.Path + "/" + dir.Name,
				Count: dir.Count, PinnedCount: dir.Pinned, Size: dir.Size, LastAccessAt: dir.LastAccessAt,
			})
		}
	}
	objectOffset := max(int64(offset)-stat.Dirs, 0)
	if remain := treePageSize - len(resp.Children); remain > 0 && objectOffset < stat.Objects {
		objects, err := repo.ListTreeObjects(ctx, &cache_entity.TreeOption{
			UpstreamID: upstream.ID, Prefix: prefix, Offset: int(objectOffset), Limit: remain})
		if err != nil {
			return nil, err
		}
		for _, object := range objects {
			_, leaf, variant := splitTreeKey(object.Key)
			resp.Children = append(resp.Children, &TreeNode{
				Kind: TreeKindObject, Name: leaf, Path: req.Path + "/" + leaf,
				Variant: variant, Host: upstream.Host, Object: object,
			})
		}
	}
	resp.NextOffset = offset + len(resp.Children)
	// 合计与列表是两次查询，中间对象可能被并发清掉；一批什么都没拿到时仍报「还有更多」，
	// 界面上的「加载更多」就会原地空转。
	resp.HasMore = len(resp.Children) > 0 && int64(resp.NextOffset) < stat.Dirs+stat.Objects
	return resp, nil
}

// treeRoot 树根：每个有缓存对象的上游一个目录，按主机名排序。上游只有几个到几十个，
// 在内存里排序分页就够了。
func (c *cacheSvc) treeRoot(ctx context.Context, resp *TreeResponse, offset int) (*TreeResponse, error) {
	stats, err := cache_repo.CacheObject().StatByUpstream(ctx)
	if err != nil {
		return nil, err
	}
	hosts, err := upstreamHosts(ctx)
	if err != nil {
		return nil, err
	}
	all := make([]*TreeNode, 0, len(stats))
	for _, stat := range stats {
		host, ok := hosts[stat.UpstreamID]
		if !ok {
			// 上游删掉之后留下的记录没有主机名可挂，树上放不下它们。
			continue
		}
		all = append(all, &TreeNode{Kind: TreeKindDir, Name: host, Path: host,
			Count: stat.Count, PinnedCount: stat.Pinned, Size: stat.Size, LastAccessAt: stat.LastAccessAt})
		resp.TotalCount += stat.Count
		resp.TotalPinned += stat.Pinned
		resp.TotalSize += stat.Size
	}
	slices.SortFunc(all, func(a, b *TreeNode) int { return strings.Compare(a.Name, b.Name) })
	if offset < len(all) {
		resp.Children = append(resp.Children, all[offset:min(offset+treePageSize, len(all))]...)
	}
	resp.NextOffset = offset + len(resp.Children)
	resp.HasMore = resp.NextOffset < len(all)
	return resp, nil
}

func (c *cacheSvc) TreeSearch(ctx context.Context, req *TreeSearchRequest) (*TreeSearchResponse, error) {
	segments, err := parseTreePath(ctx, req.Path)
	if err != nil {
		return nil, err
	}
	if req.Keyword == "" || len([]rune(req.Keyword)) > maxTreeKeywordLen {
		return nil, i18n.NewError(ctx, code.CacheTreeKeywordInvalid)
	}
	resp := &TreeSearchResponse{Path: req.Path, Objects: []*TreeSearchObject{}, Dirs: []*TreeSearchDir{}}
	keyword := strings.ToLower(req.Keyword)
	opt := &cache_entity.TreeSearchOption{Keyword: req.Keyword, Limit: treeSearchLimit}
	var hosts map[int64]string
	hostMatched := map[int64]bool{}
	searchPrefix := ""
	if len(segments) == 0 {
		// 根上搜索即全站搜索，搜的是主机名以下的整段路径，所以主机名本身也参与匹配。
		if hosts, err = upstreamHosts(ctx); err != nil {
			return nil, err
		}
		for id, host := range hosts {
			opt.UpstreamIDs = append(opt.UpstreamIDs, id)
			if strings.Contains(strings.ToLower(host), keyword) {
				opt.HostMatched = append(opt.HostMatched, id)
				hostMatched[id] = true
			}
		}
		slices.Sort(opt.UpstreamIDs)
		slices.Sort(opt.HostMatched)
	} else {
		upstream, err := upstream_repo.Upstream().FindByHost(ctx, segments[0])
		if err != nil {
			return nil, err
		}
		if upstream == nil {
			return resp, nil
		}
		hosts = map[int64]string{upstream.ID: upstream.Host}
		searchPrefix = treeKeyPrefix(segments)
		opt.UpstreamID, opt.Prefix = upstream.ID, searchPrefix
	}
	repo := cache_repo.CacheObject()
	objects, total, err := repo.SearchTree(ctx, opt)
	if err != nil {
		return nil, err
	}
	resp.Matched = total
	resp.Truncated = total > int64(len(objects))
	type dirRef struct {
		upstreamID int64
		prefix     string
		nameMatch  bool
	}
	dirs := map[string]dirRef{}
	for _, object := range objects {
		host := hosts[object.UpstreamID]
		dirNames, leaf, variant := splitTreeKey(object.Key)
		path := host + "/" + strings.Join(append(slices.Clone(dirNames), leaf), "/")
		resp.Objects = append(resp.Objects, &TreeSearchObject{
			Name: leaf, Path: path, Variant: variant, Host: host, Object: object})
		// 上级目录从搜索所在目录的下一层开始给：它们是结果树里要自动展开的那些行。
		if len(segments) == 0 {
			dirs[host] = dirRef{upstreamID: object.UpstreamID, prefix: "/", nameMatch: hostMatched[object.UpstreamID]}
		}
		for i := max(len(segments)-1, 0); i < len(dirNames); i++ {
			dirs[host+"/"+strings.Join(dirNames[:i+1], "/")] = dirRef{
				upstreamID: object.UpstreamID,
				prefix:     "/" + strings.Join(dirNames[:i+1], "/") + "/",
				nameMatch:  strings.Contains(strings.ToLower(dirNames[i]), keyword),
			}
		}
	}
	for _, path := range slices.Sorted(maps.Keys(dirs)) {
		ref := dirs[path]
		stat, err := repo.StatTreeMatch(ctx, &cache_entity.TreeMatchOption{
			UpstreamID: ref.upstreamID, Prefix: ref.prefix, SearchPrefix: searchPrefix,
			Keyword: req.Keyword, AllMatched: hostMatched[ref.upstreamID],
		})
		if err != nil {
			return nil, err
		}
		resp.Dirs = append(resp.Dirs, &TreeSearchDir{
			Path: path, NameMatch: ref.nameMatch, Count: stat.Count, Size: stat.Size,
			MatchedCount: stat.MatchedCount, MatchedSize: stat.MatchedSize, LastAccessAt: stat.LastAccessAt,
		})
	}
	return resp, nil
}

// parseTreePath 把对外路径拆成段。空串是根；以 / 开头或结尾、含空段或 ..、超长的
// 一律按参数错误拒绝（通用一节），不拿去拼任何查询。
func parseTreePath(ctx context.Context, path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	if len(path) > maxTreePathLen {
		return nil, i18n.NewError(ctx, code.CacheTreePathInvalid)
	}
	segments := strings.Split(path, "/")
	for _, segment := range segments {
		if segment == "" || segment == ".." {
			return nil, i18n.NewError(ctx, code.CacheTreePathInvalid)
		}
	}
	return segments, nil
}

// treeKeyPrefix 主机名之后的各段对应的键前缀，以 / 开头也以 / 结尾。
func treeKeyPrefix(segments []string) string {
	if len(segments) <= 1 {
		return "/"
	}
	return "/" + strings.Join(segments[1:], "/") + "/"
}

// splitTreeKey 把缓存键拆成目录各段、叶子名与是否带变体。
//
// 分段只看查询串之前的路径部分（决策 3）：查询串里的 / 不成目录。变体段剥掉，
// 以标记的形式给出；查询串留在叶子名上，它确实区分了两个对象。
func splitTreeKey(key string) ([]string, string, bool) {
	body, variant := key, false
	if i := strings.Index(body, variantMarker); i >= 0 {
		body, variant = body[:i], true
	}
	pathEnd := len(body)
	if i := strings.IndexByte(body, '?'); i >= 0 {
		pathEnd = i
	}
	slash := strings.LastIndexByte(body[:pathEnd], '/')
	if slash <= 0 {
		return nil, body[slash+1:], variant
	}
	return strings.Split(body[1:slash], "/"), body[slash+1:], variant
}

// upstreamHosts upstream_id 到主机名。
func upstreamHosts(ctx context.Context) (map[int64]string, error) {
	list, err := upstream_repo.Upstream().List(ctx)
	if err != nil {
		return nil, err
	}
	hosts := make(map[int64]string, len(list))
	for _, upstream := range list {
		hosts[upstream.ID] = upstream.Host
	}
	return hosts, nil
}
