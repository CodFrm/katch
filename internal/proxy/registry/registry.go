// Package registry 是 container registry 的上游适配器。
//
// 它只做两件 registry 独有、记录字段表达不了的事（决策 12）：
//
//  1. **token 由 katch 自己换**（决策 5）。上游用 401 + WWW-Authenticate 要求鉴权，
//     katch 按挑战去 token 端点换一个匿名 token 再重试，对客户端始终表现为一个
//     不需要鉴权的 registry——挑战绝不能漏给客户端，漏了 docker 会转头来向 katch
//     鉴权，而 katch 没有账号体系可以回答它。token 按（上游、仓库、scope）缓存
//     到过期前，不为每个请求重新换。
//  2. **路径补全**。/v2 是协议前缀，由这里补回（dispatch 只负责摘掉它）；docker.io
//     的官方镜像还要把 redis 补成 library/redis，这一步由上游记录上的
//     LibraryCompletion 决定，适配器不认识任何具体主机名。
//
// 「哪些路径不可变」不在这里：manifest 按 digest 不可变、按 tag 短 TTL、blob 长期
// 缓存，全都是路径模式，是上游记录上的数据（决策 13），由缓存层按模式判定。
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/CodFrm/katch/internal/metrics"
	"github.com/CodFrm/katch/internal/proxy/origin"
)

// Request 一次 registry 侧的拉取。
type Request struct {
	// Host 上游主机名。它只是 token 缓存的第一段键，适配器不按它做任何分支。
	Host string
	// Origin 回源地址，来自上游记录。
	Origin string
	// Path 上游侧路径，转义形态、以 / 开头、**不含** /v2 协议前缀。
	Path     string
	RawQuery string
	Method   string
	// Header 客户端请求头，仍旧由 origin 按白名单过滤——客户端自己的
	// Authorization 永远不会被带给上游（决策 11）。
	Header http.Header
	// LibraryCompletion 把单段仓库名补成 library/<name>，只对 docker.io 开。
	LibraryCompletion bool
}

// Doer 发一次回源请求。由 origin.Client 实现，测试里可以换掉。
//
// 适配器不自己拿 http.Client 去发拉取请求：流式转发、超时、请求头白名单、
// hop-by-hop 清理都在 origin 那一份里，另起一条会悄悄少掉其中几项。
type Doer interface {
	Do(ctx context.Context, req *origin.Request) (*origin.Response, error)
}

// Options 构造参数，零值可用。
type Options struct {
	// Origin 回源客户端。为 nil 时自己建一个。
	Origin Doer
	// TokenClient 换 token 用的客户端。它和回源分开：换 token 是一次小 JSON 请求，
	// 可以有一个整体超时，而回源不能（几百 MB 的镜像层会被整体超时掐断）。
	TokenClient *http.Client
	// Now 取当前时间，测试里可以拨动它来验证 token 过期。
	Now func() time.Time
	// Metrics token 交换计数的去处，nil 表示进程级那一个。
	Metrics *metrics.Recorder
}

// Adapter registry 上游适配器。
type Adapter struct {
	origin      Doer
	tokenClient *http.Client
	now         func() time.Time

	recorder *metrics.Recorder

	mu     sync.Mutex
	tokens map[string]cachedToken
}

// metrics 取指标去处。延迟到调用时才取进程级那一个：包初始化时就去碰全局
// registry 会让「导入这个包」变成一次注册指标的副作用。
func (a *Adapter) metrics() *metrics.Recorder {
	if a.recorder != nil {
		return a.recorder
	}
	return metrics.Default()
}

type cachedToken struct {
	value     string
	expiresAt time.Time
}

// tokenLeeway 提前多久把 token 当成已过期。
//
// 换来的 token 在上游那边有一个确切的过期点，而请求在路上还要花一点时间；
// 掐着点用会得到一次必然失败的重试。生存期很短时按一半算，免得这个余量
// 反过来把 token 变成一次性的。
const tokenLeeway = 10 * time.Second

// maxTokenBody token 响应的读取上限。它是一小段 JSON，给上游留一个读到天亮的口子
// 没有任何好处。
const maxTokenBody = 1 << 20

// errNoChallenge 上游给了 401，但没给出一条能据以换 token 的挑战。
var errNoChallenge = errors.New("上游没有给出可用的鉴权挑战")

