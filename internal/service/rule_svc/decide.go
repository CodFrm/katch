package rule_svc

import (
	"net/url"
	"sort"
	"strings"

	"github.com/CodFrm/katch/internal/model/entity/rule_entity"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
)

// Scope 判定发生在哪一层。
type Scope string

const (
	// ScopeGlobal 全局规则（决策 14：跨上游成立的策略只配一次）。
	ScopeGlobal Scope = "global"
	// ScopeUpstream 该上游自己的规则。
	ScopeUpstream Scope = "upstream"
	// ScopeDefault 谁都没命中，落到上游的默认策略。
	ScopeDefault Scope = "default"
)

// TraceStep 求值过程中考察过的一步。
//
// 它是决策 15 的配套：具体度定序换来了确定性，代价是「为什么是这条命中」不再
// 显然。把走过的每一步连同没命中的那些一起报出来，才能在界面上回答这个问题。
type TraceStep struct {
	Scope   Scope  `json:"scope"`
	RuleID  int64  `json:"rule_id"`
	Pattern string `json:"pattern"`
	Action  string `json:"action"`
	// Matched 这一步是否匹配上了这条路径。
	Matched bool `json:"matched"`
	// Decisive 这一步是否决定了最终结果。整个过程里至多有一步为真。
	Decisive bool `json:"decisive"`
}

// Decision 一次求值的结果。
type Decision struct {
	Allowed bool
	Scope   Scope
	// MatchedRule 决定结果的那条规则。落到默认策略时为 nil。
	MatchedRule *rule_entity.AccessRule
	// Trace 求值过程，按考察顺序排列，止于决定结果的那一步。
	Trace []TraceStep
}

// Decide 是访问规则的求值内核，纯函数：不查库、不认识 HTTP、不改动入参。
//
// 求值顺序（「访问规则」一节）：
//
//  1. 全局规则按具体度排序，首个匹配者决定；
//  2. 未被全局规则决定时，该上游的规则按具体度排序，首个匹配者决定；
//  3. 仍未被决定时，落到该上游的默认策略。
//
// 全局整层先于上游内整层，而不是把两层混在一起按具体度排：跨上游成立的策略
// （「禁止一切 :latest」）若能被某个上游里一条同样具体的 allow 掀翻，那条全局
// 策略就等于没配，而这一点在界面上完全看不出来。
func Decide(global, own []*rule_entity.AccessRule, defaultPolicy, path string) *Decision {
	target := normalizePath(path)
	d := &Decision{}
	for _, layer := range []struct {
		scope Scope
		rules []*rule_entity.AccessRule
	}{{ScopeGlobal, global}, {ScopeUpstream, own}} {
		if decided := d.evaluateLayer(layer.scope, layer.rules, target); decided {
			return d
		}
	}
	// 默认策略是上游上的一个字段而不是一条规则：它没有 pattern，混进规则表只会
	// 让具体度排序无从谈起。它同样进过程——「没有任何规则命中」和「有一条规则
	// 放行了它」在界面上是两个结论。
	allowed := defaultPolicy != upstream_entity.PolicyDenyUnlessMatched
	action := rule_entity.ActionDeny
	if allowed {
		action = rule_entity.ActionAllow
	}
	d.Allowed = allowed
	d.Scope = ScopeDefault
	d.Trace = append(d.Trace, TraceStep{
		Scope: ScopeDefault, Action: action, Matched: true, Decisive: true,
	})
	return d
}

// evaluateLayer 求值一层，返回这一层是否已经决定了结果。
func (d *Decision) evaluateLayer(scope Scope, rules []*rule_entity.AccessRule, target string) bool {
	for _, r := range sortRules(rules) {
		matched := matchNormalized(r.Pattern, target)
		d.Trace = append(d.Trace, TraceStep{
			Scope: scope, RuleID: r.ID, Pattern: r.Pattern, Action: r.Action,
			Matched: matched, Decisive: matched,
		})
		if matched {
			d.Allowed = r.Action != rule_entity.ActionDeny
			d.Scope = scope
			d.MatchedRule = r
			return true
		}
	}
	return false
}

