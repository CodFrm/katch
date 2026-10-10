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

		// ── SumDB 的公开别名在分发层保留独立身份并收敛成同一个目标 ──
		{"顶层 SumDB 别名映射到固定主机", "/sumdb/sum.golang.org/lookup/example.com/mod@v1.0.0",
			KindSumDB, "sum.golang.org", "/lookup/example.com/mod@v1.0.0"},
		{"GOPROXY 内嵌 SumDB 路由映射到固定主机", "/proxy.golang.org/sumdb/sum.golang.org/tile/8/1/000.p/16",
			KindSumDB, "sum.golang.org", "/tile/8/1/000.p/16"},
		{"SumDB supported 保留独立身份", "/sumdb/sum.golang.org/supported",
			KindSumDB, "sum.golang.org", "/supported"},
		{"SumDB latest 保留独立身份", "/proxy.golang.org/sumdb/sum.golang.org/latest",
			KindSumDB, "sum.golang.org", "/latest"},
		{"直接 sum.golang.org 路径仍是通用 static", "/sum.golang.org/lookup/example.com/mod@v1.0.0",
			KindStatic, "sum.golang.org", "/lookup/example.com/mod@v1.0.0"},
		{"SumDB 顶层保留段不能选择任意主机", "/sumdb/other.example/lookup/x@y",
			KindInvalid, "", ""},
		{"空 SumDB 命名空间不会回落 SPA", "/sumdb", KindInvalid, "", ""},
		{"SumDB 保留命名空间拒绝未知路由", "/sumdb/sum.golang.org/not-a-route",
			KindInvalid, "", ""},
		{"畸形 GOPROXY SumDB 命名空间不会落回 proxy.golang.org", "/proxy.golang.org/sumdb/other.example/latest",
			KindInvalid, "", ""},

		// ── Homebrew 等会追加 /v2 的 registry base，边界必须显式 ──
		{"registry base 去掉协议边界后返回上游路径",
			"/registry/ghcr.io/v2/homebrew/core/jq/manifests/tag",
			KindRegistry, "ghcr.io", "/homebrew/core/jq/manifests/tag"},
		{"registry base 的 v2 边界本身映射到上游根路径",
			"/registry/ghcr.io/v2", KindRegistry, "ghcr.io", "/"},
		{"registry base 的 v2 边界可带尾斜杠",
			"/registry/ghcr.io/v2/", KindRegistry, "ghcr.io", "/"},

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

		{"registry 仓库名可以合法地以 v2 开头，旧路由不能全局剥离第二个 v2",
			"/v2/ghcr.io/v2/foo/manifests/tag",
			KindRegistry, "ghcr.io", "/v2/foo/manifests/tag"},
		{"registry base 保留仓库名中的 v2 段",
			"/registry/ghcr.io/v2/v2/foo/manifests/tag",
			KindRegistry, "ghcr.io", "/v2/foo/manifests/tag"},
		{"registry base 缺少主机名无效", "/registry", KindInvalid, "", ""},
		{"registry base 空主机名无效", "/registry/", KindInvalid, "", ""},
		{"registry base 主机名不含点无效", "/registry/ghcr/v2/foo", KindInvalid, "", ""},
		{"registry base 缺少 v2 边界无效", "/registry/ghcr.io", KindInvalid, "", ""},
		{"registry base 使用 v1 边界无效", "/registry/ghcr.io/v1/foo", KindInvalid, "", ""},
		{"registry base 不接受编码后的 v2 边界", "/registry/ghcr.io/%76%32/foo", KindInvalid, "", ""},
		{"registry base 主机名不能藏编码斜杠", "/registry/ghcr.io%2Fevil.example/v2/foo", KindInvalid, "", ""},
		{"registry base 路径同样挡住回溯段", "/registry/ghcr.io/v2/a/%2e%2e/x", KindInvalid, "", ""},

		// ── 整条上游 URL 粘在后面：协议头摘掉，其余照上游段的规则走 ──
		{"https:// 协议头摘掉后第一段即主机名", "/https://raw.githubusercontent.com/a/b/main/x.sh",
			KindStatic, "raw.githubusercontent.com", "/a/b/main/x.sh"},
		{"http:// 同样摘掉，回源协议由上游记录决定", "/http://deb.debian.org/debian/dists/stable/InRelease",
			KindStatic, "deb.debian.org", "/debian/dists/stable/InRelease"},
		{"反代合并过的双斜杠（https:/）是同一件事", "/https:/raw.githubusercontent.com/a/b/main/x.sh",
			KindStatic, "raw.githubusercontent.com", "/a/b/main/x.sh"},
		{"http:/ 合并形态同理", "/http:/deb.debian.org/debian", KindStatic, "deb.debian.org", "/debian"},
		{"协议头不分大小写", "/HTTPS://raw.githubusercontent.com/a/b", KindStatic, "raw.githubusercontent.com", "/a/b"},
		{"协议头之后只有主机名时上游路径是根", "/https://proxy.golang.org", KindStatic, "proxy.golang.org", "/"},
		{"协议头之后的 GOPROXY 内嵌 SumDB 路由照常收敛",
			"/https://proxy.golang.org/sumdb/sum.golang.org/latest", KindSumDB, "sum.golang.org", "/latest"},
		{"协议头之后的转义序列保持原样", "/https://raw.githubusercontent.com/a/b/main/x%20y.sh",
			KindStatic, "raw.githubusercontent.com", "/a/b/main/x%20y.sh"},
		{"协议头之后第一段不含点就没有上游可言，不回落 SPA", "/https://localhost/x", KindInvalid, "", ""},
		{"协议头够不到 katch 自己的保留段", "/https://api/v1/site", KindInvalid, "", ""},
		{"协议头之后的 /v2 保留段同样够不到", "/https://v2/docker.io/library/redis/manifests/7", KindInvalid, "", ""},
		{"协议头之后什么都没有", "/https://", KindInvalid, "", ""},
		{"协议头只摘一层", "/https://https://deb.debian.org/x", KindInvalid, "", ""},
		{"协议头之后的回溯段同样挡住", "/https://deb.debian.org/pool/../../etc/passwd", KindInvalid, "", ""},
		{"协议头之后的主机名不能藏编码斜杠", "/https://a.com%2Fb/x", KindInvalid, "", ""},
		{"只认 http 与 https，别的协议头不是上游写法", "/ftp://deb.debian.org/x", KindSPA, "", ""},

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
			"/registry/internal.corp.local/v2/a/../../x",
			"/registry/internal%2Fcorp.local/v2/a",
			"/sumdb",
			"/sumdb/other.example/latest",
			"/sumdb/sum.golang.org/not-a-route",
			"/proxy.golang.org/sumdb/other.example/latest",
			"/https://internal/x",
			"/https://internal.corp.local/../x",
			"/https://internal%2Fcorp.local/x",
		} {
			kind, host, rest := Classify(path)
			convey.So(kind.String(), convey.ShouldEqual, KindInvalid.String())
			convey.So(host, convey.ShouldEqual, "")
			convey.So(rest, convey.ShouldEqual, "")
		}
	})
}

