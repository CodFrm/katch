package proxy_svc

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"

	"github.com/CodFrm/katch/internal/metrics"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/dispatch"
	"github.com/CodFrm/katch/internal/service/setting_svc"
)

// 回源侧那三族指标（可观测性一节）量的是 katch 与上游之间那一跳，拉取路径最外层
// 那个中间件按响应反推不出来：一个 200 既可能是命中、也可能是重试了三次才成。
// 所以它们由这一层喂，用例也装在这一层。

// fixedRuntime 一份固定的运行时设置，只为了把重试次数钉死——默认值会重试两次，
// 那样一次连不上的回源会记成三条，验不出「一次回源记一条」。
type fixedRuntime int

func (f fixedRuntime) Runtime(_ context.Context) (*setting_svc.RuntimeSettings, error) {
	return &setting_svc.RuntimeSettings{
		OriginConcurrency:    32,
		OriginTimeoutSeconds: 30,
		OriginRetries:        int(f),
	}, nil
}

func scrapeRegistry(reg *prometheus.Registry) string {
	w := httptest.NewRecorder()
	promhttp.HandlerFor(reg, promhttp.HandlerOpts{}).
		ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return w.Body.String()
}

// TestFetch_RecordsOriginMetrics 一次回源要在 /metrics 上留下请求数、耗时与在途数。
func TestFetch_RecordsOriginMetrics(t *testing.T) {
	convey.Convey("一次回源记下 origin 的请求数、耗时与在途数", t, func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "payload")
		}))
		defer srv.Close()

		repo := setupRepo(t)
		repo.EXPECT().List(gomock.Any()).Return([]*upstream_entity.Upstream{
			{ID: 1, Host: "deb.debian.org", Kind: upstream_entity.KindStatic, Origin: srv.URL, Enabled: true},
		}, nil).AnyTimes()

		reg := prometheus.NewRegistry()
		svc := New(Options{Metrics: metrics.New(metrics.Options{Registerer: reg})})

		body, _, err := svc.Fetch(context.Background(), &Target{
			Kind: dispatch.KindStatic, Host: "deb.debian.org",
			Path: "/dists/stable/InRelease", Method: http.MethodGet,
		})
		convey.So(err, convey.ShouldBeNil)

		convey.Convey("响应体还没关时，这次回源仍算在途", func() {
			convey.So(scrapeRegistry(reg), convey.ShouldContainSubstring,
				`katch_origin_inflight{upstream="deb.debian.org"} 1`)
		})

		_, _ = io.ReadAll(body)
		_ = body.Close()

		got := scrapeRegistry(reg)
		convey.So(got, convey.ShouldContainSubstring,
			`katch_origin_requests_total{status="200",upstream="deb.debian.org"} 1`)
		convey.So(got, convey.ShouldContainSubstring,
			`katch_origin_duration_seconds_count{upstream="deb.debian.org"} 1`)
		// 名额与在途数都挂在响应体的生命周期上，关掉就该回到零。
		convey.So(got, convey.ShouldContainSubstring,
			`katch_origin_inflight{upstream="deb.debian.org"} 0`)
	})
}

// TestFetch_RecordsUnreachableOriginAsStatusZero 连不上的回源也是一次真实发生的回源。
//
// 不记的话「回源了多少次」会在上游挂掉时凭空变小，正好在最需要这个数的时候。
func TestFetch_RecordsUnreachableOriginAsStatusZero(t *testing.T) {
	convey.Convey("拿不到响应的回源按 status=0 记一次", t, func() {
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
		origin := srv.URL
		srv.Close() // 关掉再用：这个地址此刻一定连不上。

		repo := setupRepo(t)
		repo.EXPECT().List(gomock.Any()).Return([]*upstream_entity.Upstream{
			{ID: 1, Host: "deb.debian.org", Kind: upstream_entity.KindStatic, Origin: origin, Enabled: true},
		}, nil).AnyTimes()

		reg := prometheus.NewRegistry()
		svc := New(Options{
			Metrics: metrics.New(metrics.Options{Registerer: reg}),
			Runtime: fixedRuntime(0),
		})

		_, _, err := svc.Fetch(context.Background(), &Target{
			Kind: dispatch.KindStatic, Host: "deb.debian.org",
			Path: "/dists/stable/InRelease", Method: http.MethodGet,
		})
		convey.So(err, convey.ShouldNotBeNil)

		got := scrapeRegistry(reg)
		convey.So(got, convey.ShouldContainSubstring,
			`katch_origin_requests_total{status="0",upstream="deb.debian.org"} 1`)
		// 失败的回源同样要把在途数还回去，否则一次上游故障会让这个 gauge 永远漂着。
		convey.So(got, convey.ShouldContainSubstring,
			`katch_origin_inflight{upstream="deb.debian.org"} 0`)
	})
}
