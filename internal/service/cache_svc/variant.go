package cache_svc

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/CodFrm/katch/internal/model/entity/cache_entity"
	"github.com/CodFrm/katch/internal/proxy/dispatch"
	"github.com/CodFrm/katch/internal/service/proxy_svc"
)

// variantMarker 隔开缓存键里的「路径」与「变体」两段。
//
// 取 0x1F（单元分隔符）而不是 `#` 之类的可打印字符：路径以转义形态进来，控制字节
// 在那里一定是 %1F；而请求行里带控制字节的请求会被 net/url 当场拒掉，查询串里也
// 到不了这个字节。于是「键里出现 0x1F」只可能是我们自己拼上去的，**老形态的键
// 与变体键不可能逐字相同**——若用可打印字符做分隔，一个精心构造的查询串就能拼出
// 与某个变体键一模一样的键，撞进 (upstream_id, key) 这条唯一索引里。
//
// 落库这一侧：0x1F 是合法的单字节 UTF-8，VARCHAR 存得下；但 MySQL 的 utf8mb4
// 排序规则把控制字节当成零权重（比较时视同不存在），所以两个变体键之间的区分
// 不能指望这个字节——担这件事的是它后面那段摘要。
const variantMarker = "\x1f"

// cacheVariant 请求里会改变上游返回内容的那一部分，空串表示「没有可言」。
//
// 只认 Accept，而且只在 registry 的 manifest 请求上认（见 acceptVaries）：变体是
// 有代价的，多一档就多一份副本、多一次回源。这里不做通用的 Vary 机制——上游有没有
// 声明 Vary 我们今天既没看也没存，凭空造一个会把每个客户端的 Accept 差异都变成
// 一份新副本。
//
// 变体落成定长摘要而不是 Accept 原文：cache_object.key 是 VARCHAR(500) 且进了
// 唯一索引，一串真实的 Accept 就有两百来字节，原样拼上去会把路径能用的长度吃掉。
func cacheVariant(target *proxy_svc.Target) string {
	if !acceptVaries(target) {
		return ""
	}
	accept := normalizeAccept(target.Header)
	if accept == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(accept))
	return cache_entity.VariantTag + hex.EncodeToString(sum[:cache_entity.VariantDigestLen/2])
}

// acceptVaries 这次请求的 Accept 会不会真的改变上游给出的内容。
//
// 只有 container registry 的 manifest 会按 Accept 换一份内容（上游行为一节：
// 客户端用 Accept 告诉 registry 要 docker 还是 OCI 的 manifest），blob 按 digest
// 寻址、APT 与 Go proxy 的文件一律原样发回，Accept 对它们没有意义。
//
// 范围放宽的代价是实打实的：浏览器打开一个 raw.githubusercontent.com 的文件时发的
// 是一长串 Accept，curl 发的是 */*，把它们也算进变体，同一份文件就会按客户端各存
// 一份，每次拉取都成了未命中——那比眼下这个 bug 更糟。
func acceptVaries(target *proxy_svc.Target) bool {
	return target.Kind == dispatch.KindRegistry && isManifestRequest(target.Path)
}

// isManifestRequest 判断一条 registry 上游侧路径是不是 /<仓库名>/manifests/<引用>。
//
// 按**倒数第二段**认，而不是「路径里含 manifests」：仓库名自己可以叫 manifests
// （library/manifests/blobs/sha256:… 是一次 blob 请求），含不含只会把它认错。
func isManifestRequest(path string) bool {
	ref := strings.LastIndex(path, "/")
	if ref <= 0 || ref == len(path)-1 {
		// 没有引用段（以 / 结尾或压根没有第二段）就不是一次 manifest 请求。
		return false
	}
	return strings.HasSuffix(path[:ref], "/manifests")
}

// registryRequestImmutability 判断 registry 标准端点的保留语义。
// 第二个返回值表示协议是否定义了这个端点：已定义的结果优先于补充路径模式；
// 未定义的非标准端点才继续交给 immutable_patterns。
func registryRequestImmutability(target *proxy_svc.Target) (immutable, defined bool) {
	if target.Kind != dispatch.KindRegistry {
		return false, false
	}
	refAt := strings.LastIndex(target.Path, "/")
	if refAt <= 0 || refAt == len(target.Path)-1 {
		return false, false
	}
	action := target.Path[:refAt]
	ref, ok := registryReference(target.Path[refAt+1:])
	if !ok {
		return false, false
	}
	switch {
	case strings.HasSuffix(action, "/blobs"):
		if registryDigestReference(ref) {
			return true, true
		}
	case strings.HasSuffix(action, "/manifests"):
		if registryDigestReference(ref) {
			return true, true
		}
		if registryTag(ref) {
			return false, true
		}
	case strings.HasSuffix(action, "/referrers"):
		if registryDigestReference(ref) {
			return false, true
		}
	case strings.HasSuffix(action, "/tags"):
		if ref == "list" {
			return false, true
		}
	}
	return false, false
}

