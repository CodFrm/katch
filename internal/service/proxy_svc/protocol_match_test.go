package proxy_svc

import (
	"testing"

	"github.com/smartystreets/goconvey/convey"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/dispatch"
)

// TestProtocolMatches 请求归属 × 协议集合的全部组合。
//
// 这是白名单的第二道闸：第一道回答「表里有没有这个主机」，这一道回答「这个主机
// 开没开这次请求要的协议」。两道都只有一个出口——ErrUpstreamNotAllowed——所以
// 这里错一格，外面看到的只是一个多出来或少掉一个的 404。
func TestProtocolMatches(t *testing.T) {
	set := func(protocols ...string) upstream_entity.ProtocolSet {
		return upstream_entity.ProtocolSet(protocols)
	}
	cases := []struct {
		name      string
		kind      dispatch.Kind
		protocols upstream_entity.ProtocolSet
		want      bool
	}{
		{"registry 形态遇上只开 registry 的记录", dispatch.KindRegistry,
			set(upstream_entity.ProtocolRegistry), true},
		{"registry 形态遇上只开 static 的记录", dispatch.KindRegistry,
			set(upstream_entity.ProtocolStatic), false},
		{"registry 形态遇上只开 git 的记录", dispatch.KindRegistry,
			set(upstream_entity.ProtocolGit), false},
		{"static 形态遇上只开 static 的记录", dispatch.KindStatic,
			set(upstream_entity.ProtocolStatic), true},
		{"static 形态遇上只开 registry 的记录", dispatch.KindStatic,
			set(upstream_entity.ProtocolRegistry), false},
		// 本轮要的那一条：多开一种协议不会把原本那一种挤掉。
		{"static 形态遇上同时开了 static 与 git 的记录", dispatch.KindStatic,
			set(upstream_entity.ProtocolStatic, upstream_entity.ProtocolGit), true},
		{"registry 形态遇上同时开了 registry 与 static 的记录", dispatch.KindRegistry,
			set(upstream_entity.ProtocolRegistry, upstream_entity.ProtocolStatic), true},
		{"static 形态遇上同时开了 registry 与 static 的记录", dispatch.KindStatic,
			set(upstream_entity.ProtocolRegistry, upstream_entity.ProtocolStatic), true},
		// 只开 git 的记录在 static 路径上什么都不是：git 端点由路径形态识别，
		// 认不出来的那些路径不该因为「这个主机开了 git」就被当成静态资源发出去。
		{"static 形态遇上只开 git 的记录", dispatch.KindStatic,
			set(upstream_entity.ProtocolGit), false},
		// 空集合与认不出的取值都必须落到拒绝：判定的默认值是拒绝，而不是放行。
		{"空集合对 static 形态", dispatch.KindStatic, set(), false},
		{"空集合对 registry 形态", dispatch.KindRegistry, set(), false},
		{"nil 集合对 static 形态", dispatch.KindStatic, nil, false},
		{"只有未知取值的集合", dispatch.KindStatic, set("ftp"), false},
		{"未知取值混在已开的协议里，不影响那一种", dispatch.KindStatic,
			set("ftp", upstream_entity.ProtocolStatic), true},
		// 这两种归属根本不该走到回源，走到了就是调用方漏判了一种分支。
		{"registry 探测请求不该走到这里", dispatch.KindRegistryPing,
			set(upstream_entity.ProtocolRegistry), false},
		{"前端页面不该走到这里", dispatch.KindSPA,
			set(upstream_entity.ProtocolStatic), false},
	}

	convey.Convey("协议集合决定一条上游服务哪些请求形态", t, func() {
		for _, c := range cases {
			convey.Convey(c.name, func() {
				got := protocolMatches(
					&Target{Kind: c.kind, Host: "example.invalid", Path: "/x"},
					&upstream_entity.Upstream{Host: "example.invalid", Protocols: c.protocols},
				)
				convey.So(got, convey.ShouldEqual, c.want)
			})
		}
	})
}
