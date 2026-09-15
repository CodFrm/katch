package origin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/smartystreets/goconvey/convey"
)

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
			Path: "/dists/stable/InRelease", RawQuery: "x=1",
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

// TestDo_FollowsRedirect
//
// GitHub 的 release 资产会 302 到另一台主机。把重定向透给客户端，客户端就要去
// 直连一个不在上游表里、且在受限网络下根本不通的地址——代理白做了。
func TestDo_FollowsRedirect(t *testing.T) {
	convey.Convey("302 由 katch 自己跟随", t, func() {
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
			Path: "/pool/main/g/g%2Bb/x%7E1.deb",
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
			Method: http.MethodGet, Origin: srv.URL, Path: "/x",
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
			Method: http.MethodGet, Origin: srv.URL, Path: "/x",
		})
		convey.So(err, convey.ShouldNotBeNil)
	})
}

// TestDo_ForwardsRequestBodyAndGitHeaders
//
// 决策 10：穿透 git 的协商请求要把请求体原样送到上游，而 Content-Type 与
// Git-Protocol 必须跟着走——前者是 upload-pack 请求的载体，缺了后者协议会退回
// v0。白名单仍然是白名单：这里只多了这两项，客户端的 Authorization 照旧不外传。
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
		clientHeader.Set("Authorization", "Basic c2VjcmV0")

		resp, err := New().Do(context.Background(), &Request{
			Method: http.MethodPost, Origin: srv.URL,
			Path: "/CodFrm/katch/git-upload-pack", Header: clientHeader,
			Body: strings.NewReader(payload), ContentLength: int64(len(payload)),
		})
		convey.So(err, convey.ShouldBeNil)
		convey.So(gotMethod, convey.ShouldEqual, http.MethodPost)
		convey.So(gotBody, convey.ShouldEqual, payload)
		convey.So(gotHeader.Get("Content-Type"), convey.ShouldEqual, "application/x-git-upload-pack-request")
		convey.So(gotHeader.Get("Git-Protocol"), convey.ShouldEqual, "version=2")
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
		})
		convey.So(err, convey.ShouldBeNil)
		convey.So(gotLength, convey.ShouldEqual, 0)
		convey.So(gotEncoding, convey.ShouldBeEmpty)
	})
}
