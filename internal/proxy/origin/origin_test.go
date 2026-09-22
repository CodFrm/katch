package origin

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/smartystreets/goconvey/convey"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/destination"
)

var configuredGenericOrigin = destination.DestinationRequirement{
	AddressPolicy: destination.AllowPrivateAddresses,
}

func body(t *testing.T, resp *Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	convey.So(err, convey.ShouldBeNil)
	return string(b)
}

// TestDo_PassesBytesThrough
//
// APT 的 Packages/InRelease 在 GPG 签名的覆盖范围内，响应体改一个字节签名就废了；
// registry 的 blob 同理（digest 对不上）。所以回源拿到什么就原样给出什么。
func TestDo_PassesBytesThrough(t *testing.T) {
	convey.Convey("上游字节与响应头原样带回", t, func() {
		payload := strings.Repeat("deadbeef\n", 1024)
		gotURI := ""
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 断言不能写在假源站的 handler 里：它跑在另一个 goroutine 上，
			// convey.So 脱离 Convey 栈会 panic，表现成一次假的「回源失败」。
			gotURI = r.URL.RequestURI()
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("ETag", `"abc"`)
			// hop-by-hop 头只在单跳内有意义，带给客户端会让它误判连接语义。
			w.Header().Set("Connection", "keep-alive")
			_, _ = io.WriteString(w, payload)
		}))
		defer srv.Close()

		resp, err := New().Do(context.Background(), &Request{
			Method: http.MethodGet, Origin: srv.URL,
			Path: "/dists/stable/InRelease", RawQuery: "x=1", Requirement: configuredGenericOrigin,
		})
		convey.So(err, convey.ShouldBeNil)
		convey.So(resp.StatusCode, convey.ShouldEqual, http.StatusOK)
		convey.So(gotURI, convey.ShouldEqual, "/dists/stable/InRelease?x=1")
		convey.So(resp.Header.Get("Content-Type"), convey.ShouldEqual, "text/plain")
		convey.So(resp.Header.Get("ETag"), convey.ShouldEqual, `"abc"`)
		convey.So(resp.Header.Get("Connection"), convey.ShouldBeEmpty)
		convey.So(body(t, resp), convey.ShouldEqual, payload)
	})
}

// TestDo_DoesNotForwardClientCredentials
//
// 决策 11：首版只代理公开资源，把客户端的 Authorization 转给上游等于把它的凭据
// 泄漏给上游，而且没有任何用途。头部按白名单转发，默认就是不转。
func TestDo_DoesNotForwardClientCredentials(t *testing.T) {
	convey.Convey("客户端凭据不转发给上游", t, func() {
		var got http.Header
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Clone()
		}))
		defer srv.Close()

		clientHeader := http.Header{}
		clientHeader.Set("Authorization", "Basic c2VjcmV0")
		clientHeader.Set("Cookie", "session=secret")
		clientHeader.Set("X-Forwarded-For", "10.0.0.1")
		clientHeader.Set("Range", "bytes=0-1023")
		clientHeader.Set("Accept", "application/vnd.oci.image.manifest.v1+json")

		_, err := New().Do(context.Background(), &Request{
			Method: http.MethodGet, Origin: srv.URL, Path: "/x", Header: clientHeader,
			Requirement: configuredGenericOrigin,
		})
		convey.So(err, convey.ShouldBeNil)
		convey.So(got.Get("Authorization"), convey.ShouldBeEmpty)
		convey.So(got.Get("Cookie"), convey.ShouldBeEmpty)
		convey.So(got.Get("X-Forwarded-For"), convey.ShouldBeEmpty)
		// 白名单里的头照常转发：少了 Range，大文件断点续传就废了。
		convey.So(got.Get("Range"), convey.ShouldEqual, "bytes=0-1023")
		convey.So(got.Get("Accept"), convey.ShouldEqual, "application/vnd.oci.image.manifest.v1+json")
	})
}

