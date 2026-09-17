// Package origin 是回源侧的 HTTP 客户端：拿一条上游记录的回源地址加一段上游路径，
// 发一次请求，把响应流式交回来。
//
// 初始上游由 proxy_svc 的白名单选择；这里对它及每次重定向做目标解析、地址固定与
// 传输安全校验。缓存仍不属于这一层。
package origin

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/destination"
)

// Request 一次回源请求。
type Request struct {
	// Method 只有 GET / HEAD 有意义：katch 是镜像，不是可写代理。
	Method string
	// Origin 回源地址，来自上游记录，可以带基路径（https://mirror.example.com/debian）。
	Origin string
	// Path 上游侧路径，转义形态、以 / 开头，由 dispatch.Classify 给出。
	Path string
	// RawQuery 原样带过去的查询串。
	RawQuery string
	// Header 客户端的请求头。按白名单转发，不在白名单里的一律不带走。
	Header http.Header
	// Body 请求体，nil 表示这次回源没有请求体（绝大多数回源都是这一类）。
	//
	// 它只被读一次，读完就没了：调用方因此不能拿同一个 Request 重试——重发的
	// 那一次会给上游送一个空的请求体，而上游会拿它当一次合法的空协商来答复。
	Body io.Reader
	// ContentLength 请求体的长度，-1 表示未知（分块传输）。Body 为 nil 时无意义。
	//
	// 原样抄客户端那一侧的值：git 客户端自己会在小请求上给长度、大请求上分块，
	// 由 katch 改写这件事没有任何好处。
	ContentLength int64
	// Requirement 描述初始目标的地址策略；跨主机重定向会自动收紧为注册且公网可达，
	// registry 的跨主机字节下载还会改用 static / none 的目标要求。
	Requirement destination.DestinationRequirement
	// Authorization katch **自己**换来的上游凭据，空串表示匿名请求。
	//
	// 它和 Header 分开是刻意的：Header 是客户端那一侧的头，白名单里永远不会有
	// Authorization（决策 11，客户端的凭据不外泄给上游）；这一项是 katch 向上游
	// 出示的凭据，目前只有 registry 适配器换到 token 时会填（决策 5）。
	Authorization string
}

// Response 上游的响应。Body 由调用方负责关闭。
type Response struct {
	StatusCode    int
	Header        http.Header
	ContentLength int64
	Body          io.ReadCloser
}

// forwardedRequestHeaders 唯一会被转发给上游的请求头。
//
// 白名单而不是黑名单：决策 11 要求不把客户端的 Authorization 交给上游，而黑名单
// 意味着每出现一个新的凭据头都要记得去补一条，漏一次就是一次凭据泄漏。
// 这几个头缺了会真的坏事——没有 Range 就没有断点续传和分段拉取，没有条件请求头
// 就每次都要整份重下，没有 Accept 时 registry 不知道该给哪个 manifest 版本，
// 没有 Content-Type 上游认不出这是一个 upload-pack 请求，没有 Git-Protocol
// 协商会退回协议 v0（决策 10）。Content-Encoding 与 Content-Type 同一类：它说的
// 是**请求体自己**是什么形态，漏了它而请求体还是压缩字节，上游就会按明文去解它
// （git 对超过 1KB 的协商请求体正是这么发的）。
//
// 入口解开了编码的那种请求，转发的头里会把 Content-Encoding 摘掉（web 包的
// gitRequestBody），否则就是反过来的同一个错：声明 gzip、送的却是明文。
// 多出来的这几项各有确定用途，白名单仍是白名单。
var forwardedRequestHeaders = []string{
	"Accept",
	"Accept-Encoding",
	"Range",
	"If-Range",
	"If-None-Match",
	"If-Modified-Since",
	"User-Agent",
	"Content-Type",
	"Content-Encoding",
	"Git-Protocol",
}

