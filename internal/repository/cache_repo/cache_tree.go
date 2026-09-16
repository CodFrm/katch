package cache_repo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/cago-frame/cago/database/db"

	"github.com/CodFrm/katch/internal/model/entity/cache_entity"
)

// ErrTreePrefix 目录前缀必须以 / 开头也以 / 结尾。
var ErrTreePrefix = errors.New("目录前缀必须以 / 开头和结尾")

// 目录树的 SQL 只用 sqlite 与 MySQL 都有、且语义一致的那几个函数：SUBSTR、INSTR、
// LOWER、COALESCE、CASE。它们按字符而不是按字节计位置，所以前缀长度按字符数算。
//
// 查询串里的 `?` 一律以参数传入：gorm 拼 SQL 时会把字面量里的 `?` 也当成占位符。

// likeEscaper 转义 LIKE 的通配符。转义符取 ! 而不是 \：MySQL 的字符串字面量里 \
// 自己还要再转义一层，sqlite 不用，同一条 SQL 在两边的意思就不一样了。
var likeEscaper = strings.NewReplacer("!", "!!", "%", "!%", "_", "!_")

// treePrefix 键前缀条件。
//
// 范围条件让 (upstream_id, key) 这条唯一索引用得上（sqlite 的 LIKE 不走索引），
// 也让 sqlite 下的前缀按字节精确匹配；LIKE 则在大小写不敏感的 MySQL 排序规则下
// 把口径钉回「以这个前缀开头」——那里的范围比较按排序规则走，/ 与 0 之间还夹着
// 别的标点。前缀以 / 结尾，/ 的下一个字节是 0，所以上界就是把末尾的 / 换成 0。
func treePrefix(upstreamID int64, prefix string) (string, []any, error) {
	if !strings.HasPrefix(prefix, "/") || !strings.HasSuffix(prefix, "/") {
		return "", nil, ErrTreePrefix
	}
	upper := prefix[:len(prefix)-1] + "0"
	return "upstream_id=? AND `key`>=? AND `key`<? AND `key` LIKE ? ESCAPE '!'",
		[]any{upstreamID, prefix, upper, likeEscaper.Replace(prefix) + "%"}, nil
}

// treeRest 键里前缀之后的那一段。
func treeRest(prefix string) string {
	return fmt.Sprintf("SUBSTR(`key`,%d)", utf8.RuneCountInString(prefix)+1)
}

// treeDirCond 这条记录落在前缀下的某个子目录里：剩下的部分里有 /，且在查询串之前。
//
// 变体段（0x1F 之后）里没有 /，不必单独切；也不在 SQL 里找 0x1F——MySQL 的排序规则
// 把控制字节当成零权重，INSTR 找它的结果靠不住。
func treeDirCond(rest string) (string, []any) {
	slash := "INSTR(" + rest + ",'/')"
	query := "INSTR(" + rest + ",?)"
	return slash + ">0 AND (" + query + "=0 OR " + slash + "<" + query + ")", []any{"?", "?"}
}

// treeDirName 子目录名：剩下的部分里第一个 / 之前。
func treeDirName(rest string) string {
	return "SUBSTR(" + rest + ",1,INSTR(" + rest + ",'/')-1)"
}

// treeMatch 关键字匹配条件：剩下的部分转小写后做子串匹配。
func treeMatch(rest, keyword string) (string, any) {
	return "LOWER(" + rest + ") LIKE ? ESCAPE '!'", "%" + likeEscaper.Replace(strings.ToLower(keyword)) + "%"
}

// treeAggregate 目录合计的几列。pinned 用 CASE 数而不是直接 SUM：布尔列在两边的
// 存储形态不一样，CASE 的口径两边一致。
const treeAggregate = "COUNT(*) AS count," +
	"COALESCE(SUM(CASE WHEN pinned THEN 1 ELSE 0 END),0) AS pinned," +
	"COALESCE(SUM(size),0) AS size,COALESCE(MAX(last_access_at),0) AS last_access_at"

func (c *cacheObjectRepo) StatByUpstream(ctx context.Context) ([]*cache_entity.UpstreamTreeStat, error) {
	list := make([]*cache_entity.UpstreamTreeStat, 0)
	if err := db.Ctx(ctx).Model(&cache_entity.CacheObject{}).
		Select("upstream_id," + treeAggregate).
		Group("upstream_id").
		Scan(&list).Error; err != nil {
		return nil, err
	}
	return list, nil
}

func (c *cacheObjectRepo) StatByPrefix(ctx context.Context, upstreamID int64, prefix string) (*cache_entity.PrefixTreeStat, error) {
	where, whereArgs, err := treePrefix(upstreamID, prefix)
	if err != nil {
		return nil, err
	}
	rest := treeRest(prefix)
	dir, dirArgs := treeDirCond(rest)
	sql := "SELECT " + treeAggregate + "," +
		"COALESCE(SUM(CASE WHEN " + dir + " THEN 0 ELSE 1 END),0) AS objects," +
		"COUNT(DISTINCT CASE WHEN " + dir + " THEN " + treeDirName(rest) + " END) AS dirs " +
		"FROM `cache_objects` WHERE " + where
	args := make([]any, 0, 2*len(dirArgs)+len(whereArgs))
	args = append(args, dirArgs...)
	args = append(args, dirArgs...)
	args = append(args, whereArgs...)
	ret := &cache_entity.PrefixTreeStat{}
	if err := db.Ctx(ctx).Raw(sql, args...).Scan(ret).Error; err != nil {
		return nil, err
	}
	return ret, nil
}