func TestDo_ForwardsColdPreconditionsWithoutClientCredentials(t *testing.T) {
	convey.Convey("冷缓存前置条件转发给上游但客户端凭据不外泄", t, func() {
		var got http.Header
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Clone()
			if r.Header.Get("If-Match") == `"current"` &&
				r.Header.Get("If-Unmodified-Since") == "Wed, 21 Oct 2015 07:28:00 GMT" {
				w.WriteHeader(http.StatusPreconditionFailed)
			}
		}))
		defer srv.Close()

		clientHeader := http.Header{}
		clientHeader.Set("If-Match", `"current"`)
		clientHeader.Set("If-Unmodified-Since", "Wed, 21 Oct 2015 07:28:00 GMT")
		clientHeader.Set("Authorization", "Basic c2VjcmV0")
		clientHeader.Set("Proxy-Authorization", "Basic c2VjcmV0")
		clientHeader.Set("Cookie", "session=secret")

		resp, err := New().Do(context.Background(), &Request{
			Method: http.MethodGet, Origin: srv.URL, Path: "/x", Header: clientHeader,
			Requirement: configuredGenericOrigin,
		})
		convey.So(err, convey.ShouldBeNil)
		defer resp.Body.Close()
		convey.So(resp.StatusCode, convey.ShouldEqual, http.StatusPreconditionFailed)
		convey.So(got.Get("If-Match"), convey.ShouldEqual, `"current"`)
		convey.So(got.Get("If-Unmodified-Since"), convey.ShouldEqual, "Wed, 21 Oct 2015 07:28:00 GMT")
		convey.So(got.Get("Authorization"), convey.ShouldBeEmpty)
		convey.So(got.Get("Proxy-Authorization"), convey.ShouldBeEmpty)
		convey.So(got.Get("Cookie"), convey.ShouldBeEmpty)
	})
}

// TestDo_FollowsSameHostRedirect
//
// 同一主机内的安全重定向由 katch 自己跟随；否则客户端在受限网络下仍会被迫直连源站。
func TestDo_FollowsSameHostRedirect(t *testing.T) {
	convey.Convey("同主机 302 由 katch 自己跟随", t, func() {
		asset := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "binary-asset")
		}))
		defer asset.Close()
		release := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, asset.URL+"/objects/x", http.StatusFound)
		}))
		defer release.Close()

		resp, err := New().Do(context.Background(), &Request{
			Method: http.MethodGet, Origin: release.URL, Path: "/o/r/releases/download/v1/x",
			Requirement: configuredGenericOrigin,
		})
		convey.So(err, convey.ShouldBeNil)
		convey.So(resp.StatusCode, convey.ShouldEqual, http.StatusOK)
		convey.So(resp.Header.Get("Location"), convey.ShouldBeEmpty)
		convey.So(body(t, resp), convey.ShouldEqual, "binary-asset")
	})
}

// TestDo_KeepsEscapedPath
//
// Go module proxy 的路径里有 ! 转义，APT 的文件名里有 + 和 ~。先解码再拼回去
// 会改写请求行，上游要么 404 要么给回另一个对象。
func TestDo_KeepsEscapedPath(t *testing.T) {
	convey.Convey("上游路径的转义形态原样送达", t, func() {
		var gotURI string
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			gotURI = r.URL.RequestURI()
		}))
		defer srv.Close()

		_, err := New().Do(context.Background(), &Request{
			Method: http.MethodGet, Origin: srv.URL + "/debian",
			Path: "/pool/main/g/g%2Bb/x%7E1.deb", Requirement: configuredGenericOrigin,
		})
		convey.So(err, convey.ShouldBeNil)
		// 回源地址自带的基路径要保留在前面，转义序列一个字节都不改。
		convey.So(gotURI, convey.ShouldEqual, "/debian/pool/main/g/g%2Bb/x%7E1.deb")
	})
}

// TestDo_UpstreamStatusIsPassedThrough 上游的 4xx 原样透传，不当成回源失败。
func TestDo_UpstreamStatusIsPassedThrough(t *testing.T) {
	convey.Convey("上游 4xx 是一个正常的响应而不是错误", t, func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, "no such file")
		}))
		defer srv.Close()

		resp, err := New().Do(context.Background(), &Request{
			Method: http.MethodGet, Origin: srv.URL, Path: "/x", Requirement: configuredGenericOrigin,
		})
		convey.So(err, convey.ShouldBeNil)
		convey.So(resp.StatusCode, convey.ShouldEqual, http.StatusNotFound)
		convey.So(body(t, resp), convey.ShouldEqual, "no such file")
	})
}

// TestDo_UnreachableOriginIsError 上游不可达要能被上层识别成 502。
func TestDo_UnreachableOriginIsError(t *testing.T) {
	convey.Convey("上游不可达返回错误", t, func() {
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
		srv.Close() // 立刻关掉，拿到一个必然连不上的地址

		_, err := New().Do(context.Background(), &Request{
			Method: http.MethodGet, Origin: srv.URL, Path: "/x", Requirement: configuredGenericOrigin,
		})
		convey.So(err, convey.ShouldNotBeNil)
	})
}

