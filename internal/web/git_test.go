package web

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/smartystreets/goconvey/convey"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/service/git_svc"
	"github.com/CodFrm/katch/internal/service/proxy_svc"
)

const (
	gitHost        = "git.example.invalid"
	gitRepo        = "/CodFrm/katch"
	advertisePath  = "/" + gitHost + gitRepo + "/info/refs"
	advertiseQuery = "?service=git-upload-pack"
	uploadPackPath = "/" + gitHost + gitRepo + "/git-upload-pack"
)

// 客户端在 upload-pack 这一步发的字节，形态照 smart HTTP 抄。
const wantLine = "0032want d9a1b0c2c3d4e5f60718293a4b5c6d7e8f901234\n0000"

// gitRequest 发一次带请求体与请求头的拉取，装配形态与 request 一致。
func gitRequest(t *testing.T, method, path, body string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.NoRoute(newNoRouteHandlerFS(testDist()))
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	for k, values := range header {
		for _, v := range values {
			req.Header.Add(k, v)
		}
	}
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

// gitUpstream 注册一条开了 git 的上游（同时开 static，这正是 github.com 的形态）。
func gitUpstream(t *testing.T, originURL string, protocols ...string) {
	t.Helper()
	upstreamTable(t, &upstream_entity.Upstream{
		ID: 1, Host: gitHost, Protocols: upstream_entity.ProtocolSet(protocols),
		Origin: originURL, Enabled: true, MutableTTLSeconds: 60,
	})
}

// denyGate 一个一律不放行的退避闸。
type denyGate struct{}

func (denyGate) Allow(string) bool { return false }

// TestGit_UploadPackIsPassedThrough 目标 (a)(b)：clone 的两个端点都不再 405，
// 请求体与 Content-Type / Git-Protocol 原样送达上游，客户端的 Authorization 不外传，
// 响应带 X-Katch-Git: passthrough。
func TestGit_UploadPackIsPassedThrough(t *testing.T) {
	convey.Convey("ref 广播穿透上游", t, func() {
		advertisement := "001e# service=git-upload-pack\n0000"
		var got *http.Request
		var gotURI string
		hits := &atomic.Int64{}
		origin := fakeOrigin(t, func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			got, gotURI = r.Clone(r.Context()), r.URL.RequestURI()
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			_, _ = io.WriteString(w, advertisement)
		})
		gitUpstream(t, origin, upstream_entity.ProtocolStatic, upstream_entity.ProtocolGit)

		header := http.Header{}
		header.Set("Git-Protocol", "version=2")
		header.Set("Authorization", "Basic c2VjcmV0")
		w := gitRequest(t, http.MethodGet, advertisePath+advertiseQuery, "", header)

		convey.So(w.Code, convey.ShouldEqual, http.StatusOK)
		convey.So(hits.Load(), convey.ShouldEqual, 1)
		convey.So(gotURI, convey.ShouldEqual, gitRepo+"/info/refs?service=git-upload-pack")
		convey.So(got.Header.Get("Git-Protocol"), convey.ShouldEqual, "version=2")
		convey.So(got.Header.Get("Authorization"), convey.ShouldBeEmpty)
		convey.So(w.Body.String(), convey.ShouldEqual, advertisement)
		convey.So(w.Header().Get("Content-Type"), convey.ShouldEqual,
			"application/x-git-upload-pack-advertisement")
		convey.So(w.Header().Get("X-Katch-Git"), convey.ShouldEqual, "passthrough")
	})

	convey.Convey("协商的 POST 连同请求体一起穿透上游", t, func() {
		result := "0008NAK\n" + strings.Repeat("PACKDATA", 128)
		var gotBody, gotMethod, gotContentType, gotAuth string
		hits := &atomic.Int64{}
		origin := fakeOrigin(t, func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			b, _ := io.ReadAll(r.Body)
			gotBody, gotMethod = string(b), r.Method
			gotContentType = r.Header.Get("Content-Type")
			gotAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
			_, _ = io.WriteString(w, result)
		})
		gitUpstream(t, origin, upstream_entity.ProtocolStatic, upstream_entity.ProtocolGit)

		header := http.Header{}
		header.Set("Content-Type", "application/x-git-upload-pack-request")
		header.Set("Authorization", "Basic c2VjcmV0")
		w := gitRequest(t, http.MethodPost, uploadPackPath, wantLine, header)

		convey.So(w.Code, convey.ShouldEqual, http.StatusOK)
		convey.So(hits.Load(), convey.ShouldEqual, 1)
		convey.So(gotMethod, convey.ShouldEqual, http.MethodPost)
		convey.So(gotBody, convey.ShouldEqual, wantLine)
		convey.So(gotContentType, convey.ShouldEqual, "application/x-git-upload-pack-request")
		convey.So(gotAuth, convey.ShouldBeEmpty)
		convey.So(w.Body.String(), convey.ShouldEqual, result)
		convey.So(w.Header().Get("X-Katch-Git"), convey.ShouldEqual, "passthrough")
	})
}

