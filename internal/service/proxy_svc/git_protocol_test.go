package proxy_svc

import (
	"testing"

	"github.com/smartystreets/goconvey/convey"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/dispatch"
)

// TestProtocolMatches_Git git 端点要的是 git 这一种协议，而不是它寄生的那个
// static 路径空间所要的那一种。
//
// 两个方向都要钉住：只开 static 的 github.com 不能因为路径长得像 git 端点就
// 把 clone 转出去（那条记录的站长没开这个协议）；只开 git 的记录也不能反过来
// 把普通静态路径服务掉——protocolMatches 里已有的那条用例管着后半句。
func TestProtocolMatches_Git(t *testing.T) {
	set := func(protocols ...string) upstream_entity.ProtocolSet {
		return upstream_entity.ProtocolSet(protocols)
	}
	advertise := dispatch.GitEndpoint{
		Service: dispatch.GitUploadPack, Advertise: true, Repo: "/CodFrm/katch",
	}
	negotiate := dispatch.GitEndpoint{Service: dispatch.GitUploadPack, Repo: "/CodFrm/katch"}
	cases := []struct {
		name      string
		kind      dispatch.Kind
		git       dispatch.GitEndpoint
		protocols upstream_entity.ProtocolSet
		want      bool
	}{
		{"ref 广播遇上开了 git 的记录", dispatch.KindStatic, advertise,
			set(upstream_entity.ProtocolGit), true},
		{"协商遇上开了 git 的记录", dispatch.KindStatic, negotiate,
			set(upstream_entity.ProtocolGit), true},
		{"同时开了 static 与 git 的记录两种形态都服务", dispatch.KindStatic, negotiate,
			set(upstream_entity.ProtocolStatic, upstream_entity.ProtocolGit), true},
		{"只开 static 的记录不服务 git 端点", dispatch.KindStatic, negotiate,
			set(upstream_entity.ProtocolStatic), false},
		{"只开 registry 的记录不服务 git 端点", dispatch.KindStatic, negotiate,
			set(upstream_entity.ProtocolRegistry), false},
		{"空集合不服务 git 端点", dispatch.KindStatic, negotiate, set(), false},
		// registry 是自己那条路径空间，/v2/ 之下不存在 git 端点。真拿着一个
		// git 结果走到这里，要的仍然是 registry——否则只开 git 的记录会因为
		// 一个后缀像 git 的 blob 路径而把 /v2/ 的请求服务掉。
		{"registry 形态不因为带着 git 结果就改判", dispatch.KindRegistry, negotiate,
			set(upstream_entity.ProtocolGit), false},
	}

	convey.Convey("git 端点要的是 git 协议", t, func() {
		for _, c := range cases {
			convey.Convey(c.name, func() {
				got := protocolMatches(
					&Target{Kind: c.kind, Git: c.git, Host: "example.invalid", Path: "/x"},
					&upstream_entity.Upstream{Host: "example.invalid", Protocols: c.protocols},
				)
				convey.So(got, convey.ShouldEqual, c.want)
			})
		}
	})
}
