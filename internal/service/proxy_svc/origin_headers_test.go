package proxy_svc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/dispatch"
)

// TestFetch_DropsUpstreamAccountHeaders 上游用来描述**katch 这个调用方**的头不许转出去。
//
// docker.io 会在每个响应上贴 Docker-Ratelimit-Source 与一组 Ratelimit-*：前者是这台
// 镜像站的出口 IP，后者是这台镜像站在上游那边的配额余量。两者说的都不是「客户端拿到的
// 这个对象」，而是「katch 自己的账户状态」，原样转出去等于把出口 IP 和剩余额度播给每
// 一个匿名客户端——而且一旦命中缓存这些值就不再出现，同一个 URL 的响应头于是随缓存
// 状态漂移。
//
// Strict-Transport-Security 与 Alt-Svc 是另一类：它们是源站对**自己那个域名**的传输
// 策略，转发之后会被客户端安到 katch 的域名上。Alt-Svc 尤其危险——它会把客户端指向
// 源站的备用端点。Set-Cookie 同理：镜像站没有会话，源站的 cookie 落到 katch 的域名下
// 只会跟着此后每一次拉取发回来。
//
// Retry-After 相反，要留着：它是上游对「这个对象什么时候再来问」的答复，删掉会让
// 客户端在 429/503 之后立刻重试。
func TestFetch_DropsUpstreamAccountHeaders(t *testing.T) {
	convey.Convey("上游的限流状态、出口 IP、传输策略与 cookie 都到此为止", t, func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			h := w.Header()
			h.Set("Content-Type", "application/json")
			h.Set("Docker-Content-Digest", "sha256:cafe")
			h.Set("Etag", `"sha256:cafe"`)
			h.Set("Retry-After", "120")
			h.Set("Docker-Ratelimit-Source", "203.0.113.7")
			h.Set("Ratelimit-Limit", "100;w=3600")
			h.Set("Ratelimit-Remaining", "99;w=3600")
			h.Set("X-Ratelimit-Limit", "100;w=3600")
			h.Set("X-Ratelimit-Remaining", "99;w=3600")
			h.Set("Strict-Transport-Security", "max-age=31536000")
			h.Set("Alt-Svc", `h3=":443"; ma=2592000`)
			h.Set("Set-Cookie", "session=abc; Path=/")
			_, _ = w.Write([]byte(`{}`))
		}))
		defer srv.Close()

		repo := setupRepo(t)
		repo.EXPECT().List(gomock.Any()).Return([]*upstream_entity.Upstream{
			{ID: 1, Host: "registry.test", Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
				Origin: srv.URL, Enabled: true},
		}, nil).AnyTimes()

		body, meta, err := Proxy().Fetch(context.Background(), &Target{
			Kind: dispatch.KindStatic, Host: "registry.test",
			Path: "/v2/library/redis/manifests/7", Method: http.MethodGet,
		})
		convey.So(err, convey.ShouldBeNil)
		defer func() { _ = body.Close() }()

		for _, name := range []string{
			"Docker-Ratelimit-Source", "Ratelimit-Limit", "Ratelimit-Remaining",
			"X-Ratelimit-Limit", "X-Ratelimit-Remaining",
			"Strict-Transport-Security", "Alt-Svc", "Set-Cookie",
		} {
			convey.So(meta.Header.Get(name), convey.ShouldEqual, "")
		}
		// 描述这一份内容本身的头照常送达，这次摘的不是它们。
		convey.So(meta.Header.Get("Content-Type"), convey.ShouldEqual, "application/json")
		convey.So(meta.Header.Get("Docker-Content-Digest"), convey.ShouldEqual, "sha256:cafe")
		convey.So(meta.Header.Get("Etag"), convey.ShouldEqual, `"sha256:cafe"`)
		convey.So(meta.Header.Get("Retry-After"), convey.ShouldEqual, "120")
	})
}
