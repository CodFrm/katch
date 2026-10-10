// Package dispatch 判定一个请求路径归谁。
//
// 这是纯函数：只看路径字符串，不查库、不认识上游表。「这个主机是否被允许」是
// 白名单的事（决策 6），由 proxy_svc 查上游表回答；这里只回答「路径的哪一段是
// 主机名、剩下的是什么」。两件事分开，分段规则才能被穷举地测。
package dispatch

import (
	"net/url"
	"strings"
)

const (
	// registryBaseNamespace is an explicit base for clients such as Homebrew that
	// append their own /v2/<repository> path to a configured artifact domain.
	// Keeping the boundary in the route avoids guessing whether a repository's
	// legitimate first segment named v2 is a protocol prefix.
	registryBaseNamespace = "/registry"
	sumDBNamespace        = "/sumdb"
	// SumDBHost 是 sumdb 别名固定指向的那一个 checksum database。路由在这里认它，
	// proxy_svc 的 checksum 桥也按它改写目标：两边各写一遍字符串，改一处就会让
	// 另一边把请求送去一个 nobody 认识的主机。
	SumDBHost        = "sum.golang.org"
	sumDBRoutePrefix = sumDBNamespace + "/" + SumDBHost
	goProxyHost      = "proxy.golang.org"
)

// Kind 一个路径的归属。
type Kind int

const (
	// KindSPA 交给前端 SPA，回落 index.html。
	KindSPA Kind = iota
	// KindSelf katch 自身的端点（/api/...、/metrics）。
	KindSelf
	// KindRegistryPing 对 /v2/ 本身的探测，直接回 200。
	KindRegistryPing
	// KindRegistry container registry 协议，主机名在 /v2/ **之后**。
	KindRegistry
	// KindStatic 其余上游，第一段就是主机名。
	KindStatic
	// KindSumDB 仅用于两个公开 checksum database 别名，保留到策略与缓存边界。
	KindSumDB
	// KindInvalid 形如上游请求、却拿不出可用主机名或路径含 .. 回溯段。
	// 它和「主机不在白名单里」一样返回 404，不给出任何区别。
	KindInvalid
)

// IsPull 这一类请求是不是一次上游拉取。
//
// 三处要按同一条判据分流：访问规则只约束拉取、拉取指标只数拉取、web 只把拉取交给
// 缓存与代理。各写各的条件，加一个新的 Kind 时必定漏掉其中一处——sumdb 别名就是
// 这么从 /metrics 和最近请求里消失过一次的。
func IsPull(kind Kind) bool {
	switch kind {
	case KindRegistry, KindStatic, KindSumDB:
		return true
	default:
		return false
	}
}

// String 让表驱动用例的失败信息可读。
func (k Kind) String() string {
	switch k {
	case KindSPA:
		return "SPA"
	case KindSelf:
		return "Self"
	case KindRegistryPing:
		return "RegistryPing"
	case KindRegistry:
		return "Registry"
	case KindStatic:
		return "Static"
	case KindSumDB:
		return "SumDB"
	case KindInvalid:
		return "Invalid"
	}
	return "Unknown"
}

