package dispatch

import (
	"testing"

	"github.com/smartystreets/goconvey/convey"
)

// TestClassify 穷举分段规则。
//
// 这是安全相关的那一段：判错了要么把 katch 变成开放代理（把不该当主机名的段
// 当成主机名），要么让 docker pull 拿到一份 200 + HTML（把上游路径交给 SPA）。
func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		path string
		kind Kind
		host string
		rest string
	}{
		// ── 保留段与上游段靠点号区分（决策 2）──
		{"根路径交给 SPA", "/", KindSPA, "", ""},
		{"不含点的第一段是 katch 自己的保留段", "/mirrors", KindSPA, "", ""},
		{"多段但都不含点仍是 SPA 路由", "/settings/upstreams", KindSPA, "", ""},
		{"前端产物路径不含点的第一段", "/assets/index-abc123.js", KindSPA, "", ""},
		{"管理接口是 katch 自身端点", "/api/v1/admin/upstreams", KindSelf, "", ""},
		{"/api 本身也是自身端点", "/api", KindSelf, "", ""},
		{"/metrics 是自身端点", "/metrics", KindSelf, "", ""},

		// ── 第一段含点即上游主机名（决策 1）──
		{"含点的第一段是上游主机名", "/deb.debian.org/dists/stable/InRelease",
			KindStatic, "deb.debian.org", "/dists/stable/InRelease"},
		{"只给主机名时上游路径是根", "/proxy.golang.org", KindStatic, "proxy.golang.org", "/"},
		{"主机名后跟斜杠时上游路径是根", "/proxy.golang.org/", KindStatic, "proxy.golang.org", "/"},
		{"上游路径原样带过去，不做规整", "/proxy.golang.org/github.com/!cod!frm/@v/list",
			KindStatic, "proxy.golang.org", "/github.com/!cod!frm/@v/list"},
		{"转义序列在上游路径里保持原样", "/raw.githubusercontent.com/a/b/main/x%20y.sh",
			KindStatic, "raw.githubusercontent.com", "/a/b/main/x%20y.sh"},

		// ── registry 的主机名在 /v2/ 之后 ──
		{"/v2/ 本身是探测请求", "/v2/", KindRegistryPing, "", ""},
		{"/v2 不带斜杠也是探测请求", "/v2", KindRegistryPing, "", ""},
		{"registry 的主机名在 /v2/ 之后，且 rest 不含 /v2",
			"/v2/docker.io/library/redis/manifests/7",
			KindRegistry, "docker.io", "/library/redis/manifests/7"},
		{"registry 的 blob 路径同理", "/v2/ghcr.io/o/r/blobs/sha256:abc",
			KindRegistry, "ghcr.io", "/o/r/blobs/sha256:abc"},
		{"/v2/ 之后只有主机名时上游路径是根", "/v2/docker.io", KindRegistry, "docker.io", "/"},
		{"/v2/ 之后第一段不含点就没有上游可言，不是 SPA 路由",
			"/v2/library/redis/manifests/7", KindInvalid, "", ""},

		// ── 拿不出主机名或含回溯段的，一律无效 ──
		{"回溯段会爬出回源地址的基路径", "/deb.debian.org/pool/../../etc/passwd", KindInvalid, "", ""},
		{"转义过的回溯段是同一件事", "/deb.debian.org/pool/%2e%2e/x", KindInvalid, "", ""},
		{"registry 路径上的回溯段同样挡住", "/v2/docker.io/a/../../x", KindInvalid, "", ""},
		{"主机名里藏一层路径（%2F）不算主机名", "/a.com%2Fb/x", KindInvalid, "", ""},
	}

	convey.Convey("按路径判定归属", t, func() {
		for _, c := range cases {
			convey.Convey(c.name+"："+c.path, func() {
				kind, host, rest := Classify(c.path)
				convey.So(kind.String(), convey.ShouldEqual, c.kind.String())
				convey.So(host, convey.ShouldEqual, c.host)
				convey.So(rest, convey.ShouldEqual, c.rest)
			})
		}
	})
}

// TestClassify_NoHostLeaksOnInvalid 钉住「无效即什么都不回」：只要判定不是一条
// 可服务的上游请求，就不能把主机名从返回值里带出去——调用方一旦拿到它，迟早
// 会有人把它写进 404 的响应体或日志之外的地方，那就是一个内网主机探测器。
func TestClassify_NoHostLeaksOnInvalid(t *testing.T) {
	convey.Convey("判定无效时不带出主机名", t, func() {
		for _, path := range []string{
			"/a.com%2Fb/x",
			"/internal.corp.local/../x",
			"/v2/internal.corp.local/a/../../x",
		} {
			kind, host, rest := Classify(path)
			convey.So(kind.String(), convey.ShouldEqual, KindInvalid.String())
			convey.So(host, convey.ShouldEqual, "")
			convey.So(rest, convey.ShouldEqual, "")
		}
	})
}