// TestGit_OnlyGitEndpointsMayPost 方法闸只对 git 的协商端点放行 POST。
//
// 放行的是一种**路径形态**，不是「这台主机开了 git」：闸装在白名单之前，
// 按主机放行会让 405 与 404 的差别变成一个主机名探测信号。
func TestGit_OnlyGitEndpointsMayPost(t *testing.T) {
	convey.Convey("git 端点之外的 POST 仍然 405", t, func() {
		gitUpstream(t, "http://127.0.0.1:1",
			upstream_entity.ProtocolStatic, upstream_entity.ProtocolGit)
		for _, path := range []string{
			"/" + gitHost + gitRepo + "/releases/download/v1/katch.tar.gz",
			// ref 广播是 GET，POST 到它上面不是一个 git 请求。
			advertisePath + advertiseQuery,
			// 不带 service 的 info/refs 是哑协议的文件请求。
			advertisePath,
		} {
			convey.Convey(path, func() {
				w := gitRequest(t, http.MethodPost, path, "x", nil)
				convey.So(w.Code, convey.ShouldEqual, http.StatusMethodNotAllowed)
			})
		}
	})
}

// TestGit_ReceivePackIsForbidden 目标 (c)：push 的两种形态都返回 403 空体。
//
// 403 而不是 404，且不问上游表：拒绝的是这个**动作**，与这台主机在不在白名单里
// 无关。按主机分别给 403 / 404 才是泄漏——那两个状态码的差别就成了探针。
func TestGit_ReceivePackIsForbidden(t *testing.T) {
	convey.Convey("push 一律 403 且响应体为空", t, func() {
		hits := &atomic.Int64{}
		origin := fakeOrigin(t, func(_ http.ResponseWriter, _ *http.Request) { hits.Add(1) })
		cases := map[string]struct {
			method string
			path   string
		}{
			"push 的 ref 广播": {http.MethodGet,
				"/" + gitHost + gitRepo + "/info/refs?service=git-receive-pack"},
			"push 的协商": {http.MethodPost, "/" + gitHost + gitRepo + "/git-receive-pack"},
			"不在白名单里的主机也是同一个 403": {http.MethodPost,
				"/internal.corp.local/x/git-receive-pack"},
		}
		for name, c := range cases {
			convey.Convey(name+"："+c.path, func() {
				gitUpstream(t, origin,
					upstream_entity.ProtocolStatic, upstream_entity.ProtocolGit)
				w := gitRequest(t, c.method, c.path, "0000", nil)
				convey.So(w.Code, convey.ShouldEqual, http.StatusForbidden)
				convey.So(w.Body.String(), convey.ShouldBeEmpty)
				convey.So(hits.Load(), convey.ShouldEqual, 0)
			})
		}
	})
}