// Classify 判定路径归属，返回 (归属, 上游主机名, 上游侧路径)。
//
// 入参是**转义形态**的路径（http.Request.URL.EscapedPath()）：上游路径里的
// %2F、%21 之类必须原样带到上游，先解码再拼回去会改写字节。返回的 host 是解码
// 后的形态（它是查上游表的 key），rest 保持转义形态且总是以 / 开头。
//
// 判定顺序即决策顺序，第一条命中即止：
//
//  1. /http://<host>/...、/https://<host>/... 是整条上游 URL 粘在后面的写法，
//     协议头摘掉，其后只能是上游主机名；
//  2. /api/...、/metrics 是 katch 自身的端点；
//  3. /registry/<host>/v2/... 是供会自行追加 /v2 的客户端使用的 registry base；
//  4. /v2 与 /v2/ 是 registry 的探测请求；
//  5. /v2/<host>/... 是 registry 协议，主机名在 /v2/ 之后；
//  6. 第一段含 . 的，该段即主机名（决策 2：公网主机名必然含点）；
//  7. 其余交给 SPA。
//
// /<host>/v2/...（主机名在前的 registry 写法）不在这张表里：它和 static 上游
// 在 /v2/ 之下的对象同一个形态，分得开的只有上游表，见 HostFirstRegistry。
//
// 只有 KindRegistry、KindStatic 与 KindSumDB 会带回 host/rest，其余都是空串。
// KindRegistry 的 rest **不含** /v2 前缀——那是 registry 的协议前缀，由回源侧
// 按上游类别补回，不属于「上游路径」。
//
// 注意第 6 条会把 /favicon.ico 这类根目录下的前端产物也判成上游（它含点）。
// 这不是 bug：调用方先查 dist 再问 Classify，命中的产物文件永远赢——dist 是
// 编译期固定的一小撮文件，而上游是运行时可增删的记录，让固定的那份优先才不会
// 因为谁加了一条上游就把界面打坏。
func Classify(path string) (Kind, string, string) {
	if path == "" || path[0] != '/' {
		return KindSPA, "", ""
	}
	if rest, ok := stripScheme(path); ok {
		kind, host, tail := classifyStatic(rest)
		if kind == KindSPA {
			// 带着协议头就是一次明确的上游尝试，第一段不含点便拿不出上游；
			// 回落给 SPA 会让它拿到 200 + HTML，也会让协议头成了绕到保留段的路。
			return KindInvalid, "", ""
		}
		return kind, host, tail
	}
	if path == "/api" || path == "/metrics" ||
		strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/metrics/") {
		return KindSelf, "", ""
	}
	if path == registryBaseNamespace || strings.HasPrefix(path, registryBaseNamespace+"/") {
		return classifyRegistryBase(path)
	}
	if sumDBNamespacePath(path) {
		return classifySumDB(path)
	}
	if path == "/v2" || path == "/v2/" {
		return KindRegistryPing, "", ""
	}
	if rest, ok := strings.CutPrefix(path, "/v2/"); ok {
		// registry 客户端固定请求 /v2 前缀，所以这里的第一段必然是主机名；
		// 它不含点就是个拿不出上游的请求，和未知主机一样 404。
		return upstream(KindRegistry, rest)
	}
	return classifyStatic(strings.TrimPrefix(path, "/"))
}

// classifyStatic 按「第一段即主机名」解 rest（不带开头的斜杠）。
func classifyStatic(rest string) (Kind, string, string) {
	kind, host, tail := upstream(KindStatic, rest)
	if kind == KindStatic && strings.EqualFold(host, goProxyHost) && sumDBNamespacePath(tail) {
		return classifySumDB(tail)
	}
	return kind, host, tail
}

// stripScheme 摘掉粘在路径最前面的 http:// 或 https://，返回其后不带开头斜杠的
// 那一段。
//
// 「前缀 + 整条原始 URL」是 GitHub 代理通行的写法（gh-proxy、ghfast），客户端
// 照那个约定拼出来的就是 /https://<host>/...。协议头本身不带信息——回源走什么
// 协议由上游记录的回源地址决定——所以摘掉即可。单斜杠那一种是反代合并过重复
// 斜杠（nginx 的 merge_slashes 默认开着）之后的同一个请求。只摘一层。
func stripScheme(path string) (string, bool) {
	rest := strings.TrimPrefix(path, "/")
	scheme, after, ok := strings.Cut(rest, ":/")
	if !ok || !strings.EqualFold(scheme, "http") && !strings.EqualFold(scheme, "https") {
		return "", false
	}
	return strings.TrimPrefix(after, "/"), true
}

// HostFirstRegistry 认出「上游主机在前」的 registry 写法 /<host>/v2/...（前面可
// 带协议头），返回主机名与归一成旧路由 /v2/<host>/... 的路径。
//
// 这是「把 https:// 换成 https://<katch>/，其余照抄」落到 registry 上的样子：
// https://ghcr.io/v2/<repository>/... 照抄过来，/v2 就在主机名之后；自己追加
// /v2/<repository> 的客户端把 <katch>/<host> 当 registry base 时也是它。
//
// 这里只认形态。主机名之后的 /v2 是协议边界还是上游路径的第一段，路径本身分不
// 出来：一条只开 static 的上游（registry 的 blob 重定向落点就是）在 /v2/ 之下
// 有它自己的对象。调用方必须先确认这台主机开了 registry 协议再用 canonical。
// 只摘主机名之后的那一个 v2，仓库名可以合法地以 v2 开头。
func HostFirstRegistry(path string) (host, canonical string, ok bool) {
	if path == "" || path[0] != '/' {
		return "", "", false
	}
	rest, schemed := stripScheme(path)
	if !schemed {
		rest = strings.TrimPrefix(path, "/")
	}
	kind, host, tail := upstream(KindStatic, rest)
	if kind != KindStatic || tail != "/v2" && !strings.HasPrefix(tail, "/v2/") {
		return "", "", false
	}
	rawHost, _, _ := strings.Cut(rest, "/")
	return host, "/v2/" + rawHost + strings.TrimPrefix(tail, "/v2"), true
}