// hopByHopHeaders 只在单跳内成立的响应头，不能转给客户端。
// Set-Cookie 一并去掉：上游的 cookie 在 katch 的域名下没有任何意义，带过去只会
// 变成一个由 katch 背书、发给所有拉取者的 cookie。
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Proxy-Connection",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
	"Set-Cookie",
}

// Options 构造回源客户端。
type Options struct {
	Resolver        destination.DestinationResolver
	TLSClientConfig *tls.Config
}

// Client 回源客户端。
type Client struct {
	http *http.Client
}

type resolvedTargetKey struct{}
type requirementKey struct{}

type resolvedTransport struct {
	base     *http.Transport
	resolver destination.DestinationResolver
}

func (t *resolvedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	requirement, _ := req.Context().Value(requirementKey{}).(destination.DestinationRequirement)
	target, err := t.resolver.Resolve(req.Context(), req.URL, requirement)
	if err != nil {
		return nil, err
	}
	cloned := req.Clone(context.WithValue(req.Context(), resolvedTargetKey{}, target))
	urlCopy := *target.URL
	cloned.URL = &urlCopy
	cloned.Host = target.Host
	return t.base.RoundTrip(cloned)
}

// New 构造回源客户端。
//
// **不设 http.Client.Timeout**：那是一个覆盖整次请求（含响应体读取）的总时限，
// 一个几百 MB 的镜像层会稳定地在读到一半时被它掐断。
//
// 也**不设 ResponseHeaderTimeout**：「单次回源多久拿不到响应算失败」是 setting 表
// 里的运行时项，由调用方（proxy_svc）在每次回源时按当时的设置加一个只跑到响应头
// 为止的计时器。在这里再钉一个固定值，会让设置页上调大的超时被悄悄截断在这个数上——
// 一个改了却不生效的设置比没有这个设置更糟。连接建立仍有自己的硬上限（下面两个），
// 那是 TCP/TLS 这一步的事，和整次回源的时限不是一回事。
func New(options ...Options) *Client {
	var opt Options
	if len(options) > 0 {
		opt = options[0]
	}
	if opt.Resolver == nil {
		opt.Resolver = destination.New(destination.Options{})
	}
	tlsConfig := opt.TLSClientConfig
	if tlsConfig != nil {
		tlsConfig = tlsConfig.Clone()
		tlsConfig.ServerName = ""
	}
	transport := &http.Transport{
		// A proxy would replace the validated destination with its own dial target.
		Proxy: nil,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			target, ok := ctx.Value(resolvedTargetKey{}).(*destination.ResolvedTarget)
			if !ok {
				return nil, destination.ErrDestinationNotAllowed
			}
			return (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).
				DialContext(ctx, network, target.DialAddress)
		},
		TLSClientConfig:       tlsConfig,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	client := &http.Client{Transport: &resolvedTransport{base: transport, resolver: opt.Resolver}}
	client.CheckRedirect = checkRedirect
	return &Client{http: client}
}

func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	previous := via[len(via)-1]
	if req.URL.User != nil || (req.URL.Scheme != "http" && req.URL.Scheme != "https") ||
		(previous.URL.Scheme == "https" && req.URL.Scheme != "https") ||
		(previous.Method != http.MethodGet && previous.Method != http.MethodHead) {
		return destination.ErrDestinationNotAllowed
	}
	requirement, _ := previous.Context().Value(requirementKey{}).(destination.DestinationRequirement)
	if !sameHostname(previous.URL, req.URL) {
		requirement.RequireRegistered = true
		requirement.AddressPolicy = destination.PublicAddressesOnly
		if requirement.Transport == upstream_entity.ProtocolRegistry {
			requirement.Transport = upstream_entity.ProtocolStatic
			requirement.Profile = upstream_entity.PackageProfileNone
		}
	}
	if !sameAuthority(previous.URL, req.URL) {
		req.Header.Del("Authorization")
		req.Header.Del("Proxy-Authorization")
		req.Header.Del("Cookie")
	}
	*req = *req.WithContext(context.WithValue(req.Context(), requirementKey{}, requirement))
	return nil
}