// New 构造 registry 适配器。
func New(opt Options) *Adapter {
	if opt.Origin == nil {
		opt.Origin = origin.New()
	}
	if opt.TokenClient == nil {
		opt.TokenClient = &http.Client{Timeout: 15 * time.Second}
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &Adapter{
		origin:      opt.Origin,
		tokenClient: opt.TokenClient,
		now:         opt.Now,
		recorder:    opt.Metrics,
		tokens:      make(map[string]cachedToken),
	}
}

// Do 发起一次 registry 回源，必要时换 token 并重试一次。
//
// 与 origin.Do 的约定一致：上游的 4xx/5xx 是正常返回值，只有拿不到响应才返回
// error。唯一的改写是 401——客户端不该看见它，见 refuse。
func (a *Adapter) Do(ctx context.Context, req *Request) (*origin.Response, error) {
	path, scope := upstreamPath(req.Path, req.LibraryCompletion)
	key := req.Host + "\x00" + scope

	resp, err := a.send(ctx, req, path, a.token(key))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return strip(resp), nil
	}

	// 缓存里那个（如果有）已经被上游拒了，先扔掉再换，否则下一次还会用它。
	a.forget(key)
	challenge, cerr := parseChallenge(resp.Header.Get("WWW-Authenticate"))
	drain(resp.Body)
	if cerr != nil {
		return refuse(), nil
	}
	token, err := a.exchange(ctx, challenge, scope)
	if err != nil {
		// 计在这里而不是 exchange 里：那一层不知道自己在为哪个上游换 token，
		// 而这一族的标签就是上游（可观测性一节）。
		a.metrics().RecordTokenExchange(req.Host, metrics.TokenFailure)
		// 换不到 token 是一次回源失败（502），不是「这个对象不存在」：把它说成
		// 404 会让客户端把结论记下来。
		return nil, err
	}
	a.metrics().RecordTokenExchange(req.Host, metrics.TokenSuccess)
	a.remember(key, token)

	retried, err := a.send(ctx, req, path, token.value)
	if err != nil {
		return nil, err
	}
	if retried.StatusCode == http.StatusUnauthorized {
		// 带着刚换来的 token 仍旧被拒：katch 是匿名的，换一次也不会变成有权限。
		drain(retried.Body)
		return refuse(), nil
	}
	return strip(retried), nil
}

func (a *Adapter) send(ctx context.Context, req *Request, path, token string) (*origin.Response, error) {
	authorization := ""
	if token != "" {
		authorization = "Bearer " + token
	}
	return a.origin.Do(ctx, &origin.Request{
		Method:        req.Method,
		Origin:        req.Origin,
		Path:          path,
		RawQuery:      req.RawQuery,
		Header:        req.Header,
		Authorization: authorization,
	})
}

// token 取一个还没过期的 token，没有就返回空串。
func (a *Adapter) token(key string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.tokens[key]
	if !ok || !a.now().Before(entry.expiresAt) {
		return ""
	}
	return entry.value
}

func (a *Adapter) remember(key string, token cachedToken) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tokens[key] = token
}

func (a *Adapter) forget(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.tokens, key)
}

// challenge 一条 Bearer 挑战里我们用得上的部分。
type challenge struct {
	Realm   string
	Service string
	Scope   string
}

// parseChallenge 解析 WWW-Authenticate。
//
// 手写解析而不是按逗号切：scope 的值本身可以带逗号（repository:x:pull,push），
// 按逗号切会在推送型 scope 上悄悄切错。
func parseChallenge(value string) (challenge, error) {
	scheme, params, ok := strings.Cut(strings.TrimSpace(value), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		// Basic 之类的挑战没有可以匿名换取的 token，换不了就是换不了。
		return challenge{}, errNoChallenge
	}
	fields := parseAuthParams(params)
	c := challenge{Realm: fields["realm"], Service: fields["service"], Scope: fields["scope"]}
	realm, err := url.Parse(c.Realm)
	if err != nil || (realm.Scheme != "http" && realm.Scheme != "https") || realm.Host == "" {
		// realm 是上游说了算的一个地址，katch 会照着它去发请求；不限成 http(s)
		// 绝对地址，一个被攻陷的上游就能把 katch 指向别处。
		return challenge{}, errNoChallenge
	}
	return c, nil
}

// parseAuthParams 把 key="value", key=value 这串参数解出来。
func parseAuthParams(s string) map[string]string {
	out := make(map[string]string, 3)
	for i := 0; i < len(s); {
		for i < len(s) && (s[i] == ' ' || s[i] == ',' || s[i] == '\t') {
			i++
		}
		start := i
		for i < len(s) && s[i] != '=' && s[i] != ',' {
			i++
		}
		if i >= len(s) || s[i] != '=' {
			break
		}
		key := strings.ToLower(strings.TrimSpace(s[start:i]))
		i++
		var value string
		if i < len(s) && s[i] == '"' {
			i++
			var b strings.Builder
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				b.WriteByte(s[i])
				i++
			}
			if i < len(s) {
				i++
			}
			value = b.String()
		} else {
			start = i
			for i < len(s) && s[i] != ',' {
				i++
			}
			value = strings.TrimSpace(s[start:i])
		}
		if key != "" {
			out[key] = value
		}
	}
	return out
}

