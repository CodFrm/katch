package cache_repo

import (
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/cago-frame/cago/pkg/utils/testutils"
	"github.com/smartystreets/goconvey/convey"

	"github.com/CodFrm/katch/internal/model/entity/cache_entity"
)

// 目录树的查询全在 SQL 里聚合（决策 1），所以这里要钉住的是 SQL 的形状：
// 前缀的上下界与 LIKE 转义、分段只看查询串之前的 `/`、按名称排序与分页。

// variantLike 变体段（0x1F 之后的 accept= 加 16 位摘要）在键尾的 LIKE 形状。
const variantLike = "%accept=________________"

// matchSQL 关键字匹配条件在 SQL 里的样子：rest 里含关键字，且带变体段的键要在
// 变体段之前就含——变体段的摘要不是名字的一部分，界面上也看不见它。
func matchSQL(rest string) string {
	return "LOWER\\(" + rest + "\\) LIKE \\? ESCAPE '!' AND \\(`key` NOT LIKE \\? ESCAPE '!' OR LOWER\\(" +
		rest + "\\) LIKE \\? ESCAPE '!'\\)"
}

// prefixWhere 前缀条件在 SQL 里的样子：范围给索引用，LIKE 给大小写不敏感的 MySQL 保证口径。
const prefixWhere = "upstream_id=\\? AND `key`>=\\? AND `key`<\\? AND `key` LIKE \\? ESCAPE '!'"

