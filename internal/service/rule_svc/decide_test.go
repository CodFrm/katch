package rule_svc

import (
	"testing"

	"github.com/smartystreets/goconvey/convey"

	"github.com/CodFrm/katch/internal/model/entity/rule_entity"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
)

// 这一组是纯函数的直接用例。求值内核决定了每一次拒绝是否正当，它必须能脱离
// 数据库、HTTP 和上游表被穷举地测——那也是把它做成纯函数的唯一理由。

func rule(id int64, action, pattern string) *rule_entity.AccessRule {
	return &rule_entity.AccessRule{ID: id, Action: action, Pattern: pattern}
}

// TestMatch 模式匹配：* 匹配任意一段字符，其余字符按字面比。
func TestMatch(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		path    string
		want    bool
	}{
		{"通配符吃掉剩下的一段", "library/*", "library/alpine:3.21", true},
		{"通配符可以出现在开头", "*:latest", "library/redis:latest", true},
		{"没有通配符时必须整条相等", "library/alpine:3.21", "library/alpine:3.21", true},
		{"没有通配符时差一个字符就不匹配", "library/alpine:3.21", "library/alpine:3.20", false},
		{"前缀对不上就不匹配", "library/*", "other/redis", false},
		{"模式与路径的前导斜杠都不计", "/pool/*", "/pool/main/n/nginx.deb", true},
		{"模式不带斜杠也匹配带斜杠的路径", "pool/*", "/pool/main/n/nginx.deb", true},
		{"单个 * 匹配一切", "*", "/dists/stable/InRelease", true},
		{"空模式什么都不匹配", "", "/dists/stable/InRelease", false},
		{"通配符可以匹配空串", "dists/*", "/dists/", true},
		{"通配符不能凭空补出一段", "dists/*", "/dists", false},
		// 转义形态必须先还原再比：拿 %2F 绕过一条 deny 是最省事的一种规避，
		// 而上游看到的是还原之后的那条路径。
		{"路径的转义形态按还原后的字节比", "library/*:latest", "/library%2Falpine:latest", true},
	}
	convey.Convey("模式匹配", t, func() {
		for _, c := range cases {
			convey.Convey(c.name, func() {
				convey.So(Match(c.pattern, c.path), convey.ShouldEqual, c.want)
			})
		}
	})
}

// TestMoreSpecific 具体度定序：字面前缀长者优先，前缀等长时通配符少者优先，
// 仍相等时 deny 优先。这条序是拒绝是否正当的全部依据。
func TestMoreSpecific(t *testing.T) {
	cases := []struct {
		name string
		a    *rule_entity.AccessRule
		b    *rule_entity.AccessRule
	}{
		{
			"字面前缀长者优先",
			rule(1, rule_entity.ActionAllow, "library/alpine/*"),
			rule(2, rule_entity.ActionDeny, "library/*"),
		},
		{
			"前缀等长时通配符少者优先",
			rule(1, rule_entity.ActionAllow, "library/*"),
			rule(2, rule_entity.ActionAllow, "library/*/*"),
		},
		{
			"前缀与通配符都相等时 deny 优先",
			rule(1, rule_entity.ActionDeny, "library/*"),
			rule(2, rule_entity.ActionAllow, "library/*"),
		},
		{
			"三项都相等时按模式字典序，保证定序是确定的",
			rule(1, rule_entity.ActionDeny, "aaa*"),
			rule(2, rule_entity.ActionDeny, "bbb*"),
		},
		{
			"模式也相同时按 id，两条一模一样的规则不会随机排",
			rule(1, rule_entity.ActionDeny, "library/*"),
			rule(2, rule_entity.ActionDeny, "library/*"),
		},
	}
	convey.Convey("具体度定序", t, func() {
		for _, c := range cases {
			convey.Convey(c.name, func() {
				convey.So(MoreSpecific(c.a, c.b), convey.ShouldBeTrue)
				// 反过来必须为假：不是严格序的比较函数会让排序结果随输入顺序变，
				// 于是同一组规则在两台机器上给出两种判定。
				convey.So(MoreSpecific(c.b, c.a), convey.ShouldBeFalse)
			})
		}
	})
}

