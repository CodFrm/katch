package cache

import "strings"

// IsImmutable 按上游记录的模式列表判定一条上游侧路径是不是内容寻址的对象。
//
// 判定结果决定这个对象走哪一档缓存（决策 7）：命中即长期缓存、只由 LRU 淘汰；
// 不命中就是可变对象，按 TTL 过期。判错的代价不对称——把可变的当成不可变，
// 镜像站会持续发出过期的 InRelease 或 tag，所以这里只认显式写下的模式，
// 空列表一律当可变。
//
// 模式语义（对整条上游侧路径求值）：
//   - 不含通配符：作为**子串**匹配。APT 的 pool/ 就是这一类，写起来就是原样抄
//     一段路径，不必关心它出现在第几层；Go proxy 的版本文件不要写成 /@v/——
//     那会把会变的 @v/list 一起变成永久缓存，要写 /@v/*.info 这种带后缀的；
//   - `*`：匹配任意一段字符，**包括** `/`。路径的层数在不同上游之间差别很大，
//     不跨层的通配在这里几乎总要写成一串 `*/*/*`；
//   - `?`：匹配任意一个字符。raw.githubusercontent.com 那种「路径里有 40 位
//     commit SHA」的不可变模式靠它表达。
//
// 含不含通配符，匹配范围都是**子串**：一条模式只描述路径里的一段特征，让它有时
// 锚定整条路径、有时不锚定，是每次写模式都要先猜一次的陷阱。要锚定开头就写
// 明确的前缀（`/pool/` 这样的写法本身就足够具体）。
func IsImmutable(patterns []string, path string) bool {
	for _, p := range patterns {
		if p == "" {
			continue
		}
		if !strings.ContainsAny(p, "*?") {
			if strings.Contains(path, p) {
				return true
			}
			continue
		}
		if wildcardMatch("*"+p+"*", path) {
			return true
		}
	}
	return false
}

// wildcardMatch 是 `*`/`?` 的整串匹配，`*` 跨 `/`；子串语义由调用方在两头补 `*`。
//
// 不用 path.Match：它的 `*` 不跨 `/`，而上游路径的层数各不相同，逐层通配在这里
// 只会让每条模式都写成一串 `*/*/*`。这里用带回溯点的线性扫描，不递归——
// 模式来自管理界面，递归实现会让 `a*a*a*a*...` 这种输入变成一次 CPU 拒绝服务。
func wildcardMatch(pattern, s string) bool {
	var (
		p, i       int
		starP      = -1
		starI      int
		patternLen = len(pattern)
		sLen       = len(s)
	)
	for i < sLen {
		switch {
		case p < patternLen && (pattern[p] == '?' || pattern[p] == s[i]):
			p++
			i++
		case p < patternLen && pattern[p] == '*':
			starP = p
			starI = i
			p++
		case starP >= 0:
			// 回到最近一个 `*`，让它多吃一个字符再试。
			p = starP + 1
			starI++
			i = starI
		default:
			return false
		}
	}
	for p < patternLen && pattern[p] == '*' {
		p++
	}
	return p == patternLen
}