// TestDo_ForwardsRequestBodyAndGitHeaders
//
// 决策 10：穿透 git 的协商请求要把请求体原样送到上游，而 Content-Type 与
// Git-Protocol 必须跟着走——前者是 upload-pack 请求的载体，缺了后者协议会退回
// v0。Content-Encoding 同理：它描述的是**请求体自己**的形态，头与字节对不上时
// 上游会按错的形态去解这份数据（git 对超过 1KB 的协商请求体就是这么压的）。
// 白名单仍然是白名单：这里只多了这几项，客户端的 Authorization 照旧不外传。
func TestDo_ForwardsRequestBodyAndGitHeaders(t *testing.T) {
	convey.Convey("请求体与 git 的两个头原样送达上游", t, func() {
		payload := "0032want d9a1b0c2c3d4e5f60718293a4b5c6d7e8f901234\n0000"
		var (
			gotBody   string
			gotHeader http.Header
			gotMethod string
		)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			gotHeader = r.Header.Clone()
			gotMethod = r.Method
			w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
			_, _ = io.WriteString(w, "0008NAK\n")
		}))
		defer srv.Close()

		clientHeader := http.Header{}
		clientHeader.Set("Content-Type", "application/x-git-upload-pack-request")
		clientHeader.Set("Git-Protocol", "version=2")
		clientHeader.Set("Content-Encoding", "gzip")
		clientHeader.Set("Authorization", "Basic c2VjcmV0")

		resp, err := New().Do(context.Background(), &Request{
			Method: http.MethodPost, Origin: srv.URL,
			Path: "/CodFrm/katch/git-upload-pack", Header: clientHeader,
			Body: strings.NewReader(payload), ContentLength: int64(len(payload)),
			Requirement: configuredGenericOrigin,
		})
		convey.So(err, convey.ShouldBeNil)
		convey.So(gotMethod, convey.ShouldEqual, http.MethodPost)
		convey.So(gotBody, convey.ShouldEqual, payload)
		convey.So(gotHeader.Get("Content-Type"), convey.ShouldEqual, "application/x-git-upload-pack-request")
		convey.So(gotHeader.Get("Git-Protocol"), convey.ShouldEqual, "version=2")
		// 请求体带着编码，这个头就必须跟着走；把它丢掉等于给上游一份声明为明文、
		// 内容是 gzip 的请求。
		convey.So(gotHeader.Get("Content-Encoding"), convey.ShouldEqual, "gzip")
		// 白名单里多两项不等于把凭据也放进来了。
		convey.So(gotHeader.Get("Authorization"), convey.ShouldBeEmpty)
		convey.So(resp.Header.Get("Content-Type"), convey.ShouldEqual, "application/x-git-upload-pack-result")
		convey.So(body(t, resp), convey.ShouldEqual, "0008NAK\n")
	})
}

// TestDo_NilBodyStaysBodyless 没有请求体的回源仍然不带请求体。
//
// 一个带着 Transfer-Encoding: chunked 却没有内容的 GET 会被一部分上游直接拒掉，
// 而 katch 绝大多数回源都是这一类。
func TestDo_NilBodyStaysBodyless(t *testing.T) {
	convey.Convey("没有请求体时不给上游造一个出来", t, func() {
		gotLength := int64(-1)
		gotEncoding := ""
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			gotLength = r.ContentLength
			gotEncoding = strings.Join(r.TransferEncoding, ",")
		}))
		defer srv.Close()

		_, err := New().Do(context.Background(), &Request{
			Method: http.MethodGet, Origin: srv.URL, Path: "/dists/stable/InRelease",
			Requirement: configuredGenericOrigin,
		})
		convey.So(err, convey.ShouldBeNil)
		convey.So(gotLength, convey.ShouldEqual, 0)
		convey.So(gotEncoding, convey.ShouldBeEmpty)
	})
}

