package registry

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/smartystreets/goconvey/convey"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/destination"
)

// TestUpstreamPath 路径补全是纯函数，穷举着测。
//
// library/ 补全只由入参那个标志决定——它来自上游记录（决策 12）。适配器里不许
// 出现 docker.io 这个字符串，否则「再加一个 registry 只要加一条记录」就不成立了。
func TestUpstreamPath(t *testing.T) {
	cases := []struct {
		name       string
		path       string
		completion bool
		wantPath   string
		wantScope  string
	}{
		{"单段仓库名补成 library/", "/redis/manifests/7", true,
			"/v2/library/redis/manifests/7", "repository:library/redis:pull"},
		{"标志关着就不补", "/redis/manifests/7", false,
			"/v2/redis/manifests/7", "repository:redis:pull"},
		{"已经带命名空间的不重复补", "/library/redis/manifests/7", true,
			"/v2/library/redis/manifests/7", "repository:library/redis:pull"},
		{"多段命名空间原样带过去", "/ns/sub/repo/blobs/sha256:abc", true,
			"/v2/ns/sub/repo/blobs/sha256:abc", "repository:ns/sub/repo:pull"},
		{"按 digest 的 manifest 同样只改仓库名", "/redis/manifests/sha256:abc", true,
			"/v2/library/redis/manifests/sha256:abc", "repository:library/redis:pull"},
		{"tags/list 也是一个动词段", "/redis/tags/list", true,
			"/v2/library/redis/tags/list", "repository:library/redis:pull"},
		{"仓库名恰好叫 blobs 时按最后一个动词段断句", "/blobs/manifests/7", true,
			"/v2/library/blobs/manifests/7", "repository:library/blobs:pull"},
		{"拿不出仓库名的请求只补协议前缀", "/_catalog", true, "/v2/_catalog", ""},
		{"根路径同理", "/", true, "/v2/", ""},
	}
	convey.Convey("求回源路径与 scope", t, func() {
		for _, c := range cases {
			convey.Convey(c.name+"："+c.path, func() {
				path, scope := upstreamPath(c.path, c.completion)
				convey.So(path, convey.ShouldEqual, c.wantPath)
				convey.So(scope, convey.ShouldEqual, c.wantScope)
			})
		}
	})
}

// fakeUpstream 一个按 token 放行的假 registry。challenge 为空表示给一条正常的
// Bearer 挑战，否则用给定的那一条。
type fakeUpstream struct {
	srv        *httptest.Server
	tokenSrv   *httptest.Server
	tokenHits  atomic.Int64
	hits       atomic.Int64
	authSeen   []string
	acceptAuth atomic.Bool
}

const upstreamToken = "issued-token"

func newFakeUpstream(t *testing.T, challenge string, tokenStatus int, expiresIn int) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{}
	f.acceptAuth.Store(true)

	f.tokenSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.tokenHits.Add(1)
		if tokenStatus != http.StatusOK {
			w.WriteHeader(tokenStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"token":"`+upstreamToken+`","expires_in":`+
			strconv.Itoa(expiresIn)+`}`)
	}))
	t.Cleanup(f.tokenSrv.Close)

	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.authSeen = append(f.authSeen, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") == "Bearer "+upstreamToken && f.acceptAuth.Load() {
			_, _ = io.WriteString(w, "manifest-bytes")
			return
		}
		value := challenge
		if value == "" {
			value = `Bearer realm="` + f.tokenSrv.URL + `/token",service="reg",scope="repository:library/redis:pull"`
		}
		w.Header().Set("WWW-Authenticate", value)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"errors":[]}`)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUpstream) request() *Request {
	return &Request{
		Host: "docker.io", Origin: f.srv.URL, Path: "/redis/manifests/7",
		Method: http.MethodGet, LibraryCompletion: true,
		// 客户端自己的凭据：它永远不该出现在发往上游的请求上（决策 11）。
		Header: http.Header{"Authorization": []string{"Bearer client-secret"}},
	}
}