// TestGit_HostWithoutGitProtocolIs404 目标 (d)：没开 git 的主机 404，且不回显主机名。
//
// 它和「表里没有这条记录」必须无从区分——两者都是那个不带主机名的空 404。
func TestGit_HostWithoutGitProtocolIs404(t *testing.T) {
	convey.Convey("协议没开的主机 404 且不带主机名", t, func() {
		hits := &atomic.Int64{}
		origin := fakeOrigin(t, func(_ http.ResponseWriter, _ *http.Request) { hits.Add(1) })
		cases := map[string]struct {
			method string
			path   string
		}{
			"ref 广播":   {http.MethodGet, advertisePath + advertiseQuery},
			"协商的 POST": {http.MethodPost, uploadPackPath},
		}
		for name, c := range cases {
			convey.Convey(name, func() {
				// 只开 static：release 资产照常服务，clone 不服务。
				gitUpstream(t, origin, upstream_entity.ProtocolStatic)
				w := gitRequest(t, c.method, c.path, wantLine, nil)
				convey.So(w.Code, convey.ShouldEqual, http.StatusNotFound)
				convey.So(w.Body.String(), convey.ShouldBeEmpty)
				convey.So(hits.Load(), convey.ShouldEqual, 0)
				for k, values := range w.Header() {
					for _, v := range values {
						convey.So(k+": "+v, convey.ShouldNotContainSubstring, gitHost)
					}
				}
			})
		}
	})
}

// TestGit_PassthroughIsNotCached 目标 (b)：穿透的应答不进对象缓存（决策 9）。
//
// ref 广播是一个不带 Range 的 GET，缓存层原本会把它当成一个普通对象存下来，
// 于是第二个客户端会拿到第一个客户端那一次的广播。判据是「源站被打了两次」：
// 只看响应头上没有 HIT，证不了盘上没有留下副本。
func TestGit_PassthroughIsNotCached(t *testing.T) {
	convey.Convey("穿透的 git 应答不写进缓存", t, func() {
		advertisement := "001e# service=git-upload-pack\n0000"
		hits := &atomic.Int64{}
		origin := fakeOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			_, _ = io.WriteString(w, advertisement)
		})
		gitUpstream(t, origin, upstream_entity.ProtocolStatic, upstream_entity.ProtocolGit)
		withDiskCache(t)

		for range 2 {
			w := gitRequest(t, http.MethodGet, advertisePath+advertiseQuery, "", nil)
			convey.So(w.Code, convey.ShouldEqual, http.StatusOK)
			convey.So(w.Body.String(), convey.ShouldEqual, advertisement)
			convey.So(w.Header().Get("X-Katch-Cache"), convey.ShouldBeEmpty)
			convey.So(w.Header().Get("X-Katch-Git"), convey.ShouldEqual, "passthrough")
		}
		convey.So(hits.Load(), convey.ShouldEqual, 2)
	})
}

// TestGit_BackoffFailsFast 目标 (e)：上游在退避窗口里时，穿透直接 502 且不打上游。
//
// 穿透不是一条绕过闸的新通路：它和别的拉取走同一个 proxy_svc，退避照样先问。
func TestGit_BackoffFailsFast(t *testing.T) {
	convey.Convey("退避窗口里的上游，git 穿透直接 502", t, func() {
		hits := &atomic.Int64{}
		origin := fakeOrigin(t, func(_ http.ResponseWriter, _ *http.Request) { hits.Add(1) })
		gitUpstream(t, origin, upstream_entity.ProtocolStatic, upstream_entity.ProtocolGit)
		prev := proxy_svc.Proxy()
		proxy_svc.Register(proxy_svc.New(proxy_svc.Options{Gate: denyGate{}}))
		t.Cleanup(func() { proxy_svc.Register(prev) })

		for _, c := range []struct {
			method string
			path   string
		}{
			{http.MethodGet, advertisePath + advertiseQuery},
			{http.MethodPost, uploadPackPath},
		} {
			w := gitRequest(t, c.method, c.path, wantLine, nil)
			convey.So(w.Code, convey.ShouldEqual, http.StatusBadGateway)
			convey.So(w.Body.String(), convey.ShouldNotContainSubstring, gitHost)
		}
		convey.So(hits.Load(), convey.ShouldEqual, 0)
	})
}

