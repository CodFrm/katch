package proxy_svc_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/smartystreets/goconvey/convey"

	"github.com/CodFrm/katch/internal/proxy/dispatch"
	"github.com/CodFrm/katch/internal/service/git_svc"
	"github.com/CodFrm/katch/internal/service/proxy_svc"
)

// fakeMirror 记下穿透路径有没有让镜像层开工。
type fakeMirror struct {
	mu   sync.Mutex
	seen [][2]string
}

func (f *fakeMirror) Ensure(_ context.Context, host, repo string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, [2]string{host, repo})
}

func (f *fakeMirror) Quiesce(context.Context) error { return nil }

func (f *fakeMirror) calls() [][2]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][2]string(nil), f.seen...)
}

// captureMirror 把镜像层换成一个记账的假的，用例结束换回去。
func captureMirror(t *testing.T) *fakeMirror {
	t.Helper()
	f := &fakeMirror{}
	before := git_svc.Mirror()
	git_svc.Register(f)
	t.Cleanup(func() { git_svc.Register(before) })
	return f
}

// statusOrigin 一个按给定状态码作答的假源站。
func statusOrigin(t *testing.T, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte("001e# service=git-upload-pack\n"))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestFetch_TriggersMirrorAfterPassthrough 一次成功的穿透拉取要让后台开始建镜像。
//
// 触发点在穿透这一侧而不是在别处：只有走到这里，才同时知道「这是一个 git 仓库
// 的只读请求」「这台上游在白名单里且开着 git」「上游确实认这个仓库」三件事。
func TestFetch_TriggersMirrorAfterPassthrough(t *testing.T) {
	convey.Convey("git 穿透成功之后", t, func() {
		const host = "git.example.invalid"
		mirror := captureMirror(t)
		registerGitUpstream(t, host, statusOrigin(t, http.StatusOK))
		svc := proxy_svc.New(proxy_svc.Options{})

		body, meta, err := svc.Fetch(context.Background(), &proxy_svc.Target{
			Kind: dispatch.KindStatic, Host: host, Path: "/foo/bar.git/info/refs",
			RawQuery: "service=git-upload-pack", Method: http.MethodGet,
			Header: http.Header{},
			Git: dispatch.GitEndpoint{
				Service: dispatch.GitUploadPack, Advertise: true, Repo: "/foo/bar.git",
			},
		})
		convey.So(err, convey.ShouldBeNil)
		convey.So(meta.StatusCode, convey.ShouldEqual, http.StatusOK)
		_, _ = io.Copy(io.Discard, body)
		convey.So(body.Close(), convey.ShouldBeNil)

		convey.Convey("镜像层收到的是这台主机和这个仓库", func() {
			convey.So(mirror.calls(), convey.ShouldResemble, [][2]string{{host, "/foo/bar.git"}})
		})
	})
}

// TestFetch_DoesNotMirrorWhatUpstreamRefused 上游不认这个仓库时不登记镜像。
func TestFetch_DoesNotMirrorWhatUpstreamRefused(t *testing.T) {
	convey.Convey("上游对这个仓库答 404 时", t, func() {
		const host = "git.example.invalid"
		mirror := captureMirror(t)
		registerGitUpstream(t, host, statusOrigin(t, http.StatusNotFound))
		svc := proxy_svc.New(proxy_svc.Options{})

		body, meta, err := svc.Fetch(context.Background(), &proxy_svc.Target{
			Kind: dispatch.KindStatic, Host: host, Path: "/nope.git/info/refs",
			RawQuery: "service=git-upload-pack", Method: http.MethodGet,
			Header: http.Header{},
			Git: dispatch.GitEndpoint{
				Service: dispatch.GitUploadPack, Advertise: true, Repo: "/nope.git",
			},
		})
		convey.So(err, convey.ShouldBeNil)
		convey.So(meta.StatusCode, convey.ShouldEqual, http.StatusNotFound)
		_, _ = io.Copy(io.Discard, body)
		convey.So(body.Close(), convey.ShouldBeNil)

		convey.Convey("不给一个上游自己都没有的仓库建镜像", func() {
			convey.So(mirror.calls(), convey.ShouldBeEmpty)
		})
	})
}

// TestFetch_DoesNotMirrorPlainStatic 普通静态拉取和镜像无关。
func TestFetch_DoesNotMirrorPlainStatic(t *testing.T) {
	convey.Convey("不是 git 端点的拉取", t, func() {
		const host = "git.example.invalid"
		mirror := captureMirror(t)
		registerGitUpstream(t, host, statusOrigin(t, http.StatusOK))
		svc := proxy_svc.New(proxy_svc.Options{})

		body, _, err := svc.Fetch(context.Background(), &proxy_svc.Target{
			Kind: dispatch.KindStatic, Host: host, Path: "/releases/v1.0.0.tar.gz",
			Method: http.MethodGet, Header: http.Header{},
		})
		convey.So(err, convey.ShouldBeNil)
		_, _ = io.Copy(io.Discard, body)
		convey.So(body.Close(), convey.ShouldBeNil)

		convey.So(mirror.calls(), convey.ShouldBeEmpty)
	})
}