func read(t *testing.T, body io.ReadCloser) string {
	t.Helper()
	defer func() { _ = body.Close() }()
	b, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type localResolver struct{}

func (localResolver) Resolve(
	_ context.Context, target *url.URL, _ destination.DestinationRequirement,
) (*destination.ResolvedTarget, error) {
	cloned := *target
	return &destination.ResolvedTarget{URL: &cloned, Authority: target.Host, Host: target.Host,
		ServerName: target.Hostname(), DialAddress: target.Host}, nil
}

func testAdapter(opt Options) *Adapter {
	if opt.Resolver == nil {
		opt.Resolver = localResolver{}
	}
	return New(opt)
}

// TestDo_ClientCredentialsAreNeverForwarded 决策 11 的那一半：katch 换来的 token
// 出去了，客户端自己的 Authorization 一个字节都不许出去。
func TestDo_ClientCredentialsAreNeverForwarded(t *testing.T) {
	convey.Convey("上游看见的只有 katch 换来的 token", t, func() {
		f := newFakeUpstream(t, "", http.StatusOK, 300)
		resp, err := testAdapter(Options{}).Do(context.Background(), f.request())

		convey.So(err, convey.ShouldBeNil)
		convey.So(resp.StatusCode, convey.ShouldEqual, http.StatusOK)
		convey.So(read(t, resp.Body), convey.ShouldEqual, "manifest-bytes")
		convey.So(f.authSeen, convey.ShouldResemble, []string{"", "Bearer " + upstreamToken})
	})
}

// TestDo_StripsChallengeOnAnyStatus 挑战头在任何状态码上都不许流出去。
//
// 不止 401：katch 这一侧没有账号体系，任何一条从它出去的 WWW-Authenticate 都只会
// 让客户端去试一个不存在的鉴权流程。
func TestDo_StripsChallengeOnAnyStatus(t *testing.T) {
	convey.Convey("非 401 的响应也摘掉挑战头", t, func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="https://auth.example.com/token"`)
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, "not found")
		}))
		defer srv.Close()

		resp, err := testAdapter(Options{}).Do(context.Background(), &Request{
			Host: "ghcr.io", Origin: srv.URL, Path: "/o/r/manifests/7", Method: http.MethodGet,
		})
		convey.So(err, convey.ShouldBeNil)
		defer func() { _ = resp.Body.Close() }()
		// 上游的 404 原样透传：那是它对这个对象的判断。
		convey.So(resp.StatusCode, convey.ShouldEqual, http.StatusNotFound)
		convey.So(resp.Header.Get("WWW-Authenticate"), convey.ShouldBeBlank)
	})
}

// TestDo_UnusableChallenge 上游要的是一种换不来 token 的鉴权。
//
// 判据有两条：客户端拿到的不是 401 也没有挑战头，以及 token 端点根本没被打——
// 一条 Basic 挑战里没有任何可以拿去换 token 的地址。
func TestDo_UnusableChallenge(t *testing.T) {
	convey.Convey("挑战无法据以换取 token 时不把 401 交给客户端", t, func() {
		for name, challenge := range map[string]string{
			"Basic 挑战":     `Basic realm="https://reg.example.com/"`,
			"realm 不是绝对地址": `Bearer realm="/token",service="reg"`,
			"realm 是别的协议":  `Bearer realm="file:///etc/passwd"`,
			"根本没有挑战头":      "",
		} {
			convey.Convey(name, func() {
				hits := &atomic.Int64{}
				tokenSrv := httptest.NewServer(http.HandlerFunc(
					func(_ http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
				defer tokenSrv.Close()
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					if challenge != "" {
						w.Header().Set("WWW-Authenticate", challenge)
					}
					w.WriteHeader(http.StatusUnauthorized)
				}))
				defer srv.Close()

				resp, err := testAdapter(Options{}).Do(context.Background(), &Request{
					Host: "reg.example.com", Origin: srv.URL,
					Path: "/o/r/manifests/7", Method: http.MethodGet,
				})
				convey.So(err, convey.ShouldBeNil)
				convey.So(resp.StatusCode, convey.ShouldEqual, http.StatusForbidden)
				convey.So(resp.Header.Get("WWW-Authenticate"), convey.ShouldBeBlank)
				convey.So(read(t, resp.Body), convey.ShouldBeBlank)
				convey.So(hits.Load(), convey.ShouldEqual, 0)
			})
		}
	})
}

// TestDo_StillUnauthorizedAfterExchange 私有仓库：换到了 token 也还是被拒。
//
// 只重试一次，且客户端拿到的仍然不是 401——katch 是匿名的，再换一次也不会变成
// 有权限，把 401 转出去只会让客户端去试一个它无从完成的鉴权。
func TestDo_StillUnauthorizedAfterExchange(t *testing.T) {
	convey.Convey("带着换来的 token 仍被拒时给客户端 403", t, func() {
		f := newFakeUpstream(t, "", http.StatusOK, 300)
		f.acceptAuth.Store(false)

		resp, err := testAdapter(Options{}).Do(context.Background(), f.request())
		convey.So(err, convey.ShouldBeNil)
		convey.So(resp.StatusCode, convey.ShouldEqual, http.StatusForbidden)
		convey.So(resp.Header.Get("WWW-Authenticate"), convey.ShouldBeBlank)
		convey.So(read(t, resp.Body), convey.ShouldBeBlank)
		convey.Convey("只重试一次，不会围着 401 打转", func() {
			convey.So(f.hits.Load(), convey.ShouldEqual, 2)
			convey.So(f.tokenHits.Load(), convey.ShouldEqual, 1)
		})
	})
}

// TestDo_TokenEndpointFailureIsAFetchError 换不到 token 是一次回源失败。
//
// 必须是 error 而不是某个 4xx：上层据此回 502。回 404 会让客户端把「这个对象
// 不存在」当成结论记下来，而这只是鉴权端点此刻不通。
func TestDo_TokenEndpointFailureIsAFetchError(t *testing.T) {
	convey.Convey("鉴权端点出错时返回 error", t, func() {
		f := newFakeUpstream(t, "", http.StatusInternalServerError, 300)

		resp, err := testAdapter(Options{}).Do(context.Background(), f.request())
		convey.So(err, convey.ShouldNotBeNil)
		convey.So(resp, convey.ShouldBeNil)
	})
}

// TestDo_TokenIsReusedUntilItExpires token 缓存的两条边：过期前复用，过期后重换。
func TestDo_TokenIsReusedUntilItExpires(t *testing.T) {
	convey.Convey("token 按（上游、仓库、scope）缓存", t, func() {
		f := newFakeUpstream(t, "", http.StatusOK, 60)
		now := time.Now()
		adapter := testAdapter(Options{Now: func() time.Time { return now }})

		resp, err := adapter.Do(context.Background(), f.request())
		convey.So(err, convey.ShouldBeNil)
		convey.So(read(t, resp.Body), convey.ShouldEqual, "manifest-bytes")
		convey.So(f.tokenHits.Load(), convey.ShouldEqual, 1)

		convey.Convey("同一个仓库的下一次请求不再换", func() {
			resp, err := adapter.Do(context.Background(), f.request())
			convey.So(err, convey.ShouldBeNil)
			convey.So(read(t, resp.Body), convey.ShouldEqual, "manifest-bytes")
			convey.So(f.tokenHits.Load(), convey.ShouldEqual, 1)
			// 也不该再吃一次 401：第一发就带着 token 出去。
			convey.So(f.hits.Load(), convey.ShouldEqual, 3)
		})

		convey.Convey("另一个仓库要另换一个：scope 不同，token 不通用", func() {
			req := f.request()
			req.Path = "/nginx/manifests/1"
			resp, err := adapter.Do(context.Background(), req)
			convey.So(err, convey.ShouldBeNil)
			convey.So(read(t, resp.Body), convey.ShouldEqual, "manifest-bytes")
			convey.So(f.tokenHits.Load(), convey.ShouldEqual, 2)
		})

		convey.Convey("过了有效期就重新换一个", func() {
			// expires_in 是 60 秒，留出 10 秒余量，第 55 秒时那一个已经不该再用。
			now = now.Add(55 * time.Second)
			resp, err := adapter.Do(context.Background(), f.request())
			convey.So(err, convey.ShouldBeNil)
			convey.So(read(t, resp.Body), convey.ShouldEqual, "manifest-bytes")
			convey.So(f.tokenHits.Load(), convey.ShouldEqual, 2)
		})
	})
}

// TestParseChallenge_ScopeWithComma scope 的值本身可以带逗号，按逗号切会切错。
func TestParseChallenge_ScopeWithComma(t *testing.T) {
	convey.Convey("解析带逗号的 scope", t, func() {
		c, err := parseChallenge(
			`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",` +
				`scope="repository:library/redis:pull,push"`)
		convey.So(err, convey.ShouldBeNil)
		convey.So(c.Realm, convey.ShouldEqual, "https://auth.docker.io/token")
		convey.So(c.Service, convey.ShouldEqual, "registry.docker.io")
		convey.So(c.Scope, convey.ShouldEqual, "repository:library/redis:pull,push")
	})
}