func sameHostname(a, b *url.URL) bool {
	return strings.EqualFold(strings.TrimSuffix(a.Hostname(), "."), strings.TrimSuffix(b.Hostname(), "."))
}

func sameAuthority(a, b *url.URL) bool {
	return strings.EqualFold(a.Host, b.Host)
}

// Do 发起一次回源请求。
//
// 上游返回 4xx/5xx 不算错误——那是一个要原样透传给客户端的响应；只有连不上、
// 超时这类拿不到响应的情况才返回 error（上层据此回 502）。
func (c *Client) Do(ctx context.Context, req *Request) (*Response, error) {
	target, err := buildURL(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, target, req.Body)
	if err != nil {
		return nil, err
	}
	if req.Body != nil {
		// NewRequest 只认得几种已知类型的长度，别的一律按未知（分块）处理。
		// 客户端给了长度就照抄回去，免得一个本来带 Content-Length 的小请求
		// 变成分块传输——有的上游对分块的 upload-pack 请求并不友好。
		httpReq.ContentLength = req.ContentLength
	}
	for _, k := range forwardedRequestHeaders {
		if v := req.Header.Values(k); len(v) > 0 {
			httpReq.Header[http.CanonicalHeaderKey(k)] = append([]string(nil), v...)
		}
	}
	if req.Authorization != "" {
		// 放在白名单复制之后：这一项是 katch 的凭据，不受客户端请求头影响。
		httpReq.Header.Set("Authorization", req.Authorization)
	}
	httpReq = httpReq.WithContext(context.WithValue(httpReq.Context(), requirementKey{}, req.Requirement))
	// 响应体不在这里关：它就是要流式交给调用方的那个东西，调用方负责 Close。
	resp, err := c.http.Do(httpReq)
	if err != nil {
		if errors.Is(err, destination.ErrDestinationNotAllowed) {
			return nil, destination.ErrDestinationNotAllowed
		}
		return nil, fmt.Errorf("回源失败: %w", err)
	}
	return &Response{
		StatusCode:    resp.StatusCode,
		Header:        cleanResponseHeader(resp.Header),
		ContentLength: resp.ContentLength,
		Body:          resp.Body,
	}, nil
}

// buildURL 把回源地址和上游路径拼成最终 URL。
//
// 拼字符串再 Parse，而不是先解码成 url.URL 再赋值 Path：后者会把 %2F、%2B 这类
// 转义在重新编码时改写，Go module proxy 和 APT 的路径都会因此请求到别的对象上。
func buildURL(req *Request) (string, error) {
	base, err := url.Parse(req.Origin)
	if err != nil {
		return "", fmt.Errorf("回源地址无法解析: %w", err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		// 只认 http(s)：file://、gopher:// 这类 scheme 会把回源变成本机读文件。
		return "", fmt.Errorf("回源地址的协议不被支持: %s", base.Scheme)
	}
	target, err := url.Parse(strings.TrimSuffix(req.Origin, "/") + req.Path)
	if err != nil {
		return "", fmt.Errorf("回源地址无法解析: %w", err)
	}
	target.RawQuery = req.RawQuery
	return target.String(), nil
}

// cleanResponseHeader 复制一份去掉 hop-by-hop 的响应头。
//
// 其余的头一律原样带回：Content-Type、Content-Length、ETag、Last-Modified、
// Accept-Ranges、Content-Range、Docker-Content-Digest 少一个，客户端的校验或
// 断点续传就会出问题。
func cleanResponseHeader(src http.Header) http.Header {
	dst := src.Clone()
	if dst == nil {
		return http.Header{}
	}
	// Connection 里点名的头也是 hop-by-hop 的，一并去掉。
	for _, name := range src.Values("Connection") {
		for _, one := range strings.Split(name, ",") {
			dst.Del(strings.TrimSpace(one))
		}
	}
	for _, k := range hopByHopHeaders {
		dst.Del(k)
	}
	return dst
}