// fakeMirror 顶替镜像层：拉取路径这一层要验的是「本地答得了就别穿透」，
// 镜像怎么建、新鲜不新鲜由 git_svc 自己的用例验。
type fakeMirror struct {
	mu sync.Mutex
	// answer 这次给不给本地应答，nil 表示让调用方穿透。
	answer func(req *git_svc.AnswerRequest) *git_svc.Answer
	reqs   []git_svc.AnswerRequest
}

func (f *fakeMirror) Ensure(context.Context, string, string) {}

func (f *fakeMirror) Quiesce(context.Context) error { return nil }

func (f *fakeMirror) InUse(string, string) bool { return false }

func (f *fakeMirror) Answer(_ context.Context, req *git_svc.AnswerRequest) *git_svc.Answer {
	f.mu.Lock()
	f.reqs = append(f.reqs, *req)
	answer := f.answer
	f.mu.Unlock()
	if answer == nil {
		return nil
	}
	return answer(req)
}

func (f *fakeMirror) lastRequest() git_svc.AnswerRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		return git_svc.AnswerRequest{}
	}
	return f.reqs[len(f.reqs)-1]
}

// withMirror 把镜像层换成用例的替身。
func withMirror(t *testing.T, mirror git_svc.MirrorSvc) {
	t.Helper()
	before := git_svc.Mirror()
	git_svc.Register(mirror)
	t.Cleanup(func() { git_svc.Register(before) })
}

// countingBody 一份记得自己有没有被关掉的响应体。
type countingBody struct {
	io.Reader
	closed *atomic.Int64
}

func (b *countingBody) Close() error {
	b.closed.Add(1)
	return nil
}

// TestGit_LocalMirrorAnswers 目标 (a)(b)：镜像答得了的时候由本地答，
// 响应带 X-Katch-Git: local，上游一次都不被碰。
func TestGit_LocalMirrorAnswers(t *testing.T) {
	convey.Convey("本地镜像答得了时不穿透", t, func() {
		hits := &atomic.Int64{}
		origin := fakeOrigin(t, func(_ http.ResponseWriter, _ *http.Request) { hits.Add(1) })
		gitUpstream(t, origin, upstream_entity.ProtocolStatic, upstream_entity.ProtocolGit)
		closed := &atomic.Int64{}

		convey.Convey("ref 广播", func() {
			advertisement := "001e# service=git-upload-pack\n0000refs"
			mirror := &fakeMirror{answer: func(*git_svc.AnswerRequest) *git_svc.Answer {
				return &git_svc.Answer{
					ContentType: "application/x-git-upload-pack-advertisement",
					Body: &countingBody{
						Reader: strings.NewReader(advertisement), closed: closed,
					},
				}
			}}
			withMirror(t, mirror)

			w := gitRequest(t, http.MethodGet, advertisePath+advertiseQuery, "", nil)

			convey.So(w.Code, convey.ShouldEqual, http.StatusOK)
			convey.So(w.Body.String(), convey.ShouldEqual, advertisement)
			convey.So(w.Header().Get("Content-Type"), convey.ShouldEqual,
				"application/x-git-upload-pack-advertisement")
			convey.So(w.Header().Get("X-Katch-Git"), convey.ShouldEqual, "local")
			// 本地应答不是缓存命中：两个头各自只回答一件事。
			convey.So(w.Header().Get("X-Katch-Cache"), convey.ShouldBeEmpty)
			convey.So(hits.Load(), convey.ShouldEqual, 0)

			convey.Convey("问的是这台主机下的这个仓库，且是广播这一半", func() {
				convey.So(mirror.lastRequest().Host, convey.ShouldEqual, gitHost)
				convey.So(mirror.lastRequest().Repo, convey.ShouldEqual, gitRepo)
				convey.So(mirror.lastRequest().Advertise, convey.ShouldBeTrue)
			})
			convey.Convey("应答体被关掉：镜像上的读占用要跟着放掉", func() {
				convey.So(closed.Load(), convey.ShouldEqual, 1)
			})
		})

		convey.Convey("协商的 POST：请求体原样交给镜像", func() {
			result := "0008NAK\nPACKDATA"
			mirror := &fakeMirror{answer: func(*git_svc.AnswerRequest) *git_svc.Answer {
				return &git_svc.Answer{
					ContentType: "application/x-git-upload-pack-result",
					Body:        &countingBody{Reader: strings.NewReader(result), closed: closed},
				}
			}}
			withMirror(t, mirror)

			w := gitRequest(t, http.MethodPost, uploadPackPath, wantLine, nil)

			convey.So(w.Code, convey.ShouldEqual, http.StatusOK)
			convey.So(w.Body.String(), convey.ShouldEqual, result)
			convey.So(w.Header().Get("Content-Type"), convey.ShouldEqual,
				"application/x-git-upload-pack-result")
			convey.So(w.Header().Get("X-Katch-Git"), convey.ShouldEqual, "local")
			convey.So(string(mirror.lastRequest().Body), convey.ShouldEqual, wantLine)
			convey.So(mirror.lastRequest().Advertise, convey.ShouldBeFalse)
			convey.So(hits.Load(), convey.ShouldEqual, 0)
		})
	})
}