func TestTokenRealmRejectsUserinfoAndHTTPSDowngrade(t *testing.T) {
	t.Run("userinfo is rejected as an opaque destination denial", func(t *testing.T) {
		_, err := parseChallenge(`Bearer realm="https://user:secret@auth.example.com/token"`)
		if !errors.Is(err, destination.ErrDestinationNotAllowed) ||
			err.Error() != destination.ErrDestinationNotAllowed.Error() {
			t.Fatalf("error = %v, want uniform destination denial", err)
		}
	})

	t.Run("HTTPS registry cannot downgrade its token realm", func(t *testing.T) {
		var hits atomic.Int64
		tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			_, _ = io.WriteString(w, `{"token":"unexpected"}`)
		}))
		defer tokenServer.Close()
		adapter := testAdapter(Options{})
		_, err := adapter.exchange(context.Background(), "https://registry.example.com",
			challenge{Realm: tokenServer.URL + "/token"}, "")
		if !errors.Is(err, destination.ErrDestinationNotAllowed) ||
			err.Error() != destination.ErrDestinationNotAllowed.Error() {
			t.Fatalf("error = %v, want uniform destination denial", err)
		}
		if hits.Load() != 0 {
			t.Fatalf("downgraded token realm was dialed %d times", hits.Load())
		}
	})
}