// TestDecide 求值顺序：全局 → 上游内 → 上游的默认策略，每层内首个匹配者决定。
func TestDecide(t *testing.T) {
	cases := []struct {
		name        string
		global      []*rule_entity.AccessRule
		own         []*rule_entity.AccessRule
		policy      string
		path        string
		wantAllowed bool
		wantScope   Scope
		wantRuleID  int64
	}{
		{
			// 这就是任务目标 (a)：两条规则的具体度完全相同，决定结果的是层级，
			// 不是谁更晚写进库里。
			name:        "全局 deny 与上游 allow 前缀等长时全局赢",
			global:      []*rule_entity.AccessRule{rule(1, rule_entity.ActionDeny, "dists/*")},
			own:         []*rule_entity.AccessRule{rule(2, rule_entity.ActionAllow, "dists/*")},
			policy:      upstream_entity.PolicyAllowAll,
			path:        "/dists/stable/InRelease",
			wantAllowed: false,
			wantScope:   ScopeGlobal,
			wantRuleID:  1,
		},
		{
			// 这一条是决策 14 的真正牙齿：上游内那条明显更具体，如果两层被合成
			// 一份列表一起按具体度排，它就会赢。全局整层先于上游内整层求值，
			// 结果才由那条笼统的全局 deny 决定——否则「禁止一切 :latest」这类
			// 跨上游策略，随便哪个上游写一条更细的 allow 就能掀翻。
			name:        "更笼统的全局 deny 仍然压过更具体的上游 allow",
			global:      []*rule_entity.AccessRule{rule(1, rule_entity.ActionDeny, "dists/*")},
			own:         []*rule_entity.AccessRule{rule(2, rule_entity.ActionAllow, "dists/stable/*")},
			policy:      upstream_entity.PolicyAllowAll,
			path:        "/dists/stable/InRelease",
			wantAllowed: false,
			wantScope:   ScopeGlobal,
			wantRuleID:  1,
		},
		{
			name:        "全局没有命中时由上游内规则决定",
			global:      []*rule_entity.AccessRule{rule(1, rule_entity.ActionDeny, "pool/*")},
			own:         []*rule_entity.AccessRule{rule(2, rule_entity.ActionAllow, "dists/*")},
			policy:      upstream_entity.PolicyDenyUnlessMatched,
			path:        "/dists/stable/InRelease",
			wantAllowed: true,
			wantScope:   ScopeUpstream,
			wantRuleID:  2,
		},
		{
			name:        "三步都没命中时落到默认策略 allow_all",
			policy:      upstream_entity.PolicyAllowAll,
			path:        "/dists/stable/InRelease",
			wantAllowed: true,
			wantScope:   ScopeDefault,
		},
		{
			name:        "三步都没命中时落到默认策略 deny_unless_matched",
			global:      []*rule_entity.AccessRule{rule(1, rule_entity.ActionAllow, "pool/*")},
			own:         []*rule_entity.AccessRule{rule(2, rule_entity.ActionAllow, "sumdb/*")},
			policy:      upstream_entity.PolicyDenyUnlessMatched,
			path:        "/dists/stable/InRelease",
			wantAllowed: false,
			wantScope:   ScopeDefault,
		},
		{
			name:        "默认策略留空按 allow_all，和上游保存时的缺省一致",
			path:        "/dists/stable/InRelease",
			wantAllowed: true,
			wantScope:   ScopeDefault,
		},
		{
			name: "同层内具体度相同时 deny 优先",
			global: []*rule_entity.AccessRule{
				rule(1, rule_entity.ActionAllow, "dists/*"),
				rule(2, rule_entity.ActionDeny, "dists/*"),
			},
			policy:      upstream_entity.PolicyAllowAll,
			path:        "/dists/stable/InRelease",
			wantAllowed: false,
			wantScope:   ScopeGlobal,
			wantRuleID:  2,
		},
		{
			// 具体度定序与人的直觉一致：library/alpine:3.21 比 library/* 更该说了算。
			name: "同层内更具体的 allow 胜过笼统的 deny",
			global: []*rule_entity.AccessRule{
				rule(1, rule_entity.ActionDeny, "library/*"),
				rule(2, rule_entity.ActionAllow, "library/alpine:3.21"),
			},
			policy:      upstream_entity.PolicyAllowAll,
			path:        "library/alpine:3.21",
			wantAllowed: true,
			wantScope:   ScopeGlobal,
			wantRuleID:  2,
		},
		{
			name:        "上游内的 deny 胜过默认放行",
			own:         []*rule_entity.AccessRule{rule(9, rule_entity.ActionDeny, "*:latest")},
			policy:      upstream_entity.PolicyAllowAll,
			path:        "library/redis:latest",
			wantAllowed: false,
			wantScope:   ScopeUpstream,
			wantRuleID:  9,
		},
	}
	convey.Convey("求值顺序", t, func() {
		for _, c := range cases {
			convey.Convey(c.name, func() {
				got := Decide(c.global, c.own, c.policy, c.path)
				convey.So(got.Allowed, convey.ShouldEqual, c.wantAllowed)
				convey.So(got.Scope, convey.ShouldEqual, c.wantScope)
				if c.wantRuleID == 0 {
					convey.So(got.MatchedRule, convey.ShouldBeNil)
					return
				}
				convey.So(got.MatchedRule, convey.ShouldNotBeNil)
				convey.So(got.MatchedRule.ID, convey.ShouldEqual, c.wantRuleID)
			})
		}
	})
}