func registryReference(raw string) (string, bool) {
	decoded, err := url.PathUnescape(raw)
	return decoded, err == nil && !strings.Contains(decoded, "/")
}

func registryDigestReference(ref string) bool {
	algorithm, encoded, ok := strings.Cut(ref, ":")
	if !ok || algorithm == "" || encoded == "" || !asciiLetter(rune(algorithm[0])) {
		return false
	}
	for _, ch := range algorithm[1:] {
		if !asciiLetter(ch) && (ch < '0' || ch > '9') && !strings.ContainsRune("+._-", ch) {
			return false
		}
	}
	for _, ch := range encoded {
		if !asciiLetter(ch) && (ch < '0' || ch > '9') && !strings.ContainsRune("=_-", ch) {
			return false
		}
	}
	return true
}

func registryTag(ref string) bool {
	if len(ref) == 0 || len(ref) > 128 ||
		(!asciiLetter(rune(ref[0])) && (ref[0] < '0' || ref[0] > '9') && ref[0] != '_') {
		return false
	}
	for _, ch := range ref[1:] {
		if !asciiLetter(ch) && (ch < '0' || ch > '9') && !strings.ContainsRune("_.-", ch) {
			return false
		}
	}
	return true
}

func asciiLetter(ch rune) bool {
	return ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z'
}

// normalizeAccept 把一次请求的 Accept 归一成可比较的形态，同一个意思归一成同一个串。
//
// 「没有 Accept」与「Accept: */*」都归一成空串：两者都是「随便给一份」，也正是
// 改动之前那些缓存行的写入形态，归一到空串它们才继续命中得到（否则库里的存量
// 会在升级的一瞬间全部变成再也查不到的死行）。
//
// 归一得不够，键就会炸：同一个 docker pull 在不同版本、不同工具下的 Accept 会差在
// 空格、先后、q= 和拆成几行上，逐字当键的话每次拉取都是未命中。归一的依据是
// RFC 9110 §12.5.1——媒体范围的先后不表达偏好（偏好由 q 表达），q 的默认值是 1。
func normalizeAccept(header http.Header) string {
	// Values 会把 `Accept: a` 与 `Accept: b` 这样拆成两行发的请求头合起来，
	// 客户端拆不拆行不该换一份副本。
	ranges := make([]string, 0, 8)
	for _, line := range header.Values("Accept") {
		for _, item := range strings.Split(line, ",") {
			if r := normalizeMediaRange(item); r != "" {
				ranges = append(ranges, r)
			}
		}
	}
	slices.Sort(ranges)
	ranges = slices.Compact(ranges)
	if len(ranges) == 0 || (len(ranges) == 1 && ranges[0] == "*/*") {
		return ""
	}
	return strings.Join(ranges, ",")
}

// normalizeMediaRange 归一一条媒体范围，空串表示这一条没有内容、可以丢掉。
func normalizeMediaRange(in string) string {
	fields := strings.Split(in, ";")
	// 媒体类型大小写不敏感，统一小写。
	mediaType := strings.ToLower(strings.TrimSpace(fields[0]))
	if mediaType == "" {
		return ""
	}
	params := make([]string, 0, len(fields)-1)
	for _, field := range fields[1:] {
		if p := normalizeParam(field); p != "" {
			params = append(params, p)
		}
	}
	// 参数之间同样没有先后可言。
	slices.Sort(params)
	params = slices.Compact(params)
	return strings.Join(append([]string{mediaType}, params...), ";")
}

// normalizeParam 归一一个媒体范围参数，空串表示它等同于默认值、可以丢掉。
//
// 参数名与取值一并小写：Accept 里的参数（q、charset、registry 的 v=）取值都是
// 大小写不敏感的记号，分大小写只会让同一个意思落到两个键上。
func normalizeParam(in string) string {
	param := strings.ToLower(strings.TrimSpace(in))
	name, value, ok := strings.Cut(param, "=")
	if !ok {
		return param
	}
	name, value = strings.TrimSpace(name), strings.TrimSpace(value)
	if name == "" {
		return ""
	}
	if name != "q" {
		return name + "=" + value
	}
	q, err := strconv.ParseFloat(value, 64)
	if err != nil {
		// 读不懂就原样留着：读不懂的 q 也可能改变上游的选择，抹掉它等于把两个
		// 未必相同的请求并到一个键上。
		return "q=" + value
	}
	if q >= 1 {
		// q=1 是默认值，写不写都一样。
		return ""
	}
	// 1.0 与 1、0.50 与 0.5 是同一个权重，按最短形态写回去。
	return "q=" + strconv.FormatFloat(q, 'f', -1, 64)
}