// tokenResponse token 端点的响应。access_token 是 OAuth2 形态的别名，两种都要认。
type tokenResponse struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

// exchange 按挑战换一个 token。
func (a *Adapter) exchange(ctx context.Context, c challenge, fallbackScope string) (cachedToken, error) {
	realm, err := url.Parse(c.Realm)
	if err != nil {
		return cachedToken{}, fmt.Errorf("鉴权地址无法解析: %w", err)
	}
	query := realm.Query()
	if c.Service != "" {
		query.Set("service", c.Service)
	}
	// 优先用挑战里的 scope：上游最清楚这次要什么权限。自己算的那个只是兜底。
	scope := c.Scope
	if scope == "" {
		scope = fallbackScope
	}
	if scope != "" {
		query.Set("scope", scope)
	}
	realm.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return cachedToken{}, err
	}
	resp, err := a.tokenClient.Do(req)
	if err != nil {
		return cachedToken{}, fmt.Errorf("换取上游凭据失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return cachedToken{}, fmt.Errorf("换取上游凭据失败: 鉴权端点返回 %d", resp.StatusCode)
	}
	var body tokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxTokenBody)).Decode(&body); err != nil {
		return cachedToken{}, fmt.Errorf("上游凭据无法解析: %w", err)
	}
	value := body.Token
	if value == "" {
		value = body.AccessToken
	}
	if value == "" {
		return cachedToken{}, errors.New("上游凭据为空")
	}
	return cachedToken{value: value, expiresAt: a.now().Add(lifetime(body.ExpiresIn))}, nil
}

// lifetime 一个 token 在 katch 这边按多久算有效。
func lifetime(expiresIn int) time.Duration {
	// 端点没给 expires_in 时按 registry 的惯例算 60 秒。
	if expiresIn <= 0 {
		expiresIn = 60
	}
	full := time.Duration(expiresIn) * time.Second
	if full <= 2*tokenLeeway {
		return full / 2
	}
	return full - tokenLeeway
}

// upstreamPath 求出回源路径与这次拉取的 scope。
//
// 回源路径 = /v2 + （可能补过 library/ 的）仓库名 + 动词段。拿不出仓库名的请求
// （/v2/、_catalog 之类）原样补上 /v2 前缀，scope 交给挑战去说。
func upstreamPath(path string, libraryCompletion bool) (string, string) {
	repository, tail := splitRepository(path)
	if repository == "" {
		return "/v2" + path, ""
	}
	// 补全只看记录上的标志，不看主机名：新加一个需要补全的 registry 应该是加
	// 一条记录，而不是改这里的代码（决策 12）。
	if libraryCompletion && !strings.Contains(repository, "/") {
		repository = "library/" + repository
	}
	return "/v2/" + repository + tail, "repository:" + repository + ":pull"
}

// verbs registry 协议里仓库名之后的那一段。仓库名可以有任意多段，靠它们断句。
var verbs = map[string]bool{"manifests": true, "blobs": true, "tags": true, "referrers": true}

// splitRepository 把「仓库名」和「动词段」切开。
//
// 取最后一个动词段而不是第一个：仓库名本身可以叫 blobs（library/blobs/manifests/7），
// 取第一个会把仓库名切掉半截。
func splitRepository(path string) (string, string) {
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i := len(segments) - 1; i >= 1; i-- {
		if verbs[segments[i]] {
			return strings.Join(segments[:i], "/"), "/" + strings.Join(segments[i:], "/")
		}
	}
	return "", ""
}

// strip 摘掉挑战头再交给上层。
//
// 即便这一次不是 401 也照摘：任何一条从 katch 出去的 WWW-Authenticate 都是在
// 请客户端向 katch 鉴权，而 katch 是一个公开的镜像站，没有账号可以给它。
func strip(resp *origin.Response) *origin.Response {
	resp.Header.Del("WWW-Authenticate")
	return resp
}

// refuse 上游拒绝了这次匿名拉取时给客户端的回应。
//
// 403 而不是把上游的 401 透传出去：401 的语义是「带上凭据再来」，可 katch 这一侧
// 不需要也不接受凭据，把它转出去只会让客户端去试一个不存在的鉴权流程。
// 403 说的是这次拿不到，客户端据此停下来——这正是事实。
func refuse() *origin.Response {
	return &origin.Response{
		StatusCode:    http.StatusForbidden,
		Header:        http.Header{},
		ContentLength: 0,
		Body:          io.NopCloser(strings.NewReader("")),
	}
}

// drain 关掉一个不再需要的响应体。
//
// 先读掉一小截再关：401 的响应体是几十字节的 JSON，读完才能让这条连接回到
// 连接池，否则每次换 token 都会丢掉一条连接。
func drain(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxTokenBody))
	_ = body.Close()
}
