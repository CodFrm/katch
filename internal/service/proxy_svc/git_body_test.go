package proxy_svc_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/dispatch"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
	mock_upstream_repo "github.com/CodFrm/katch/internal/repository/upstream_repo/mock"
	"github.com/CodFrm/katch/internal/service/proxy_svc"
)

// registerGitUpstream 注册一条同时开了 static 与 git 的上游，装配形态与 main 一致。
func registerGitUpstream(t *testing.T, host, originURL string) {
	t.Helper()
	repo := mock_upstream_repo.NewMockUpstreamRepo(gomock.NewController(t))
	repo.EXPECT().List(gomock.Any()).Return([]*upstream_entity.Upstream{{
		ID: 9, Host: host, Origin: originURL, Enabled: true,
		Protocols: upstream_entity.ProtocolSet{
			upstream_entity.ProtocolStatic, upstream_entity.ProtocolGit,
		},
		MutableTTLSeconds: 60,
	}}, nil).AnyTimes()
	upstream_repo.RegisterUpstream(proxy_svc.NewCachedUpstreamRepo(repo))
}

// brokenOrigin 一个记数之后直接掐断连接的假源站：每一次尝试都会让回源拿不到响应。
func brokenOrigin(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	hits := &atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, hits
}

// TestFetch_RequestBodyIsNotRetried 带请求体的回源只打一次上游。
//
// 请求体是一次性的 io.Reader，读完就没了。照常重试的话，第二次尝试会给上游送
// 一个**空的** upload-pack 请求，而上游会拿它当一次合法的空协商正常答复——
// 客户端于是收到一份和它的 want 无关的响应，比直接失败难查得多。
func TestFetch_RequestBodyIsNotRetried(t *testing.T) {
	convey.Convey("回源失败时，没有请求体的照常重试", t, func() {
		originURL, hits := brokenOrigin(t)
		registerGitUpstream(t, "git.example.invalid", originURL)
		svc := proxy_svc.New(proxy_svc.Options{})

		_, _, err := svc.Fetch(context.Background(), &proxy_svc.Target{
			Kind: dispatch.KindStatic, Host: "git.example.invalid",
			Path: "/CodFrm/katch/info/refs", RawQuery: "service=git-upload-pack",
			Method: http.MethodGet, Header: http.Header{},
			Git: dispatch.GitEndpoint{
				Service: dispatch.GitUploadPack, Advertise: true, Repo: "/CodFrm/katch",
			},
		})
		convey.So(err, convey.ShouldNotBeNil)
		convey.So(hits.Load(), convey.ShouldBeGreaterThan, 1)
	})

	convey.Convey("回源失败时，带请求体的不重试", t, func() {
		originURL, hits := brokenOrigin(t)
		registerGitUpstream(t, "git.example.invalid", originURL)
		svc := proxy_svc.New(proxy_svc.Options{})

		payload := "0032want d9a1b0c2c3d4e5f60718293a4b5c6d7e8f901234\n0000"
		_, _, err := svc.Fetch(context.Background(), &proxy_svc.Target{
			Kind: dispatch.KindStatic, Host: "git.example.invalid",
			Path: "/CodFrm/katch/git-upload-pack", Method: http.MethodPost,
			Header: http.Header{}, Body: strings.NewReader(payload),
			ContentLength: int64(len(payload)),
			Git: dispatch.GitEndpoint{
				Service: dispatch.GitUploadPack, Repo: "/CodFrm/katch",
			},
		})
		convey.So(err, convey.ShouldNotBeNil)
		convey.So(hits.Load(), convey.ShouldEqual, 1)
	})
}