func readBody(t *testing.T, resp *Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type resolverCall struct {
	target      string
	requirement destination.DestinationRequirement
}

type mappedResolver struct {
	dials      map[string]string
	registered map[string]destination.RewriteUpstream
	calls      []resolverCall
}

func (r *mappedResolver) Resolve(
	_ context.Context, target *url.URL, requirement destination.DestinationRequirement,
) (*destination.ResolvedTarget, error) {
	r.calls = append(r.calls, resolverCall{target: target.String(), requirement: requirement})
	if requirement.RequireRegistered {
		upstream, ok := r.registered[target.Hostname()]
		if !ok || (requirement.Transport != "" && !upstream.Transports.Has(requirement.Transport)) ||
			(requirement.Profile != "" &&
				upstream_entity.NormalizePackageProfile(upstream.Profile) != requirement.Profile) {
			return nil, destination.ErrDestinationNotAllowed
		}
	}
	dial, ok := r.dials[target.Host]
	if !ok {
		return nil, destination.ErrDestinationNotAllowed
	}
	cloned := *target
	return &destination.ResolvedTarget{
		URL: &cloned, Authority: target.Host, Host: target.Host,
		ServerName: target.Hostname(), DialAddress: dial,
	}, nil
}

func parseTarget(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func mappedURL(t *testing.T, serverURL, hostname string) string {
	t.Helper()
	u, err := url.Parse(serverURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = hostname + ":" + u.Port()
	return u.String()
}

func serverAddress(t *testing.T, serverURL string) string {
	t.Helper()
	u, err := url.Parse(serverURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

func TestDo_RedirectDestinationSafety(t *testing.T) {
	t.Run("registered cross-host redirect is pinned and strips credentials", func(t *testing.T) {
		var targetHits atomic.Int64
		var gotHost, gotAuthorization string
		targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			targetHits.Add(1)
			gotHost = r.Host
			gotAuthorization = r.Header.Get("Authorization")
			_, _ = io.WriteString(w, "asset")
		}))
		defer targetServer.Close()
		targetURL := mappedURL(t, targetServer.URL, "cdn.example.com")

		originServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, targetURL+"/asset", http.StatusFound)
		}))
		defer originServer.Close()
		originURL := mappedURL(t, originServer.URL, "packages.example.com")

		resolver := &mappedResolver{
			dials: map[string]string{
				parseTarget(t, originURL).Host: serverAddress(t, originServer.URL),
				parseTarget(t, targetURL).Host: serverAddress(t, targetServer.URL),
			},
			registered: map[string]destination.RewriteUpstream{
				"cdn.example.com": {
					Profile:    upstream_entity.PackageProfileNPM,
					Transports: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
				},
			},
		}
		resp, err := New(Options{Resolver: resolver}).Do(context.Background(), &Request{
			Method: http.MethodGet, Origin: originURL, Path: "/metadata",
			Authorization: "Bearer origin-token",
			Requirement: destination.DestinationRequirement{
				Transport:     upstream_entity.ProtocolStatic,
				Profile:       upstream_entity.PackageProfileNPM,
				AddressPolicy: destination.PublicAddressesOnly,
			},
		})
		if err != nil {
			t.Fatalf("Do() error = %v", err)
		}
		if got := readBody(t, resp); got != "asset" {
			t.Fatalf("body = %q", got)
		}
		if targetHits.Load() != 1 || gotHost != parseTarget(t, targetURL).Host {
			t.Fatalf("target hits/Host = %d/%q", targetHits.Load(), gotHost)
		}
		if gotAuthorization != "" {
			t.Fatalf("cross-host Authorization = %q", gotAuthorization)
		}
		last := resolver.calls[len(resolver.calls)-1].requirement
		if !last.RequireRegistered || last.AddressPolicy != destination.PublicAddressesOnly ||
			last.Transport != upstream_entity.ProtocolStatic || last.Profile != upstream_entity.PackageProfileNPM {
			t.Fatalf("cross-host requirement = %+v", last)
		}
	})

	t.Run("registry cross-host redirect uses a registered byte-serving CDN", func(t *testing.T) {
		var targetHits atomic.Int64
		var gotURI, gotAuthorization, gotCookie string
		targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			targetHits.Add(1)
			gotURI = r.URL.RequestURI()
			gotAuthorization = r.Header.Get("Authorization")
			gotCookie = r.Header.Get("Cookie")
			_, _ = io.WriteString(w, "registry-blob")
		}))
		defer targetServer.Close()
		targetURL := mappedURL(t, targetServer.URL, "pkg-containers.githubusercontent.com")

		originServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", targetURL+"/ghcr1/blobs/sha256:abc?se=2030-01-01&sig=signed%2Bvalue")
			w.WriteHeader(http.StatusTemporaryRedirect)
		}))
		defer originServer.Close()
		originURL := mappedURL(t, originServer.URL, "ghcr.io")

		resolver := &mappedResolver{
			dials: map[string]string{
				parseTarget(t, originURL).Host: serverAddress(t, originServer.URL),
				parseTarget(t, targetURL).Host: serverAddress(t, targetServer.URL),
			},
			registered: map[string]destination.RewriteUpstream{
				"pkg-containers.githubusercontent.com": {
					Profile:    upstream_entity.PackageProfileNone,
					Transports: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
				},
			},
		}
		resp, err := New(Options{Resolver: resolver}).Do(context.Background(), &Request{
			Method: http.MethodGet, Origin: originURL, Path: "/v2/homebrew/core/jq/blobs/sha256:abc",
			Authorization: "Bearer registry-token",
			Requirement: destination.DestinationRequirement{
				Transport:     upstream_entity.ProtocolRegistry,
				AddressPolicy: destination.PublicAddressesOnly,
			},
		})
		if err != nil {
			t.Fatalf("Do() error = %v", err)
		}
		if got := readBody(t, resp); got != "registry-blob" {
			t.Fatalf("body = %q", got)
		}
		if targetHits.Load() != 1 || gotURI != "/ghcr1/blobs/sha256:abc?se=2030-01-01&sig=signed%2Bvalue" {
			t.Fatalf("target hits/URI = %d/%q", targetHits.Load(), gotURI)
		}
		if gotAuthorization != "" || gotCookie != "" {
			t.Fatalf("cross-host credentials = Authorization %q, Cookie %q", gotAuthorization, gotCookie)
		}
		last := resolver.calls[len(resolver.calls)-1].requirement
		if !last.RequireRegistered || last.AddressPolicy != destination.PublicAddressesOnly ||
			last.Transport != upstream_entity.ProtocolStatic || last.Profile != upstream_entity.PackageProfileNone {
			t.Fatalf("registry CDN requirement = %+v", last)
		}
	})

	for name, registered := range map[string]map[string]destination.RewriteUpstream{
		"unregistered CDN": {},
		"registry-only CDN": {
			"pkg-containers.githubusercontent.com": {
				Profile:    upstream_entity.PackageProfileNone,
				Transports: upstream_entity.ProtocolSet{upstream_entity.ProtocolRegistry},
			},
		},
		"profiled static CDN": {
			"pkg-containers.githubusercontent.com": {
				Profile:    upstream_entity.PackageProfileComposer,
				Transports: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
			},
		},
	} {
		t.Run(name+" is rejected for a registry redirect", func(t *testing.T) {
			var targetHits atomic.Int64
			targetServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				targetHits.Add(1)
			}))
			defer targetServer.Close()
			targetURL := mappedURL(t, targetServer.URL, "pkg-containers.githubusercontent.com")
			originServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", targetURL+"/blob?sig=secret")
				w.WriteHeader(http.StatusTemporaryRedirect)
			}))
			defer originServer.Close()
			originURL := mappedURL(t, originServer.URL, "ghcr.io")
			resolver := &mappedResolver{dials: map[string]string{
				parseTarget(t, originURL).Host: serverAddress(t, originServer.URL),
				parseTarget(t, targetURL).Host: serverAddress(t, targetServer.URL),
			}, registered: registered}

			resp, err := New(Options{Resolver: resolver}).Do(context.Background(), &Request{
				Method: http.MethodGet, Origin: originURL, Path: "/v2/o/r/blobs/sha256:abc",
				Requirement: destination.DestinationRequirement{Transport: upstream_entity.ProtocolRegistry},
			})
			if resp != nil || !errors.Is(err, destination.ErrDestinationNotAllowed) {
				t.Fatalf("response/error = %#v/%v", resp, err)
			}
			if targetHits.Load() != 0 {
				t.Fatalf("blocked CDN was dialed %d times", targetHits.Load())
			}
		})
	}

	t.Run("unregistered cross-host redirect is rejected before dial", func(t *testing.T) {
		var targetHits atomic.Int64
		targetServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			targetHits.Add(1)
		}))
		defer targetServer.Close()
		targetURL := mappedURL(t, targetServer.URL, "blocked.example.com")
		originServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, targetURL+"/secret", http.StatusFound)
		}))
		defer originServer.Close()
		originURL := mappedURL(t, originServer.URL, "packages.example.com")
		resolver := &mappedResolver{dials: map[string]string{
			parseTarget(t, originURL).Host: serverAddress(t, originServer.URL),
			parseTarget(t, targetURL).Host: serverAddress(t, targetServer.URL),
		}, registered: map[string]destination.RewriteUpstream{}}

		resp, err := New(Options{Resolver: resolver}).Do(context.Background(), &Request{
			Method: http.MethodGet, Origin: originURL, Path: "/metadata",
			Requirement: destination.DestinationRequirement{Transport: upstream_entity.ProtocolStatic},
		})
		if resp != nil || !errors.Is(err, destination.ErrDestinationNotAllowed) {
			t.Fatalf("response/error = %#v/%v", resp, err)
		}
		if err.Error() != destination.ErrDestinationNotAllowed.Error() {
			t.Fatalf("error leaked destination detail: %q", err)
		}
		if targetHits.Load() != 0 {
			t.Fatalf("blocked target was dialed %d times", targetHits.Load())
		}
	})

	for name, location := range map[string]func(string) string{
		"userinfo": func(target string) string {
			u := parseTarget(t, target)
			u.User = url.UserPassword("user", "secret")
			return u.String()
		},
		"https downgrade": func(target string) string { return target },
	} {
		t.Run(name+" is rejected", func(t *testing.T) {
			var targetHits atomic.Int64
			targetServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				targetHits.Add(1)
			}))
			defer targetServer.Close()
			targetURL := mappedURL(t, targetServer.URL, "cdn.example.com")
			redirectTo := location(targetURL) + "/asset"

			originServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, redirectTo, http.StatusFound)
			}))
			originServer.StartTLS()
			defer originServer.Close()
			originURL := mappedURL(t, originServer.URL, "packages.example.com")
			resolver := &mappedResolver{dials: map[string]string{
				parseTarget(t, originURL).Host: serverAddress(t, originServer.URL),
				parseTarget(t, targetURL).Host: serverAddress(t, targetServer.URL),
			}, registered: map[string]destination.RewriteUpstream{
				"cdn.example.com": {
					Profile:    upstream_entity.PackageProfileNone,
					Transports: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
				},
			}}

			resp, err := New(Options{
				Resolver:        resolver,
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // test server certificate
			}).Do(context.Background(), &Request{
				Method: http.MethodGet, Origin: originURL, Path: "/x",
				Requirement: destination.DestinationRequirement{Transport: upstream_entity.ProtocolRegistry},
			})
			if resp != nil || !errors.Is(err, destination.ErrDestinationNotAllowed) {
				t.Fatalf("response/error = %#v/%v", resp, err)
			}
			if targetHits.Load() != 0 {
				t.Fatalf("unsafe target was dialed %d times", targetHits.Load())
			}
		})
	}
}