// TestHostFirstRegistry 钉住「上游主机在前」的 registry 写法怎么认、归一成什么。
//
// 这里只看路径形态：这台主机开没开 registry 协议由调用方查上游表决定——一条只开
// static 的上游（registry 的 blob 重定向落点就是）在 /v2/ 之下有它自己的对象，
// 光凭形态改写会把那些请求全部送错地方。
func TestHostFirstRegistry(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		host      string
		canonical string
		ok        bool
	}{
		{"主机名后的 /v2 是协议边界", "/ghcr.io/v2/o/r/manifests/t", "ghcr.io", "/v2/ghcr.io/o/r/manifests/t", true},
		{"blob 路径同理", "/ghcr.io/v2/o/r/blobs/sha256:abc", "ghcr.io", "/v2/ghcr.io/o/r/blobs/sha256:abc", true},
		{"边界本身映射到上游根路径", "/ghcr.io/v2", "ghcr.io", "/v2/ghcr.io", true},
		{"边界可带尾斜杠", "/ghcr.io/v2/", "ghcr.io", "/v2/ghcr.io/", true},
		{"带协议头的整条 registry URL", "/https://ghcr.io/v2/o/r/manifests/t", "ghcr.io", "/v2/ghcr.io/o/r/manifests/t", true},
		{"反代合并过双斜杠的协议头", "/https:/ghcr.io/v2/o/r/manifests/t", "ghcr.io", "/v2/ghcr.io/o/r/manifests/t", true},
		{"只摘第一个 v2，仓库名里的 v2 段留着", "/ghcr.io/v2/v2/foo/manifests/tag", "ghcr.io", "/v2/ghcr.io/v2/foo/manifests/tag", true},
		{"仓库路径里的转义序列保持原样", "/ghcr.io/v2/o/r%20x/manifests/t", "ghcr.io", "/v2/ghcr.io/o/r%20x/manifests/t", true},

		{"主机名后不是 v2", "/ghcr.io/v1/foo", "", "", false},
		{"v2 必须是完整的一段", "/ghcr.io/v2foo/x", "", "", false},
		{"v2 不在主机名之后", "/ghcr.io/o/v2/x", "", "", false},
		{"只有主机名", "/ghcr.io", "", "", false},
		{"不接受编码后的 v2 边界", "/ghcr.io/%76%32/foo", "", "", false},
		{"旧路由不是这种写法", "/v2/ghcr.io/o/r/manifests/t", "", "", false},
		{"registry base 不是这种写法", "/registry/ghcr.io/v2/o/r/manifests/t", "", "", false},
		{"回溯段不归一", "/ghcr.io/v2/a/%2e%2e/x", "", "", false},
		{"主机名不能藏编码斜杠", "/ghcr.io%2Fevil.example/v2/foo", "", "", false},
		{"协议头之后第一段不含点", "/https://v2/ghcr.io/o", "", "", false},
		{"不以斜杠开头", "ghcr.io/v2/o/r/manifests/t", "", "", false},
		{"空路径", "", "", "", false},
	}

	convey.Convey("认出上游主机在前的 registry 写法", t, func() {
		for _, c := range cases {
			convey.Convey(c.name+"："+c.path, func() {
				host, canonical, ok := HostFirstRegistry(c.path)
				convey.So(ok, convey.ShouldEqual, c.ok)
				convey.So(host, convey.ShouldEqual, c.host)
				convey.So(canonical, convey.ShouldEqual, c.canonical)
			})
		}
	})

	convey.Convey("归一后的路径就是旧路由认的那一种", t, func() {
		_, canonical, _ := HostFirstRegistry("/https://ghcr.io/v2/o/r/manifests/t")
		kind, host, rest := Classify(canonical)
		convey.So(kind.String(), convey.ShouldEqual, KindRegistry.String())
		convey.So(host, convey.ShouldEqual, "ghcr.io")
		convey.So(rest, convey.ShouldEqual, "/o/r/manifests/t")
	})
}