// TestGit_DeclinedLocalAnswerStillPassesTheBodyThrough 目标 (d)：镜像答不了的请求
// （带 shallow 或 --filter 的那些）照常穿透，**请求体一个字节都不能少**。
//
// 这是本地应答这条路上最容易悄悄坏掉的地方：要判断答不答得了就得先把请求体读掉，
// 读完了不还回去，上游收到的就是一个空请求。
func TestGit_DeclinedLocalAnswerStillPassesTheBodyThrough(t *testing.T) {
	convey.Convey("镜像不接这一次时，请求体原样送到上游", t, func() {
		var gotBody, gotContentType string
		hits := &atomic.Int64{}
		origin := fakeOrigin(t, func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			gotContentType = r.Header.Get("Content-Type")
			w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
			_, _ = io.WriteString(w, "0008NAK\n")
		})
		gitUpstream(t, origin, upstream_entity.ProtocolStatic, upstream_entity.ProtocolGit)
		// answer 为 nil：镜像这一次不接。
		mirror := &fakeMirror{}
		withMirror(t, mirror)

		header := http.Header{}
		header.Set("Content-Type", "application/x-git-upload-pack-request")
		deepen := wantLine[:len(wantLine)-4] + "000cdeepen 1\n0000"
		w := gitRequest(t, http.MethodPost, uploadPackPath, deepen, header)

		convey.So(w.Code, convey.ShouldEqual, http.StatusOK)
		convey.So(hits.Load(), convey.ShouldEqual, 1)
		convey.So(gotBody, convey.ShouldEqual, deepen)
		convey.So(gotContentType, convey.ShouldEqual, "application/x-git-upload-pack-request")
		convey.So(w.Header().Get("X-Katch-Git"), convey.ShouldEqual, "passthrough")
		convey.So(string(mirror.lastRequest().Body), convey.ShouldEqual, deepen)
	})
}

// TestGit_LocalAnswerIsNotOfferedForPush push 在本地应答这条路上同样不存在：
// 403 那道闸在前面，镜像层根本不该被问到。
func TestGit_LocalAnswerIsNotOfferedForPush(t *testing.T) {
	convey.Convey("push 不会走到镜像层", t, func() {
		gitUpstream(t, "http://127.0.0.1:1",
			upstream_entity.ProtocolStatic, upstream_entity.ProtocolGit)
		mirror := &fakeMirror{answer: func(*git_svc.AnswerRequest) *git_svc.Answer {
			return &git_svc.Answer{ContentType: "x", Body: io.NopCloser(strings.NewReader("x"))}
		}}
		withMirror(t, mirror)

		w := gitRequest(t, http.MethodPost, "/"+gitHost+gitRepo+"/git-receive-pack", "0000", nil)
		convey.So(w.Code, convey.ShouldEqual, http.StatusForbidden)
		convey.So(mirror.lastRequest().Host, convey.ShouldBeEmpty)
	})
}