func TestCacheObjectRepo_StatByUpstream(t *testing.T) {
	convey.Convey("树根按上游合计对象数、pin 数、体积与最近访问", t, func() {
		ctx, _, mock := testutils.Database(t)
		mock.ExpectQuery(regexp.QuoteMeta("SELECT upstream_id,COUNT(*) AS count," +
			"COALESCE(SUM(CASE WHEN pinned THEN 1 ELSE 0 END),0) AS pinned," +
			"COALESCE(SUM(size),0) AS size,COALESCE(MAX(last_access_at),0) AS last_access_at " +
			"FROM `cache_objects` GROUP BY `upstream_id`")).
			WillReturnRows(sqlmock.NewRows([]string{"upstream_id", "count", "pinned", "size", "last_access_at"}).
				AddRow(7, 3, 1, 300, 1700).AddRow(8, 1, 0, 10, 1600))

		got, err := NewCacheObject().StatByUpstream(ctx)
		convey.So(err, convey.ShouldBeNil)
		convey.So(len(got), convey.ShouldEqual, 2)
		convey.So(*got[0], convey.ShouldResemble, cache_entity.UpstreamTreeStat{
			UpstreamID: 7, Count: 3, Pinned: 1, Size: 300, LastAccessAt: 1700})
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}

func TestCacheObjectRepo_StatByPrefix(t *testing.T) {
	convey.Convey("按前缀合计一层：总数与直接子目录数、直接对象数", t, func() {
		ctx, _, mock := testutils.Database(t)
		// 前缀里的 % _ 与转义符自身都要转义，否则 100% 这个目录会把 1000 也算进来。
		// SUBSTR 的起点按前缀的字符数算：/pool/100%_x!/ 是 14 个字符。
		rest := "SUBSTR\\(`key`,15\\)"
		dir := "INSTR\\(" + rest + ",'/'\\)>0 AND \\(INSTR\\(" + rest + ",\\?\\)=0 OR INSTR\\(" + rest +
			",'/'\\)<INSTR\\(" + rest + ",\\?\\)\\)"
		mock.ExpectQuery("SELECT COUNT\\(\\*\\) AS count,.*"+
			"COALESCE\\(SUM\\(CASE WHEN "+dir+" THEN 0 ELSE 1 END\\),0\\) AS objects,"+
			"COUNT\\(DISTINCT CASE WHEN "+dir+" THEN SUBSTR\\("+rest+",1,INSTR\\("+rest+",'/'\\)-1\\) END\\) AS dirs "+
			"FROM `cache_objects` WHERE "+prefixWhere+"$").
			WithArgs("?", "?", "?", "?", int64(7), "/pool/100%_x!/", "/pool/100%_x!0", "/pool/100!%!_x!!/%").
			WillReturnRows(sqlmock.NewRows([]string{"count", "pinned", "size", "last_access_at", "objects", "dirs"}).
				AddRow(5, 2, 500, 1800, 3, 1))

		got, err := NewCacheObject().StatByPrefix(ctx, 7, "/pool/100%_x!/")
		convey.So(err, convey.ShouldBeNil)
		convey.So(*got, convey.ShouldResemble, cache_entity.PrefixTreeStat{
			Count: 5, Pinned: 2, Size: 500, LastAccessAt: 1800, Objects: 3, Dirs: 1})
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})

	convey.Convey("前缀必须以 / 开头和结尾，否则上界算不出来", t, func() {
		ctx, _, _ := testutils.Database(t)
		_, err := NewCacheObject().StatByPrefix(ctx, 7, "/pool")
		convey.So(err, convey.ShouldEqual, ErrTreePrefix)
	})
}

func TestCacheObjectRepo_ListTreeDirs(t *testing.T) {
	convey.Convey("列一层的子目录：按名称排序、分页、每个目录递归合计", t, func() {
		ctx, _, mock := testutils.Database(t)
		rest := "SUBSTR\\(`key`,2\\)"
		name := "SUBSTR\\(" + rest + ",1,INSTR\\(" + rest + ",'/'\\)-1\\)"
		mock.ExpectQuery("SELECT "+name+" AS name,COUNT\\(\\*\\) AS count,.*"+
			"FROM `cache_objects` WHERE "+prefixWhere+" AND INSTR\\("+rest+",'/'\\)>0 AND .*"+
			"GROUP BY "+name+" ORDER BY name LIMIT \\? OFFSET \\?$").
			WithArgs(int64(7), "/", "0", "/%", "?", "?", 200, 400).
			WillReturnRows(sqlmock.NewRows([]string{"name", "count", "pinned", "size", "last_access_at"}).
				AddRow("dists", 2, 0, 20, 900).AddRow("pool", 9, 1, 9000, 1000))

		got, err := NewCacheObject().ListTreeDirs(ctx, &cache_entity.TreeOption{
			UpstreamID: 7, Prefix: "/", Offset: 400, Limit: 200})
		convey.So(err, convey.ShouldBeNil)
		convey.So(len(got), convey.ShouldEqual, 2)
		convey.So(*got[1], convey.ShouldResemble, cache_entity.TreeDir{
			Name: "pool", Count: 9, Pinned: 1, Size: 9000, LastAccessAt: 1000})
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}

func TestCacheObjectRepo_ListTreeObjects(t *testing.T) {
	convey.Convey("列一层的对象：不属于任何子目录的记录，按键排序", t, func() {
		ctx, _, mock := testutils.Database(t)
		rest := "SUBSTR\\(`key`,7\\)"
		mock.ExpectQuery("SELECT \\* FROM `cache_objects` WHERE \\("+prefixWhere+"\\) AND \\(NOT \\(INSTR\\("+
			rest+",'/'\\)>0 AND .*\\)\\) ORDER BY `key` LIMIT \\? OFFSET \\?$").
			WithArgs(int64(7), "/pool/", "/pool0", "/pool/%", "?", "?", 50, 10).
			WillReturnRows(sqlmock.NewRows([]string{"id", "key"}).AddRow(3, "/pool/a.deb?x=/y"))

		got, err := NewCacheObject().ListTreeObjects(ctx, &cache_entity.TreeOption{
			UpstreamID: 7, Prefix: "/pool/", Offset: 10, Limit: 50})
		convey.So(err, convey.ShouldBeNil)
		convey.So(len(got), convey.ShouldEqual, 1)
		convey.So(got[0].Key, convey.ShouldEqual, "/pool/a.deb?x=/y")
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})
}

func TestCacheObjectRepo_SearchTree(t *testing.T) {
	convey.Convey("在目录下搜索", t, func() {
		ctx, _, mock := testutils.Database(t)
		repo := NewCacheObject()

		convey.Convey("前缀之下的部分做不区分大小写的子串匹配，关键字里的通配符被转义", func() {
			match := "\\(" + matchSQL("SUBSTR\\(`key`,7\\)") + "\\)"
			mock.ExpectQuery("SELECT count\\(\\*\\) FROM `cache_objects` WHERE \\("+prefixWhere+"\\) AND "+match+"$").
				WithArgs(int64(7), "/pool/", "/pool0", "/pool/%", "%redis!_7%", variantLike, "%redis!_7%"+variantLike).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(321))
			mock.ExpectQuery("SELECT \\* FROM `cache_objects` WHERE \\("+prefixWhere+"\\) AND "+match+
				" ORDER BY upstream_id,`key` LIMIT \\?$").
				WithArgs(int64(7), "/pool/", "/pool0", "/pool/%", "%redis!_7%", variantLike, "%redis!_7%"+variantLike, 200).
				WillReturnRows(sqlmock.NewRows([]string{"id", "key"}).AddRow(1, "/pool/redis_7.deb"))

			list, total, err := repo.SearchTree(ctx, &cache_entity.TreeSearchOption{
				UpstreamID: 7, Prefix: "/pool/", Keyword: "Redis_7", Limit: 200})
			convey.So(err, convey.ShouldBeNil)
			convey.So(total, convey.ShouldEqual, 321)
			convey.So(len(list), convey.ShouldEqual, 1)
			convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
		})

		convey.Convey("根上搜索限定在已知上游里，主机名匹配的上游整体算命中", func() {
			match := matchSQL("SUBSTR\\(`key`,1\\)")
			mock.ExpectQuery("SELECT count\\(\\*\\) FROM `cache_objects` WHERE upstream_id IN \\(\\?,\\?\\) AND "+
				"\\(upstream_id IN \\(\\?\\) OR \\("+match+"\\)\\)$").
				WithArgs(int64(7), int64(8), int64(8), "%debian%", variantLike, "%debian%"+variantLike).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
			mock.ExpectQuery("SELECT \\* FROM `cache_objects` WHERE .* ORDER BY upstream_id,`key` LIMIT \\?$").
				WillReturnRows(sqlmock.NewRows([]string{"id", "key"}).AddRow(1, "/a").AddRow(2, "/b"))

			list, total, err := repo.SearchTree(ctx, &cache_entity.TreeSearchOption{
				UpstreamIDs: []int64{7, 8}, HostMatched: []int64{8}, Keyword: "debian", Limit: 200})
			convey.So(err, convey.ShouldBeNil)
			convey.So(total, convey.ShouldEqual, 2)
			convey.So(len(list), convey.ShouldEqual, 2)
			convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
		})

		convey.Convey("没有任何已知上游时不查库", func() {
			list, total, err := repo.SearchTree(ctx, &cache_entity.TreeSearchOption{Keyword: "x", Limit: 200})
			convey.So(err, convey.ShouldBeNil)
			convey.So(total, convey.ShouldEqual, 0)
			convey.So(len(list), convey.ShouldEqual, 0)
			convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
		})
	})
}

func TestCacheObjectRepo_ListByPrefix(t *testing.T) {
	convey.Convey("按前缀取整棵子树下的全部对象（不排除子目录），供按目录清除用", t, func() {
		ctx, _, mock := testutils.Database(t)
		mock.ExpectQuery("SELECT \\* FROM `cache_objects` WHERE "+prefixWhere+"$").
			WithArgs(int64(7), "/pool/", "/pool0", "/pool/%").
			WillReturnRows(sqlmock.NewRows([]string{"id", "key"}).
				AddRow(1, "/pool/a.deb").AddRow(2, "/pool/main/b.deb"))

		got, err := NewCacheObject().ListByPrefix(ctx, 7, "/pool/")
		convey.So(err, convey.ShouldBeNil)
		convey.So(len(got), convey.ShouldEqual, 2)
		convey.So(got[0].Key, convey.ShouldEqual, "/pool/a.deb")
		convey.So(got[1].Key, convey.ShouldEqual, "/pool/main/b.deb")
		convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
	})

	convey.Convey("前缀必须以 / 开头和结尾，否则上界算不出来", t, func() {
		ctx, _, _ := testutils.Database(t)
		_, err := NewCacheObject().ListByPrefix(ctx, 7, "/pool")
		convey.So(err, convey.ShouldEqual, ErrTreePrefix)
	})
}

func TestCacheObjectRepo_StatTreeMatch(t *testing.T) {
	convey.Convey("搜索结果里一个目录的合计与命中部分的合计", t, func() {
		ctx, _, mock := testutils.Database(t)
		repo := NewCacheObject()

		convey.Convey("命中按搜索目录之下的部分算，而不是按这个目录之下", func() {
			match := "\\(" + matchSQL("SUBSTR\\(`key`,7\\)") + "\\)"
			mock.ExpectQuery("SELECT COUNT\\(\\*\\) AS count,COALESCE\\(SUM\\(size\\),0\\) AS size,"+
				"COALESCE\\(MAX\\(last_access_at\\),0\\) AS last_access_at,"+
				"COALESCE\\(SUM\\(CASE WHEN "+match+" THEN 1 ELSE 0 END\\),0\\) AS matched_count,"+
				"COALESCE\\(SUM\\(CASE WHEN "+match+" THEN size ELSE 0 END\\),0\\) AS matched_size "+
				"FROM `cache_objects` WHERE "+prefixWhere+"$").
				WithArgs("%main%", variantLike, "%main%"+variantLike, "%main%", variantLike, "%main%"+variantLike, int64(7), "/pool/main/", "/pool/main0", "/pool/main/%").
				WillReturnRows(sqlmock.NewRows([]string{"count", "size", "last_access_at", "matched_count", "matched_size"}).
					AddRow(10, 1000, 99, 4, 400))

			got, err := repo.StatTreeMatch(ctx, &cache_entity.TreeMatchOption{
				UpstreamID: 7, Prefix: "/pool/main/", SearchPrefix: "/pool/", Keyword: "MAIN"})
			convey.So(err, convey.ShouldBeNil)
			convey.So(*got, convey.ShouldResemble, cache_entity.TreeMatchStat{
				Count: 10, Size: 1000, LastAccessAt: 99, MatchedCount: 4, MatchedSize: 400})
			convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
		})

		convey.Convey("整个上游都算命中时不再带匹配条件", func() {
			mock.ExpectQuery("SELECT COUNT\\(\\*\\) AS count,.*CASE WHEN 1=1 THEN 1 ELSE 0 END.*"+
				"FROM `cache_objects` WHERE "+prefixWhere+"$").
				WithArgs(int64(8), "/", "0", "/%").
				WillReturnRows(sqlmock.NewRows([]string{"count", "size", "last_access_at", "matched_count", "matched_size"}).
					AddRow(3, 30, 9, 3, 30))

			got, err := repo.StatTreeMatch(ctx, &cache_entity.TreeMatchOption{
				UpstreamID: 8, Prefix: "/", Keyword: "debian", AllMatched: true})
			convey.So(err, convey.ShouldBeNil)
			convey.So(got.MatchedCount, convey.ShouldEqual, 3)
			convey.So(mock.ExpectationsWereMet(), convey.ShouldBeNil)
		})
	})
}