// TestDecide_TraceExplainsTheVerdict 决策 15 的配套：具体度定序换来了确定性，
// 代价是「为什么是这条命中」不再显然，所以求值必须把走过的每一步报出来。
func TestDecide_TraceExplainsTheVerdict(t *testing.T) {
	convey.Convey("求值过程被完整记下来", t, func() {
		got := Decide(
			[]*rule_entity.AccessRule{
				// 这一条的字面前缀更长，因此先被考察；它没匹配上，求值才轮到下一条。
				rule(1, rule_entity.ActionDeny, "dists/stable/Packages*"),
				rule(2, rule_entity.ActionDeny, "dists/*"),
			},
			[]*rule_entity.AccessRule{rule(3, rule_entity.ActionAllow, "dists/*")},
			upstream_entity.PolicyAllowAll,
			"/dists/stable/InRelease",
		)

		convey.So(got.Allowed, convey.ShouldBeFalse)
		convey.Convey("考察过的规则都在，且标出了没命中的那些", func() {
			convey.So(len(got.Trace), convey.ShouldEqual, 2)
			convey.So(got.Trace[0].RuleID, convey.ShouldEqual, 1)
			convey.So(got.Trace[0].Matched, convey.ShouldBeFalse)
			convey.So(got.Trace[0].Decisive, convey.ShouldBeFalse)
			convey.So(got.Trace[0].Scope, convey.ShouldEqual, ScopeGlobal)
			convey.So(got.Trace[0].Pattern, convey.ShouldEqual, "dists/stable/Packages*")
		})
		convey.Convey("决定结果的那一步被标出来", func() {
			convey.So(got.Trace[1].RuleID, convey.ShouldEqual, 2)
			convey.So(got.Trace[1].Matched, convey.ShouldBeTrue)
			convey.So(got.Trace[1].Decisive, convey.ShouldBeTrue)
			convey.So(got.Trace[1].Action, convey.ShouldEqual, rule_entity.ActionDeny)
		})
		convey.Convey("求值在决定之后就停了，没被问过的上游内规则不进过程", func() {
			// 把从没被考察过的规则也列进来，等于在界面上编造一段没发生的过程。
			for _, step := range got.Trace {
				convey.So(step.RuleID, convey.ShouldNotEqual, 3)
			}
		})
	})
}

// TestDecide_DefaultPolicyIsTracedToo 默认策略兜底同样要能解释：界面上
// 「没有任何规则命中」和「有一条规则放行了它」是两个结论。
func TestDecide_DefaultPolicyIsTracedToo(t *testing.T) {
	convey.Convey("落到默认策略时过程里有那一步", t, func() {
		got := Decide(nil, nil, upstream_entity.PolicyDenyUnlessMatched, "/dists/stable/InRelease")
		convey.So(got.Allowed, convey.ShouldBeFalse)
		convey.So(len(got.Trace), convey.ShouldEqual, 1)
		convey.So(got.Trace[0].Scope, convey.ShouldEqual, ScopeDefault)
		convey.So(got.Trace[0].Decisive, convey.ShouldBeTrue)
		convey.So(got.Trace[0].Action, convey.ShouldEqual, rule_entity.ActionDeny)
	})
}

// TestDecide_DoesNotMutateInput 求值内核不许改动传进来的规则切片：调用方给的
// 是进程内那份快照，就地排序会让并发的另一次求值读到一个正在被重排的切片。
func TestDecide_DoesNotMutateInput(t *testing.T) {
	convey.Convey("求值不改动入参", t, func() {
		global := []*rule_entity.AccessRule{
			rule(1, rule_entity.ActionAllow, "*"),
			rule(2, rule_entity.ActionDeny, "dists/stable/*"),
		}
		Decide(global, nil, upstream_entity.PolicyAllowAll, "/dists/stable/InRelease")
		convey.So(global[0].ID, convey.ShouldEqual, 1)
		convey.So(global[1].ID, convey.ShouldEqual, 2)
	})
}