func TestDo_TokenRealmDestinationSafety(t *testing.T) {
	t.Run("registered cross-host realm is pinned and receives no client credentials", func(t *testing.T) {
		const token = "realm-token"
		var tokenHits atomic.Int64
		var tokenAuthorization string
		tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tokenHits.Add(1)
			tokenAuthorization = r.Header.Get("Authorization")
			_, _ = io.WriteString(w, `{"token":"`+token+`","expires_in":300}`)
		}))
		defer tokenServer.Close()
		tokenURL := mappedRegistryURL(t, tokenServer.URL, "auth.example.com")

		registryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") == "Bearer "+token {
				_, _ = io.WriteString(w, "manifest")
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+tokenURL+`/token",service="registry.example.com"`)
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer registryServer.Close()
		registryURL := mappedRegistryURL(t, registryServer.URL, "registry.example.com")
		resolver := &realmResolver{
			dials: map[string]string{
				mustRegistryURL(t, registryURL).Host: serverRegistryAddress(t, registryServer.URL),
				mustRegistryURL(t, tokenURL).Host:    serverRegistryAddress(t, tokenServer.URL),
			},
			registered: map[string]bool{"auth.example.com": true},
		}

		resp, err := New(Options{Resolver: resolver}).Do(context.Background(), &Request{
			Host: "registry.example.com", Origin: registryURL,
			Path: "/o/r/manifests/7", Method: http.MethodGet,
			Header: http.Header{"Authorization": {"Bearer client-secret"}},
		})
		if err != nil {
			t.Fatalf("Do() error = %v", err)
		}
		if got := read(t, resp.Body); got != "manifest" {
			t.Fatalf("body = %q", got)
		}
		if tokenHits.Load() != 1 || tokenAuthorization != "" {
			t.Fatalf("token hits/Authorization = %d/%q", tokenHits.Load(), tokenAuthorization)
		}
		var realmRequirement destination.DestinationRequirement
		for _, call := range resolver.calls {
			if mustRegistryURL(t, call.target).Hostname() == "auth.example.com" {
				realmRequirement = call.requirement
			}
		}
		if !realmRequirement.RequireRegistered ||
			realmRequirement.Transport != upstream_entity.ProtocolStatic ||
			realmRequirement.AddressPolicy != destination.PublicAddressesOnly {
			t.Fatalf("realm requirement = %+v", realmRequirement)
		}
	})

	t.Run("unregistered cross-host realm is rejected before dial", func(t *testing.T) {
		var tokenHits atomic.Int64
		tokenServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			tokenHits.Add(1)
		}))
		defer tokenServer.Close()
		tokenURL := mappedRegistryURL(t, tokenServer.URL, "blocked-auth.example.com")
		registryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+tokenURL+`/token"`)
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer registryServer.Close()
		registryURL := mappedRegistryURL(t, registryServer.URL, "registry.example.com")
		resolver := &realmResolver{dials: map[string]string{
			mustRegistryURL(t, registryURL).Host: serverRegistryAddress(t, registryServer.URL),
			mustRegistryURL(t, tokenURL).Host:    serverRegistryAddress(t, tokenServer.URL),
		}, registered: map[string]bool{}}

		resp, err := New(Options{Resolver: resolver}).Do(context.Background(), &Request{
			Host: "registry.example.com", Origin: registryURL,
			Path: "/o/r/manifests/7", Method: http.MethodGet,
		})
		if resp != nil || !errors.Is(err, destination.ErrDestinationNotAllowed) {
			t.Fatalf("response/error = %#v/%v", resp, err)
		}
		if err.Error() != destination.ErrDestinationNotAllowed.Error() {
			t.Fatalf("error leaked destination detail: %q", err)
		}
		if tokenHits.Load() != 0 {
			t.Fatalf("blocked token realm was dialed %d times", tokenHits.Load())
		}
	})
}