func TestCheckRedirectPreservesTransportSemantics(t *testing.T) {
	t.Run("registry cross-host redirect becomes static none and strips credentials", func(t *testing.T) {
		previous := &http.Request{
			Method: http.MethodGet,
			URL:    parseTarget(t, "https://ghcr.io/v2/homebrew/core/jq/blobs/sha256:abc"),
		}
		previous = previous.WithContext(context.WithValue(previous.Context(), requirementKey{},
			destination.DestinationRequirement{
				Transport:     upstream_entity.ProtocolRegistry,
				AddressPolicy: destination.AllowPrivateAddresses,
			}))
		next := &http.Request{
			Method: http.MethodGet,
			URL: parseTarget(t,
				"https://pkg-containers.githubusercontent.com/ghcr1/blob?se=2030-01-01&sig=signed%2Bvalue"),
			Header: http.Header{
				"Authorization":       {"Bearer secret"},
				"Proxy-Authorization": {"Basic secret"},
				"Cookie":              {"session=secret"},
			},
		}

		if err := checkRedirect(next, []*http.Request{previous}); err != nil {
			t.Fatalf("checkRedirect() error = %v", err)
		}
		got, _ := next.Context().Value(requirementKey{}).(destination.DestinationRequirement)
		if !got.RequireRegistered || got.Transport != upstream_entity.ProtocolStatic ||
			got.Profile != upstream_entity.PackageProfileNone || got.AddressPolicy != destination.PublicAddressesOnly {
			t.Fatalf("requirement = %+v", got)
		}
		if next.Header.Get("Authorization") != "" || next.Header.Get("Proxy-Authorization") != "" ||
			next.Header.Get("Cookie") != "" {
			t.Fatalf("credentials were not stripped: %+v", next.Header)
		}
		if next.URL.RawQuery != "se=2030-01-01&sig=signed%2Bvalue" {
			t.Fatalf("signed query = %q", next.URL.RawQuery)
		}
	})

	t.Run("same-host registry redirect keeps registry requirement", func(t *testing.T) {
		previous := &http.Request{Method: http.MethodHead, URL: parseTarget(t, "https://ghcr.io/v2/o/r/blobs/x")}
		previous = previous.WithContext(context.WithValue(previous.Context(), requirementKey{},
			destination.DestinationRequirement{
				Transport:     upstream_entity.ProtocolRegistry,
				AddressPolicy: destination.AllowPrivateAddresses,
			}))
		next := &http.Request{Method: http.MethodHead, URL: parseTarget(t, "https://ghcr.io/v2/o/r/blobs/y")}

		if err := checkRedirect(next, []*http.Request{previous}); err != nil {
			t.Fatalf("checkRedirect() error = %v", err)
		}
		got, _ := next.Context().Value(requirementKey{}).(destination.DestinationRequirement)
		if got.RequireRegistered || got.Transport != upstream_entity.ProtocolRegistry ||
			got.AddressPolicy != destination.AllowPrivateAddresses {
			t.Fatalf("requirement = %+v", got)
		}
	})

	t.Run("static package redirect keeps its profile", func(t *testing.T) {
		previous := &http.Request{Method: http.MethodGet, URL: parseTarget(t, "https://api.github.com/repos/o/r/zipball/x")}
		previous = previous.WithContext(context.WithValue(previous.Context(), requirementKey{},
			destination.DestinationRequirement{
				Transport: upstream_entity.ProtocolStatic,
				Profile:   upstream_entity.PackageProfileComposer,
			}))
		next := &http.Request{Method: http.MethodGet, URL: parseTarget(t, "https://codeload.github.com/o/r/legacy.zip/x")}

		if err := checkRedirect(next, []*http.Request{previous}); err != nil {
			t.Fatalf("checkRedirect() error = %v", err)
		}
		got, _ := next.Context().Value(requirementKey{}).(destination.DestinationRequirement)
		if !got.RequireRegistered || got.Transport != upstream_entity.ProtocolStatic ||
			got.Profile != upstream_entity.PackageProfileComposer ||
			got.AddressPolicy != destination.PublicAddressesOnly {
			t.Fatalf("requirement = %+v", got)
		}
	})
}