// sortRules 按具体度排出一层的求值顺序。复制一份再排：入参通常是进程内那份
// 规则快照，就地排序会让并发的另一次求值读到一个正在被重排的切片。
func sortRules(rules []*rule_entity.AccessRule) []*rule_entity.AccessRule {
	sorted := make([]*rule_entity.AccessRule, len(rules))
	copy(sorted, rules)
	sort.Slice(sorted, func(i, j int) bool { return MoreSpecific(sorted[i], sorted[j]) })
	return sorted
}

// MoreSpecific 报告规则 a 是否比 b 更该说了算，纯函数（决策 15 的定序规则）：
// 字面前缀长者优先，前缀等长时通配符少者优先，仍相等时 deny 优先。
//
// 后面还跟了模式字典序与 id 两级：前三项能打平（两条 action 和 pattern 都相同的
// 规则），而一个不是全序的比较函数会让排序结果随输入顺序变，于是同一组规则在
// 两次查询里给出两种判定。定序必须是确定的，这是它取代人工顺序的全部理由。
func MoreSpecific(a, b *rule_entity.AccessRule) bool {
	if pa, pb := len(literalPrefix(a.Pattern)), len(literalPrefix(b.Pattern)); pa != pb {
		return pa > pb
	}
	if wa, wb := strings.Count(a.Pattern, "*"), strings.Count(b.Pattern, "*"); wa != wb {
		return wa < wb
	}
	if da, db := a.Action == rule_entity.ActionDeny, b.Action == rule_entity.ActionDeny; da != db {
		return da
	}
	if a.Pattern != b.Pattern {
		return a.Pattern < b.Pattern
	}
	return a.ID < b.ID
}

// literalPrefix 取模式里第一个通配符之前的那段字面量。
func literalPrefix(pattern string) string {
	if i := strings.IndexByte(pattern, '*'); i >= 0 {
		return pattern[:i]
	}
	return pattern
}

// Match 报告 pattern 是否匹配 path，纯函数。
//
// * 匹配任意一段字符（含空串与斜杠），其余字符按字面比，没有通配符时必须整条
// 相等。不引入 ** 与单段通配这类分级语义：规则是人手写的，多一层语义就多一类
// 「我以为它匹配」的误配，而误配在这里等于一次不该发生的放行或拒绝。
func Match(pattern, path string) bool {
	return matchNormalized(pattern, normalizePath(path))
}

// matchNormalized 是 Match 的内层形态，路径已经规范化过。
//
// 分出这一层是为了让一次求值只规范化一次路径：规范化含一次百分号解码，
// 而一层规则可能有几十条。
func matchNormalized(pattern, target string) bool {
	if pattern == "" {
		// 空模式什么都不匹配。当成「匹配一切」在这里是危险的缺省：一条误存的
		// 空 deny 会让整个上游静默变成 403。
		return false
	}
	return matchSegments(strings.Split(normalizePattern(pattern), "*"), target)
}

// matchSegments 按被通配符切开的字面段依次贪心匹配。
func matchSegments(parts []string, target string) bool {
	if len(parts) == 1 {
		return parts[0] == target
	}
	if !strings.HasPrefix(target, parts[0]) {
		return false
	}
	rest := target[len(parts[0]):]
	last := parts[len(parts)-1]
	if !strings.HasSuffix(rest, last) || len(rest) < len(last) {
		return false
	}
	rest = rest[:len(rest)-len(last)]
	for _, part := range parts[1 : len(parts)-1] {
		i := strings.Index(rest, part)
		if i < 0 {
			return false
		}
		rest = rest[i+len(part):]
	}
	return true
}

// normalizePath 把一条上游侧路径化成匹配用的形态：去掉前导斜杠，并还原转义。
//
// 还原转义是安全相关的一步：拿 %2F 把 library/redis:latest 写成
// library%2Fredis:latest 是绕过一条 deny 最省事的办法，而上游看到的是还原之后
// 的那条路径——判定必须和上游看到的是同一串字节。还原不了就按原样比，不能因为
// 一个畸形的转义就放弃判定。
func normalizePath(path string) string {
	if decoded, err := url.PathUnescape(path); err == nil {
		path = decoded
	}
	return strings.TrimPrefix(path, "/")
}

// normalizePattern 规则模式只去前导斜杠。模式是人在界面上写的，写成 /pool/*
// 还是 pool/* 都该是同一条规则。
func normalizePattern(pattern string) string {
	return strings.TrimPrefix(pattern, "/")
}
