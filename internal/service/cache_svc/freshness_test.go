package cache_svc

import (
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// TestGet_MutableTTLCapsResidenceNotTheAgeTheObjectArrivedWith 可变 TTL 封的是对象在 katch
// 里的驻留时间，不是它在上游 CDN 那儿已经攒下的年龄。
//
// 以前的算法是 min(TTL, 源站寿命) − 源站 Age：CDN 报的 Age 一旦超过 TTL，对象刚存进来
// 就已经过期。真机上撞上的两处都是这样——index.crates.io 的 /config.json 没有任何寿命
// 指令、只带 Age: 1420；sum.golang.org 的部分 tile 是 max-age=10800、Age 几千秒——于是
// 每一次都回源，经 CDN 的可变元数据在镜像上基本命中不了。
//
// 现在是 min(TTL, 源站寿命 − 源站 Age)。spec 对本地新鲜度的三条约束都还在：不超过 TTL，
// 不超过源站剩下的新鲜期（源站说已经过期的照样立刻过期），命中也不会重启源站的寿命
// ——对外的 Age 仍是源站 Age 加上本地驻留时间。
func TestGet_MutableTTLCapsResidenceNotTheAgeTheObjectArrivedWith(t *testing.T) {
	const ttl = 300
	now := time.Date(2026, 9, 21, 3, 0, 0, 0, time.UTC)
	cases := []struct {
		name         string
		cacheControl string
		age          int64
		want         int64
	}{
		// index.crates.io/config.json 的形状：没有寿命指令，只有 CDN 的 Age。
		{name: "源站不给寿命、CDN 放了很久", age: 1420, want: ttl},
		// 源站剩下的比 TTL 短：以源站为准。
		{name: "源站剩下的新鲜期短于 TTL", cacheControl: "public, max-age=600", age: 450, want: 150},
		// 源站剩下的比 TTL 长：以 TTL 为准。以前这里会被扣成 200。
		{name: "源站剩下的新鲜期长于 TTL", cacheControl: "public, max-age=600", age: 100, want: ttl},
		// sum.golang.org 部分 tile 的形状。
		{name: "CDN 放了几千秒但源站还新鲜", cacheControl: "public, max-age=10800", age: 3072, want: ttl},
		// 源站自己说已经过期的，本地一秒也不多给。
		{name: "源站已经过期", cacheControl: "public, max-age=600", age: 700, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
				// Date 与 katch 的时钟对齐，让 Age 头成为唯一的年龄来源。
				w.Header().Set("Date", now.Format(http.TimeFormat))
				w.Header().Set("Age", strconv.FormatInt(tc.age, 10))
				if tc.cacheControl != "" {
					w.Header().Set("Cache-Control", tc.cacheControl)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"dl":"https://static.crates.io/crates"}`)
			})
			up := staticUpstream("index.crates.io")
			up.MutableTTLSeconds = ttl
			svc, repo, _ := setupSvc(t, o, up, Options{Now: func() time.Time { return now }})

			pullWith(t, svc, target("index.crates.io", "/config.json"))
			row := repo.byKey("/config.json")
			if row == nil {
				t.Fatal("可变对象没有落库")
			}
			if row.Immutable {
				t.Fatal("/config.json 被当成了不可变对象")
			}
			if got := row.ExpiresAt - now.Unix(); got != tc.want {
				t.Fatalf("本地新鲜期 = %d 秒，要的是 %d 秒（TTL %d，max-age %q，Age %d）",
					got, tc.want, ttl, tc.cacheControl, tc.age)
			}
		})
	}
}