func TestDo_PinnedDialBypassesProxyAndKeepsHostAndSNI(t *testing.T) {
	var proxyHits atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		proxyHits.Add(1)
	}))
	defer proxy.Close()
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("https_proxy", proxy.URL)

	var gotHost, gotSNI string
	target := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		gotSNI = r.TLS.ServerName
		_, _ = io.WriteString(w, "ok")
	}))
	target.StartTLS()
	defer target.Close()
	originURL := mappedURL(t, target.URL, "origin.example.com")
	resolver := &mappedResolver{dials: map[string]string{
		parseTarget(t, originURL).Host: serverAddress(t, target.URL),
	}}

	resp, err := New(Options{
		Resolver:        resolver,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // test server certificate
	}).Do(context.Background(), &Request{Method: http.MethodGet, Origin: originURL, Path: "/x"})
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	_ = readBody(t, resp)
	if proxyHits.Load() != 0 {
		t.Fatalf("environment proxy received %d requests", proxyHits.Load())
	}
	if gotHost != parseTarget(t, originURL).Host || gotSNI != "origin.example.com" {
		t.Fatalf("Host/SNI = %q/%q", gotHost, gotSNI)
	}
}

func TestDo_RedirectLoopStopsAfterTen(t *testing.T) {
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "/loop", http.StatusFound)
	}))
	defer server.Close()

	resp, err := New().Do(context.Background(), &Request{
		Method: http.MethodGet, Origin: server.URL, Path: "/loop", Requirement: configuredGenericOrigin,
	})
	if resp != nil || err == nil {
		t.Fatalf("response/error = %#v/%v", resp, err)
	}
	if hits.Load() != 10 {
		t.Fatalf("redirect hits = %d, want 10", hits.Load())
	}
}