func sumDBNamespacePath(path string) bool {
	return path == sumDBNamespace || strings.HasPrefix(path, sumDBNamespace+"/")
}

func classifySumDB(path string) (Kind, string, string) {
	if path != sumDBRoutePrefix && !strings.HasPrefix(path, sumDBRoutePrefix+"/") {
		return KindInvalid, "", ""
	}
	rest := strings.TrimPrefix(path, sumDBRoutePrefix)
	if rest == "" {
		rest = "/"
	}
	if !safeTail(strings.TrimPrefix(rest, "/")) || !ValidSumDBRoute(rest) {
		return KindInvalid, "", ""
	}
	return KindSumDB, SumDBHost, rest
}

// ValidSumDBRoute checksum database 协议认得的那几条路由。
//
// 导出给桥那一侧复用：路由表写两份，加一条路由时就会有一处留在旧表上——分发层
// 放行而桥 404，或者反过来。
func ValidSumDBRoute(path string) bool {
	return path == "/supported" || path == "/latest" ||
		strings.HasPrefix(path, "/lookup/") && len(path) > len("/lookup/") ||
		strings.HasPrefix(path, "/tile/") && len(path) > len("/tile/")
}

// classifyRegistryBase parses the one route whose /v2 segment is a delimiter
// supplied by katch rather than part of the repository path. The host and tail
// still go through upstream so escaping and traversal rules stay identical to
// the legacy /v2/<host>/... registry route.
func classifyRegistryBase(path string) (Kind, string, string) {
	rest := strings.TrimPrefix(path, registryBaseNamespace)
	if rest == "" || rest[0] != '/' {
		return KindInvalid, "", ""
	}
	kind, host, tail := upstream(KindRegistry, strings.TrimPrefix(rest, "/"))
	if kind != KindRegistry {
		return KindInvalid, "", ""
	}
	if tail == "/v2" {
		return KindRegistry, host, "/"
	}
	if registryPath, ok := strings.CutPrefix(tail, "/v2/"); ok {
		return KindRegistry, host, "/" + registryPath
	}
	return KindInvalid, "", ""
}

// upstream 把「主机名/剩余路径」这段形状解出来，拿不出主机名时降级。
func upstream(kind Kind, rest string) (Kind, string, string) {
	rawHost, tail, _ := strings.Cut(rest, "/")
	host, err := url.PathUnescape(rawHost)
	if err != nil {
		return KindInvalid, "", ""
	}
	// 不含点：这一段是 katch 自己的保留段，不是主机名（决策 2）。
	if !strings.Contains(host, ".") {
		return degrade(kind), "", ""
	}
	// 含点却解出斜杠（%2F 藏了一层路径）：它像主机名但不是，这种请求
	// 没有任何理由回落给 SPA——那会给一次明显的上游尝试回 200 + HTML。
	if strings.Contains(host, "/") {
		return KindInvalid, "", ""
	}
	if !safeTail(tail) {
		return KindInvalid, "", ""
	}
	return kind, host, "/" + tail
}

// degrade 决定「第一段不是主机名」时落到哪里：顶层路径交还给 SPA（不含点的第一段
// 本来就是 katch 自己的保留段），而 /v2/ 之下没有别的东西可交，只能是 404。
func degrade(kind Kind) Kind {
	if kind == KindRegistry {
		return KindInvalid
	}
	return KindSPA
}

// safeTail 挡住回溯段。上游记录的回源地址可以带基路径（https://mirror/debian），
// 路径里混进 .. 就能爬出那个基路径去请求同一主机上的别处，把「代理一个目录」
// 悄悄变成「代理整台主机」。%2e%2e 是同一件事的转义写法，所以逐段解码后再比。
func safeTail(tail string) bool {
	if tail == "" {
		return true
	}
	for _, seg := range strings.Split(tail, "/") {
		decoded, err := url.PathUnescape(seg)
		if err != nil {
			return false
		}
		if decoded == ".." || decoded == "." {
			return false
		}
	}
	return true
}