// gzipBody 把一段请求体压成 gzip：git 对超过 1KB 的协商请求就是这么发的。
func gzipBody(t *testing.T, plain string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(plain)); err != nil {
		t.Fatalf("压不了请求体：%v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("收不了 gzip 流：%v", err)
	}
	return buf.String()
}

// TestGit_GzippedNegotiationRequest git 会把超过 1KB 的协商请求压成 gzip 再发
// （Content-Encoding: gzip），而 ref 多的仓库正好落在这条线上。
//
// 不解开的话两头都坏：
//   - 回源头是白名单，Content-Encoding 不在里面，上游收到的是「声明明文、实为
//     gzip」的字节，按 pkt-line 解出一堆乱码，回一个 400。现象是 226 个 ref 的
//     仓库 clone 失败，而小仓库照常。
//   - 本地镜像也读不懂它，于是最该命中镜像的那一类请求（ref 多到要压缩）全是穿透。
//
// 契约是在入口解开：镜像拿到明文，穿透时上游拿到的也是明文与对应的 Content-Length，
// 而且不再带 Content-Encoding——声明与字节必须一致，带这个头去送明文，上游会去解
// 一份根本没压过的数据。
func TestGit_GzippedNegotiationRequest(t *testing.T) {
	convey.Convey("gzip 的协商请求在入口解开", t, func() {
		convey.Convey("穿透时上游收到的是明文", func() {
			var gotBody, gotEncoding string
			var gotLength int64
			origin := fakeOrigin(t, func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				gotBody = string(b)
				gotLength = r.ContentLength
				gotEncoding = r.Header.Get("Content-Encoding")
				w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
				_, _ = io.WriteString(w, "0008NAK\n")
			})
			gitUpstream(t, origin, upstream_entity.ProtocolStatic, upstream_entity.ProtocolGit)
			// answer 为 nil：镜像这一次不接，走穿透。
			withMirror(t, &fakeMirror{})

			header := http.Header{}
			header.Set("Content-Type", "application/x-git-upload-pack-request")
			header.Set("Content-Encoding", "gzip")
			w := gitRequest(t, http.MethodPost, uploadPackPath, gzipBody(t, wantLine), header)

			convey.So(w.Code, convey.ShouldEqual, http.StatusOK)
			convey.So(gotBody, convey.ShouldEqual, wantLine)
			convey.So(gotLength, convey.ShouldEqual, int64(len(wantLine)))
			convey.So(gotEncoding, convey.ShouldBeEmpty)
		})

		convey.Convey("本地镜像拿到的是明文，因此照常命中", func() {
			result := "0008NAK\nPACKDATA"
			hits := &atomic.Int64{}
			origin := fakeOrigin(t, func(_ http.ResponseWriter, _ *http.Request) { hits.Add(1) })
			gitUpstream(t, origin, upstream_entity.ProtocolStatic, upstream_entity.ProtocolGit)
			mirror := &fakeMirror{answer: func(*git_svc.AnswerRequest) *git_svc.Answer {
				return &git_svc.Answer{
					ContentType: "application/x-git-upload-pack-result",
					Body:        io.NopCloser(strings.NewReader(result)),
				}
			}}
			withMirror(t, mirror)

			header := http.Header{}
			header.Set("Content-Type", "application/x-git-upload-pack-request")
			header.Set("Content-Encoding", "gzip")
			w := gitRequest(t, http.MethodPost, uploadPackPath, gzipBody(t, wantLine), header)

			convey.So(w.Code, convey.ShouldEqual, http.StatusOK)
			convey.So(string(mirror.lastRequest().Body), convey.ShouldEqual, wantLine)
			convey.So(w.Header().Get("X-Katch-Git"), convey.ShouldEqual, "local")
			convey.So(hits.Load(), convey.ShouldEqual, 0)
		})
	})
}
