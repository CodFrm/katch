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
	sumDBHost             = "sum.golang.org"
	sumDBRoutePrefix      = sumDBNamespace + "/" + sumDBHost
	goProxyHost           = "proxy.golang.org"
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
//  1. /api/...、/metrics 是 katch 自身的端点；
//  2. /registry/<host>/v2/... 是供会自行追加 /v2 的客户端使用的 registry base；
//  3. /v2 与 /v2/ 是 registry 的探测请求；
//  4. /v2/<host>/... 是 registry 协议，主机名在 /v2/ 之后；
//  5. 第一段含 . 的，该段即主机名（决策 2：公网主机名必然含点）；
//  6. 其余交给 SPA。
//
// 只有 KindRegistry 与 KindStatic 会带回 host/rest，其余两项都是空串。
// KindRegistry 的 rest **不含** /v2 前缀——那是 registry 的协议前缀，由回源侧
// 按上游类别补回，不属于「上游路径」。
//
// 注意第 4 条会把 /favicon.ico 这类根目录下的前端产物也判成上游（它含点）。
// 这不是 bug：调用方先查 dist 再问 Classify，命中的产物文件永远赢——dist 是
// 编译期固定的一小撮文件，而上游是运行时可增删的记录，让固定的那份优先才不会
// 因为谁加了一条上游就把界面打坏。
func Classify(path string) (Kind, string, string) {
	if path == "" || path[0] != '/' {
		return KindSPA, "", ""
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
	kind, host, rest := upstream(KindStatic, strings.TrimPrefix(path, "/"))
	if kind == KindStatic && strings.EqualFold(host, goProxyHost) && sumDBNamespacePath(rest) {
		return classifySumDB(rest)
	}
	return kind, host, rest
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
	if !safeTail(strings.TrimPrefix(rest, "/")) || !validSumDBRoute(rest) {
		return KindInvalid, "", ""
	}
	return KindSumDB, sumDBHost, rest
}

func validSumDBRoute(path string) bool {
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