type realmResolverCall struct {
	target      string
	requirement destination.DestinationRequirement
}

type realmResolver struct {
	dials      map[string]string
	registered map[string]bool
	calls      []realmResolverCall
}

func (r *realmResolver) Resolve(
	_ context.Context, target *url.URL, requirement destination.DestinationRequirement,
) (*destination.ResolvedTarget, error) {
	r.calls = append(r.calls, realmResolverCall{target: target.String(), requirement: requirement})
	if requirement.RequireRegistered && !r.registered[target.Hostname()] {
		return nil, destination.ErrDestinationNotAllowed
	}
	dial, ok := r.dials[target.Host]
	if !ok {
		return nil, destination.ErrDestinationNotAllowed
	}
	cloned := *target
	return &destination.ResolvedTarget{URL: &cloned, Authority: target.Host, Host: target.Host,
		ServerName: target.Hostname(), DialAddress: dial}, nil
}

func mustRegistryURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func mappedRegistryURL(t *testing.T, serverURL, hostname string) string {
	t.Helper()
	u := mustRegistryURL(t, serverURL)
	u.Host = hostname + ":" + u.Port()
	return u.String()
}

func serverRegistryAddress(t *testing.T, serverURL string) string {
	t.Helper()
	return mustRegistryURL(t, serverURL).Host
}

// TestLifetime 余量不能把短命的 token 变成一次性的。
func TestLifetime(t *testing.T) {
	convey.Convey("按 expires_in 算有效期", t, func() {
		convey.So(lifetime(300), convey.ShouldEqual, 290*time.Second)
		convey.So(lifetime(0), convey.ShouldEqual, 50*time.Second)
		convey.So(lifetime(10), convey.ShouldEqual, 5*time.Second)
	})
}