func (c *cacheObjectRepo) ListTreeDirs(ctx context.Context, opt *cache_entity.TreeOption) ([]*cache_entity.TreeDir, error) {
	where, whereArgs, err := treePrefix(opt.UpstreamID, opt.Prefix)
	if err != nil {
		return nil, err
	}
	rest := treeRest(opt.Prefix)
	dir, dirArgs := treeDirCond(rest)
	name := treeDirName(rest)
	sql := "SELECT " + name + " AS name," + treeAggregate +
		" FROM `cache_objects` WHERE " + where + " AND " + dir +
		" GROUP BY " + name + " ORDER BY name LIMIT ? OFFSET ?"
	args := make([]any, 0, len(whereArgs)+len(dirArgs)+2)
	args = append(args, whereArgs...)
	args = append(args, dirArgs...)
	args = append(args, opt.Limit, opt.Offset)
	list := make([]*cache_entity.TreeDir, 0)
	if err := db.Ctx(ctx).Raw(sql, args...).Scan(&list).Error; err != nil {
		return nil, err
	}
	return list, nil
}

func (c *cacheObjectRepo) ListTreeObjects(ctx context.Context, opt *cache_entity.TreeOption) ([]*cache_entity.CacheObject, error) {
	where, whereArgs, err := treePrefix(opt.UpstreamID, opt.Prefix)
	if err != nil {
		return nil, err
	}
	dir, dirArgs := treeDirCond(treeRest(opt.Prefix))
	list := make([]*cache_entity.CacheObject, 0)
	// 按键排序：同一层里键的前缀相同，键序就是名称序，同一对象的几个变体也挨在一起。
	if err := db.Ctx(ctx).Where(where, whereArgs...).Where("NOT ("+dir+")", dirArgs...).
		Order("`key`").Offset(opt.Offset).Limit(opt.Limit).Find(&list).Error; err != nil {
		return nil, err
	}
	return list, nil
}

func (c *cacheObjectRepo) SearchTree(ctx context.Context, opt *cache_entity.TreeSearchOption) ([]*cache_entity.CacheObject, int64, error) {
	list := make([]*cache_entity.CacheObject, 0)
	query := db.Ctx(ctx).Model(&cache_entity.CacheObject{})
	if opt.UpstreamID > 0 {
		where, whereArgs, err := treePrefix(opt.UpstreamID, opt.Prefix)
		if err != nil {
			return nil, 0, err
		}
		match, matchArg := treeMatch(treeRest(opt.Prefix), opt.Keyword)
		query = query.Where(where, whereArgs...).Where(match, matchArg)
	} else {
		// 根上搜索限定在已知上游里：上游删掉之后留下的记录在树上没有位置。
		if len(opt.UpstreamIDs) == 0 {
			return list, 0, nil
		}
		match, matchArg := treeMatch(treeRest(""), opt.Keyword)
		query = query.Where("upstream_id IN ?", opt.UpstreamIDs)
		if len(opt.HostMatched) > 0 {
			query = query.Where("upstream_id IN ? OR "+match, opt.HostMatched, matchArg)
		} else {
			query = query.Where(match, matchArg)
		}
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	if err := query.Order("upstream_id,`key`").Limit(opt.Limit).Find(&list).Error; err != nil {
		return nil, 0, err
	}
	return list, total, nil
}

// ListByPrefix 前缀下（递归到底）全部对象：与 ListTreeObjects 相比不排除子目录，
// 按目录清除要连子目录里的对象一起收走。
func (c *cacheObjectRepo) ListByPrefix(ctx context.Context, upstreamID int64, prefix string) ([]*cache_entity.CacheObject, error) {
	where, whereArgs, err := treePrefix(upstreamID, prefix)
	if err != nil {
		return nil, err
	}
	list := make([]*cache_entity.CacheObject, 0)
	if err := db.Ctx(ctx).Where(where, whereArgs...).Find(&list).Error; err != nil {
		return nil, err
	}
	return list, nil
}

func (c *cacheObjectRepo) StatTreeMatch(ctx context.Context, opt *cache_entity.TreeMatchOption) (*cache_entity.TreeMatchStat, error) {
	where, whereArgs, err := treePrefix(opt.UpstreamID, opt.Prefix)
	if err != nil {
		return nil, err
	}
	match, matchArgs := "1=1", []any(nil)
	if !opt.AllMatched {
		cond, arg := treeMatch(treeRest(opt.SearchPrefix), opt.Keyword)
		match, matchArgs = cond, []any{arg}
	}
	sql := "SELECT COUNT(*) AS count,COALESCE(SUM(size),0) AS size," +
		"COALESCE(MAX(last_access_at),0) AS last_access_at," +
		"COALESCE(SUM(CASE WHEN " + match + " THEN 1 ELSE 0 END),0) AS matched_count," +
		"COALESCE(SUM(CASE WHEN " + match + " THEN size ELSE 0 END),0) AS matched_size " +
		"FROM `cache_objects` WHERE " + where
	args := make([]any, 0, 2*len(matchArgs)+len(whereArgs))
	args = append(args, matchArgs...)
	args = append(args, matchArgs...)
	args = append(args, whereArgs...)
	ret := &cache_entity.TreeMatchStat{}
	if err := db.Ctx(ctx).Raw(sql, args...).Scan(ret).Error; err != nil {
		return nil, err
	}
	return ret, nil
}
