package cache_svc

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/smartystreets/goconvey/convey"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/dispatch"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
	"github.com/CodFrm/katch/internal/service/proxy_svc"
)

// registry 的两套 manifest 媒体类型：docker 的客户端（docker pull）与 OCI 的客户端
// （podman、crane）对同一个 tag 要的是**不同的**两份内容，靠 Accept 告诉上游。
const (
	dockerManifestType = "application/vnd.docker.distribution.manifest.v2+json"
	dockerIndexType    = "application/vnd.docker.distribution.manifest.list.v2+json"
	ociManifestType    = "application/vnd.oci.image.manifest.v1+json"
	ociIndexType       = "application/vnd.oci.image.index.v1+json"
)

func registryUpstream(host string) *upstream_entity.Upstream {
	return &upstream_entity.Upstream{
		ID: 9, Host: host, Protocols: upstream_entity.ProtocolSet{upstream_entity.ProtocolRegistry},
		ImmutablePatterns: upstream_entity.PatternList{"/blobs/sha256:"},
		MutableTTLSeconds: 600,
	}
}

// registryTarget 一次 registry 侧的拉取。accept 按客户端真实的样子给，可以多行。
func registryTarget(host, path string, accept ...string) *proxy_svc.Target {
	header := http.Header{}
	for _, a := range accept {
		header.Add("Accept", a)
	}
	return &proxy_svc.Target{
		Kind: dispatch.KindRegistry, Host: host, Path: path,
		Method: http.MethodGet, Header: header,
	}
}

// manifestOrigin 假 registry：按 Accept 选一份 manifest 发回去，正如真实上游那样。
func manifestOrigin(t *testing.T) *originStub {
	t.Helper()
	return newOrigin(t, func(w http.ResponseWriter, r *http.Request) {
		mediaType, body := dockerManifestType, `{"schemaVersion":2,"flavour":"docker"}`
		if strings.Contains(r.Header.Get("Accept"), "vnd.oci.") {
			mediaType, body = ociManifestType, `{"schemaVersion":2,"flavour":"oci"}`
		}
		w.Header().Set("Content-Type", mediaType)
		_, _ = io.WriteString(w, body)
	})
}

func TestCacheKey_TransformedProfileIncludesGenerationAndDeclaredVariants(t *testing.T) {
	convey.Convey("transformed metadata identity is generation-aware and normalized", t, func() {
		tg := target("registry.example.com", "/pkg")
		tg.RawQuery = "view=full"
		tg.Header.Add("Accept", " Application/JSON ; q=1.0, text/html;q=0.50")
		tg.Header.Set("User-Agent", "ignored")
		representation := packageprofile.Representation{
			Class: packageprofile.ClassMutable, Transform: true, Variants: []string{"Accept"},
		}

		first := cacheKeyForRepresentation(tg, representation, 41)
		equivalent := target("registry.example.com", "/pkg")
		equivalent.RawQuery = "view=full"
		equivalent.Header.Add("Accept", "text/html;q=0.5,application/json")
		convey.So(cacheKeyForRepresentation(equivalent, representation, 41), convey.ShouldEqual, first)
		convey.So(cacheKeyForRepresentation(equivalent, representation, 42), convey.ShouldNotEqual, first)
		convey.So(first, convey.ShouldContainSubstring, variantMarker+"generation=41")

		undeclared := target("registry.example.com", "/pkg")
		undeclared.RawQuery = "view=full"
		undeclared.Header.Set("User-Agent", "different")
		convey.So(cacheKeyForRepresentation(undeclared, representation, 41), convey.ShouldNotEqual, first)
	})
}

func TestCacheKey_TransparentRepresentationKeepsLegacyIdentity(t *testing.T) {
	tg := target("files.example.com", "/artifact.tgz")
	representation := packageprofile.Representation{Class: packageprofile.ClassImmutable}
	if got := cacheKeyForRepresentation(tg, representation, 99); got != "/artifact.tgz" {
		t.Fatalf("transparent cache key = %q", got)
	}
}

// TestGet_RegistryDigestObjectsAreImmutableWithoutPatterns 回归线上集群的真实配置：
// registry 上游没有填 immutable_patterns 时，blob 与按 digest 请求的 manifest
// 仍是协议定义的内容寻址对象，不能被当成普通可变路径在 5 分钟后清掉。
func TestGet_RegistryDigestObjectsAreImmutableWithoutPatterns(t *testing.T) {
	convey.Convey("registry 自带的内容寻址语义不依赖上游模式", t, func() {
		o := manifestOrigin(t)
		up := registryUpstream("registry.test")
		up.ImmutablePatterns = nil
		up.MutableTTLSeconds = 300
		svc, repo, _ := setupSvc(t, o, up, Options{})

		cases := []struct {
			name      string
			path      string
			immutable bool
		}{
			{"blob 按 digest 寻址", "/library/redis/blobs/sha256:2e752c", true},
			{"转义分隔符的 blob 仍按 digest 寻址", "/library/redis/blobs/sha256%3A2e752c", true},
			{"manifest 按 digest 寻址", "/library/redis/manifests/sha256:e2debf", true},
			{"manifest 按 tag 寻址", "/library/redis/manifests/7", false},
			{"referrers 索引会随仓库内容变化", "/library/redis/referrers/sha256:e2debf", false},
		}
		for _, c := range cases {
			convey.Convey(c.name, func() {
				_, _ = pullWith(t, svc, registryTarget("registry.test", c.path))
				row := repo.byKey(c.path)
				convey.So(row, convey.ShouldNotBeNil)
				convey.So(row.Immutable, convey.ShouldEqual, c.immutable)
				if c.immutable {
					convey.So(row.ExpiresAt, convey.ShouldEqual, int64(0))
				} else {
					convey.So(row.ExpiresAt, convey.ShouldBeGreaterThan, time.Now().Unix())
				}
			})
		}
	})
}

// TestGet_RegistryProtocolRetentionOverridesPatterns 标准 registry 端点的可变性由协议定义。
// 配置里的补充模式不能把 tag manifest 或 referrers 索引变成永久快照。
func TestGet_RegistryProtocolRetentionOverridesPatterns(t *testing.T) {
	convey.Convey("registry 标准可变端点不被补充模式覆盖", t, func() {
		o := manifestOrigin(t)
		up := registryUpstream("registry.test")
		up.ImmutablePatterns = upstream_entity.PatternList{
			"/manifests/", "/referrers/", "/blobs/latest",
		}
		svc, repo, _ := setupSvc(t, o, up, Options{})

		for _, path := range []string{
			"/library/redis/manifests/7",
			"/library/redis/referrers/sha256:e2debf",
		} {
			_, _ = pullWith(t, svc, registryTarget("registry.test", path))
			row := repo.byKey(path)
			convey.So(row, convey.ShouldNotBeNil)
			convey.So(row.Immutable, convey.ShouldBeFalse)
			convey.So(row.ExpiresAt, convey.ShouldBeGreaterThan, time.Now().Unix())
		}

		_, _ = pullWith(t, svc, registryTarget("registry.test", "/library/redis/blobs/latest"))
		nonstandard := repo.byKey("/library/redis/blobs/latest")
		convey.So(nonstandard.Immutable, convey.ShouldBeTrue)
		convey.So(nonstandard.ExpiresAt, convey.ShouldEqual, int64(0))
	})
}

// TestGet_RegistryLegacyImmutableTagIsRefetched 把标准可变端点写成永久对象的旧记录不能继续命中。
func TestGet_RegistryLegacyImmutableTagIsRefetched(t *testing.T) {
	convey.Convey("旧版本永久缓存的 tag manifest 会回源并改回 TTL", t, func() {
		o := manifestOrigin(t)
		up := registryUpstream("registry.test")
		up.ImmutablePatterns = upstream_entity.PatternList{"/manifests/"}
		svc, repo, _ := setupSvc(t, o, up, Options{})
		const path = "/library/redis/manifests/7"

		convey.So(svc.Put(context.Background(), &PutRequest{
			UpstreamID: 9, Key: path, Content: strings.NewReader("stale tag"),
			ContentType: "application/json", Immutable: true,
		}), convey.ShouldBeNil)
		got, meta := pullWith(t, svc, registryTarget("registry.test", path))
		convey.So(got, convey.ShouldEqual, `{"schemaVersion":2,"flavour":"docker"}`)
		convey.So(meta.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusMiss)
		convey.So(o.hits.Load(), convey.ShouldEqual, int64(1))
		row := repo.byKey(path)
		convey.So(row.Immutable, convey.ShouldBeFalse)
		convey.So(row.ExpiresAt, convey.ShouldBeGreaterThan, time.Now().Unix())
	})
}

// TestGet_RegistryLegacyMutableDigestIsPromoted 升级前已经写下的错误记录也要自愈。
// 命中时若只发出字节、不修正元数据，后台 Sweep 仍会在旧 TTL 到点后删掉它。
func TestGet_RegistryLegacyMutableDigestIsPromoted(t *testing.T) {
	convey.Convey("旧版本写成可变的 registry digest 在命中时提升为永久对象", t, func() {
		o := manifestOrigin(t)
		up := registryUpstream("registry.test")
		up.ImmutablePatterns = nil
		svc, repo, _ := setupSvc(t, o, up, Options{})
		const path = "/library/redis/blobs/sha256:2e752c"
		const content = "legacy layer"

		convey.So(svc.Put(context.Background(), &PutRequest{
			UpstreamID: 9, Key: path, Content: strings.NewReader(content),
			ContentType: "application/octet-stream", Immutable: false, TTLSeconds: 300,
		}), convey.ShouldBeNil)
		legacy := repo.byKey(path)
		convey.So(legacy.Immutable, convey.ShouldBeFalse)
		convey.So(legacy.ExpiresAt, convey.ShouldBeGreaterThan, int64(0))
		repo.expire(path)

		got, meta := pullWith(t, svc, registryTarget("registry.test", path))
		convey.So(got, convey.ShouldEqual, content)
		convey.So(meta.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusHit)
		convey.So(o.hits.Load(), convey.ShouldEqual, int64(0))
		promoted := repo.byKey(path)
		convey.So(promoted.Immutable, convey.ShouldBeTrue)
		convey.So(promoted.ExpiresAt, convey.ShouldEqual, int64(0))
	})
}

// TestGet_RegistryPromotionFailureStillServesHit 元数据修正失败只影响自愈，不影响读。
func TestGet_RegistryPromotionFailureStillServesHit(t *testing.T) {
	convey.Convey("提升旧 digest 记录失败时继续命中并在下次重试", t, func() {
		o := manifestOrigin(t)
		up := registryUpstream("registry.test")
		up.ImmutablePatterns = nil
		svc, repo, _ := setupSvc(t, o, up, Options{})
		const path = "/library/redis/blobs/sha256:2e752c"
		const content = "legacy layer"

		convey.So(svc.Put(context.Background(), &PutRequest{
			UpstreamID: 9, Key: path, Content: strings.NewReader(content),
			ContentType: "application/octet-stream", Immutable: false, TTLSeconds: 300,
		}), convey.ShouldBeNil)
		repo.expire(path)
		repo.setPromoteError(errPromote)

		got, meta := pullWith(t, svc, registryTarget("registry.test", path))
		convey.So(got, convey.ShouldEqual, content)
		convey.So(meta.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusHit)
		convey.So(o.hits.Load(), convey.ShouldEqual, int64(0))
		convey.So(repo.byKey(path).Immutable, convey.ShouldBeFalse)

		repo.setPromoteError(nil)
		_, meta = pullWith(t, svc, registryTarget("registry.test", path))
		convey.So(meta.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusHit)
		convey.So(repo.byKey(path).Immutable, convey.ShouldBeTrue)
		convey.So(repo.byKey(path).ExpiresAt, convey.ShouldEqual, int64(0))
	})
}

// pullWith 按给定的请求头拉一次，把响应体读完再关掉——读到 EOF 就意味着这一趟的缓存记录已经落表
// （见 pump），后面那次拉取才有可能命中。
func pullWith(t *testing.T, svc CacheSvc, target *proxy_svc.Target) (string, *proxy_svc.Meta) {
	t.Helper()
	body, meta, err := svc.Get(context.Background(), target)
	if err != nil {
		t.Fatalf("拉取 %s 失败：%v", target.Path, err)
	}
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("读响应体失败：%v", err)
	}
	if err := body.Close(); err != nil {
		t.Fatalf("关响应体失败：%v", err)
	}
	return string(got), meta
}

func TestGet_APKOriginVariantsCacheAndDoNotCross(t *testing.T) {
	profile, ok := packageprofile.Lookup(upstream_entity.PackageProfileAPK)
	if !ok {
		t.Fatal("APK profile is not registered")
	}
	profiles := packageprofile.NewRegistry()
	if err := profiles.Register(profile); err != nil {
		t.Fatal(err)
	}

	var o *originStub
	o = newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.alpine.apk")
		w.Header().Set("Vary", "Origin")
		_, _ = io.WriteString(w, fmt.Sprintf("signed-apk-%d", o.hits.Load()))
	})
	up := staticUpstream("dl-cdn.alpinelinux.org")
	up.PackageProfile = upstream_entity.PackageProfileAPK
	svc, repo, _ := setupSvc(t, o, up, Options{Profiles: profiles})
	const path = "/alpine/v3.22/main/x86_64/busybox-1.37.0-r18.apk"

	pull := func(origin string) (string, *proxy_svc.Meta) {
		t.Helper()
		tg := target(up.Host, path)
		if origin != "" {
			tg.Header.Set("Origin", origin)
		}
		return pullWith(t, svc, tg)
	}
	assertColdWarm := func(origin, wantBody string, wantHits int64) {
		t.Helper()
		coldBody, coldMeta := pull(origin)
		warmBody, warmMeta := pull(origin)
		if coldBody != wantBody || warmBody != wantBody {
			t.Fatalf("Origin %q bodies = %q, %q, want %q", origin, coldBody, warmBody, wantBody)
		}
		if coldMeta.Header.Get(cacheStatusHeader) != cacheStatusMiss ||
			warmMeta.Header.Get(cacheStatusHeader) != cacheStatusHit {
			t.Fatalf("Origin %q cache statuses = %q, %q", origin,
				coldMeta.Header.Get(cacheStatusHeader), warmMeta.Header.Get(cacheStatusHeader))
		}
		if coldMeta.Header.Get("Vary") != "Origin" || warmMeta.Header.Get("Vary") != "Origin" {
			t.Fatalf("Origin %q Vary headers = %q, %q", origin,
				coldMeta.Header.Get("Vary"), warmMeta.Header.Get("Vary"))
		}
		if got := o.hits.Load(); got != wantHits {
			t.Fatalf("Origin %q origin hits = %d, want %d", origin, got, wantHits)
		}
	}

	assertColdWarm("", "signed-apk-1", 1)
	assertColdWarm("https://a.example", "signed-apk-2", 2)
	assertColdWarm("https://b.example", "signed-apk-3", 3)
	if got := len(repo.all()); got != 3 {
		t.Fatalf("cache rows = %d, want 3 isolated Origin variants", got)
	}
}

// TestGet_ManifestAcceptVariantsDoNotShareOneCopy 同一个 tag，两种客户端两份副本。
//
// Accept 决定 registry 返回哪个版本的 manifest，而缓存键里没有它的话，先拉的那个
// 客户端会把自己那份按路径存下来，后到的另一种客户端拿到的是一次**命中**——
// 内容和 Content-Type 都是别人那一份。客户端不会报错，它只是悄悄收到了错的东西。
func TestGet_ManifestAcceptVariantsDoNotShareOneCopy(t *testing.T) {
	convey.Convey("docker 与 OCI 两种 Accept 各存各的副本，且各自独立命中", t, func() {
		o := manifestOrigin(t)
		svc, repo, _ := setupSvc(t, o, registryUpstream("registry.test"), Options{})
		const path = "/library/redis/manifests/7"

		dockerBody, dockerMeta := pullWith(t, svc, registryTarget("registry.test", path,
			dockerManifestType, dockerIndexType))
		convey.So(dockerBody, convey.ShouldContainSubstring, `"flavour":"docker"`)
		convey.So(dockerMeta.Header.Get("Content-Type"), convey.ShouldEqual, dockerManifestType)
		convey.So(dockerMeta.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusMiss)

		// 关键的一次：先前那份 docker manifest 已经在缓存里了，这次要的是 OCI 的。
		ociBody, ociMeta := pullWith(t, svc, registryTarget("registry.test", path,
			ociManifestType, ociIndexType))
		convey.So(ociBody, convey.ShouldContainSubstring, `"flavour":"oci"`)
		convey.So(ociMeta.Header.Get("Content-Type"), convey.ShouldEqual, ociManifestType)
		convey.So(ociMeta.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusMiss)
		convey.So(o.hits.Load(), convey.ShouldEqual, 2)

		// 两份副本此后各自命中，谁也不再回源。
		dockerAgain, dockerMeta2 := pullWith(t, svc, registryTarget("registry.test", path,
			dockerManifestType, dockerIndexType))
		convey.So(dockerAgain, convey.ShouldEqual, dockerBody)
		convey.So(dockerMeta2.Header.Get("Content-Type"), convey.ShouldEqual, dockerManifestType)
		convey.So(dockerMeta2.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusHit)

		ociAgain, ociMeta2 := pullWith(t, svc, registryTarget("registry.test", path,
			ociManifestType, ociIndexType))
		convey.So(ociAgain, convey.ShouldEqual, ociBody)
		convey.So(ociMeta2.Header.Get("Content-Type"), convey.ShouldEqual, ociManifestType)
		convey.So(ociMeta2.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusHit)
		convey.So(o.hits.Load(), convey.ShouldEqual, 2)

		// 表里是两条键不同的记录：唯一索引是 (upstream_id, key)，撞了就写不进来。
		rows := repo.all()
		convey.So(len(rows), convey.ShouldEqual, 2)
		convey.So(rows[0].Key, convey.ShouldNotEqual, rows[1].Key)
		convey.So(rows[0].Key, convey.ShouldContainSubstring, path)
		convey.So(rows[1].Key, convey.ShouldContainSubstring, path)
	})
}

// TestGet_KeyIsUnchangedWithoutAcceptVariance 改动前写下的行不能变成永久 MISS。
//
// 键形一变，库里那些按老形态写下的行就再也查不到了：它们既不会被命中，也不会
// 因为「又拉了一次」而被覆盖，只能一直占着配额。所以「没有 Accept 可言」的请求
// 必须仍旧落在**逐字不变**的老键上——这一条同时决定了不给已有行做数据迁移。
func TestGet_KeyIsUnchangedWithoutAcceptVariance(t *testing.T) {
	convey.Convey("老形态的缓存行照常命中，带 Accept 的那次另起一条不撞索引", t, func() {
		o := manifestOrigin(t)
		svc, repo, _ := setupSvc(t, o, registryUpstream("registry.test"), Options{})
		const path = "/library/redis/manifests/7"
		const legacyBody = `{"schemaVersion":2,"flavour":"改动之前写下的"}`

		// 模拟改动之前留在库里的一行：键就是上游内路径。
		convey.So(svc.Put(context.Background(), &PutRequest{
			UpstreamID: 9, Key: path, Content: strings.NewReader(legacyBody),
			ContentType: dockerManifestType, TTLSeconds: 600,
		}), convey.ShouldBeNil)

		// 不带 Accept 的客户端（apt、go、curl 都是这样）照旧命中那一行。
		body, meta := pullWith(t, svc, registryTarget("registry.test", path))
		convey.So(body, convey.ShouldEqual, legacyBody)
		convey.So(meta.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusHit)
		convey.So(o.hits.Load(), convey.ShouldEqual, 0)

		// `Accept: */*` 是「随便给一份」，和没有 Accept 是同一个意思，同一个键。
		anyBody, anyMeta := pullWith(t, svc, registryTarget("registry.test", path, "*/*"))
		convey.So(anyBody, convey.ShouldEqual, legacyBody)
		convey.So(anyMeta.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusHit)
		convey.So(o.hits.Load(), convey.ShouldEqual, 0)

		// 真正点名要 OCI 的那次另起一条记录，老行原封不动地留着。
		ociBody, ociMeta := pullWith(t, svc, registryTarget("registry.test", path,
			ociManifestType, ociIndexType))
		convey.So(ociBody, convey.ShouldContainSubstring, `"flavour":"oci"`)
		convey.So(ociMeta.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusMiss)
		convey.So(o.hits.Load(), convey.ShouldEqual, 1)

		legacy := repo.byKey(path)
		convey.So(legacy, convey.ShouldNotBeNil)
		convey.So(legacy.ContentType, convey.ShouldEqual, dockerManifestType)
		convey.So(len(repo.all()), convey.ShouldEqual, 2)
	})
}

// dockerAcceptSet / ociAcceptSet 两种客户端各自那一串 Accept 的等价写法。
var dockerAcceptSet = []string{dockerManifestType, dockerIndexType}

// TestNormalizeAccept_SameMeaningLandsOnOneKey 归一化：意思相同的 Accept 归到一个键。
//
// 键不归一比不分变体更糟：空格、先后、q=1、拆成几行——同一个 docker pull 在不同
// 版本、不同工具下就能写出这么多种形态，逐字当键的话每次拉取都是未命中，缓存等于
// 没有。归一的依据是 RFC 9110 §12.5.1：媒体范围的先后不表达偏好，q 的默认值是 1。
func TestNormalizeAccept_SameMeaningLandsOnOneKey(t *testing.T) {
	convey.Convey("空格、先后、q=1 与拆行都不该换一个键", t, func() {
		want := normalizeAccept(headerOf(dockerAcceptSet...))
		convey.So(want, convey.ShouldNotBeEmpty)

		same := map[string][]string{
			"一行逗号分隔":    {dockerManifestType + "," + dockerIndexType},
			"逗号两侧有空格":   {dockerManifestType + " ,   " + dockerIndexType + "  "},
			"先后颠倒":      {dockerIndexType + ", " + dockerManifestType},
			"大小写不同":     {strings.ToUpper(dockerManifestType), dockerIndexType},
			"显式写出默认的 q": {dockerManifestType + ";q=1", dockerIndexType + ";q=1.0"},
			"重复了一项":     {dockerManifestType, dockerIndexType, dockerManifestType},
		}
		for name, lines := range same {
			convey.Convey(name+"：和基准同一个键", func() {
				convey.So(normalizeAccept(headerOf(lines...)), convey.ShouldEqual, want)
			})
		}

		differ := map[string][]string{
			"换成 OCI 那一套": {ociManifestType, ociIndexType},
			"少要一项":       {dockerManifestType},
			"给其中一项降了权重":  {dockerManifestType + ";q=0.5", dockerIndexType},
			"还额外收 */*":   append(append([]string{}, dockerAcceptSet...), "*/*"),
		}
		for name, lines := range differ {
			convey.Convey(name+"：这是另一个意思，另一个键", func() {
				convey.So(normalizeAccept(headerOf(lines...)), convey.ShouldNotEqual, want)
			})
		}

		convey.Convey("非默认的 q 只归一写法，不抹掉", func() {
			convey.So(normalizeAccept(headerOf(dockerManifestType+";q=0.50")),
				convey.ShouldEqual, normalizeAccept(headerOf(dockerManifestType+";q=0.5")))
			convey.So(normalizeAccept(headerOf(dockerManifestType+";q=0.5")),
				convey.ShouldNotEqual, normalizeAccept(headerOf(dockerManifestType)))
		})
	})
}

// TestNormalizeAccept_NoPreferenceIsEmpty 「随便给一份」归一成空串。
//
// 空串就是「没有变体」，也就是改动之前那些行的键形：apt、go、curl 这些不表态的
// 客户端因此继续命中库里已有的行。
func TestNormalizeAccept_NoPreferenceIsEmpty(t *testing.T) {
	convey.Convey("没有 Accept、*/*、空项都算没有偏好", t, func() {
		convey.So(normalizeAccept(http.Header{}), convey.ShouldBeEmpty)
		convey.So(normalizeAccept(headerOf("*/*")), convey.ShouldBeEmpty)
		convey.So(normalizeAccept(headerOf("  */*  ")), convey.ShouldBeEmpty)
		convey.So(normalizeAccept(headerOf("*/*", "*/*")), convey.ShouldBeEmpty)
		convey.So(normalizeAccept(headerOf("")), convey.ShouldBeEmpty)
		convey.So(normalizeAccept(headerOf(",")), convey.ShouldBeEmpty)
	})
}

// TestCacheKey_VariantOnlyWhereAcceptChangesTheAnswer 变体只加在真会变的那种请求上。
//
// 加宽一点的代价是实打实的：浏览器打开一个 GitHub 原始文件时发的是一长串 Accept，
// curl 发的是 */*，把它们也算成变体，同一份文件就按客户端各存一份。
func TestCacheKey_VariantOnlyWhereAcceptChangesTheAnswer(t *testing.T) {
	convey.Convey("只有 registry 的 manifest 请求按 Accept 分变体", t, func() {
		const manifestPath = "/library/redis/manifests/7"
		const blobPath = "/library/redis/blobs/sha256:0f1e2d"
		browserAccept := "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"

		convey.Convey("没有偏好的请求落在逐字不变的老键上", func() {
			convey.So(cacheKey(registryTarget("registry.test", manifestPath)),
				convey.ShouldEqual, manifestPath)
			convey.So(cacheKey(registryTarget("registry.test", manifestPath, "*/*")),
				convey.ShouldEqual, manifestPath)
		})

		convey.Convey("两种 manifest 客户端各是一个键", func() {
			docker := cacheKey(registryTarget("registry.test", manifestPath, dockerAcceptSet...))
			oci := cacheKey(registryTarget("registry.test", manifestPath, ociManifestType, ociIndexType))
			convey.So(docker, convey.ShouldNotEqual, oci)
			convey.So(docker, convey.ShouldStartWith, manifestPath)
			convey.So(oci, convey.ShouldStartWith, manifestPath)
			convey.So(docker, convey.ShouldContainSubstring, variantMarker+"accept=")
		})

		convey.Convey("blob 按 digest 寻址，Accept 改不了它是哪一份", func() {
			convey.So(cacheKey(registryTarget("registry.test", blobPath, dockerAcceptSet...)),
				convey.ShouldEqual, blobPath)
		})

		convey.Convey("仓库名叫 manifests 的那次 blob 请求也不算", func() {
			const trap = "/library/manifests/blobs/sha256:0f1e2d"
			convey.So(cacheKey(registryTarget("registry.test", trap, dockerAcceptSet...)),
				convey.ShouldEqual, trap)
		})

		convey.Convey("静态上游原样发文件，浏览器那串 Accept 不该另存一份", func() {
			static := target("raw.githubusercontent.com", "/foo/bar/main/x.sh")
			static.Header.Set("Accept", browserAccept)
			convey.So(cacheKey(static), convey.ShouldEqual, "/foo/bar/main/x.sh")
		})

		convey.Convey("查询串照旧进键，变体缀在它后面", func() {
			withQuery := registryTarget("registry.test", manifestPath, dockerAcceptSet...)
			withQuery.RawQuery = "ns=docker.io"
			convey.So(cacheKey(withQuery), convey.ShouldStartWith, manifestPath+"?ns=docker.io")
		})
	})
}

// TestCacheKey_VariantCannotBeSpelledByAClient 老键与变体键不可能逐字相同。
//
// 键进的是 (upstream_id, key) 那条唯一索引，两种键形一旦能拼成同一个串，一个精心
// 构造的查询串就能占住别人那一格：查询串是原样进键的，谁都能往里写字符。分隔符
// 取控制字节的理由就在这里——带控制字节的请求行在 net/url 那一关就被拒了，客户端
// 能送进查询串的只有转义形态，而那是另一个串。
func TestCacheKey_VariantCannotBeSpelledByAClient(t *testing.T) {
	convey.Convey("客户端拼不出分隔符，也就撞不上变体键", t, func() {
		const manifestPath = "/library/redis/manifests/7"
		const query = "ns=docker.io"
		withQuery := registryTarget("registry.test", manifestPath, ociManifestType, ociIndexType)
		withQuery.RawQuery = query
		variant := cacheKey(withQuery)
		suffix := strings.TrimPrefix(variant, manifestPath+"?"+query)
		convey.So(suffix, convey.ShouldStartWith, variantMarker)

		// 要占住这一格，客户端得发一个查询串正好是 query+suffix 的请求——
		// 那样的请求行连解析都过不去。
		_, err := url.ParseRequestURI("/v2/registry.test" + manifestPath + "?" + query + suffix)
		convey.So(err, convey.ShouldNotBeNil)

		// 它送得进来的只有转义形态，落成的是另一个键。
		imitation := registryTarget("registry.test", manifestPath)
		imitation.RawQuery = query + url.PathEscape(variantMarker) +
			strings.TrimPrefix(suffix, variantMarker)
		key := cacheKey(imitation)
		convey.So(key, convey.ShouldNotEqual, variant)
		convey.So(key, convey.ShouldNotContainSubstring, variantMarker)
	})
}

// TestEnforceQuota_UnreachableLegacyRowsAgeOut 查不到的老行不会永远占着配额。
//
// 这是「不给已有行做数据迁移」这个选择的另一半：改动之前带着 Accept 拉下来的
// manifest 写的是老键形，此后没有任何请求还会落到那个键上——它既不命中也不被覆盖。
// 兜住它的是本来就有的两条路：可变对象由 TTL 过期，不可变对象由 LRU 淘汰，两者
// 都只看访问时间，不认识键的形状。再也没人来取的行就是最久未访问的那一批，
// 于是它们排在被削掉的最前面。
func TestEnforceQuota_UnreachableLegacyRowsAgeOut(t *testing.T) {
	convey.Convey("再也查不到的老行照样最先被 LRU 削掉", t, func() {
		o := manifestOrigin(t)
		// 配额 25 字节、回收到 80%：写进第三个 10 字节的对象就得腾地方。
		svc, repo, _ := setupSvc(t, o, registryUpstream("registry.test"),
			Options{Runtime: newFakeRuntime(t, quotaOf(25, 80))})
		const legacyKey = "/library/redis/manifests/7"

		putLegacy(t, svc, legacyKey, "aaaaaaaaaa")
		putLegacy(t, svc, "/library/nginx/blobs/sha256:bb", "bbbbbbbbbb")
		convey.So(repo.byKey(legacyKey), convey.ShouldNotBeNil)

		putLegacy(t, svc, "/library/nginx/blobs/sha256:cc", "cccccccccc")
		convey.So(repo.byKey(legacyKey), convey.ShouldBeNil)
	})
}

// putLegacy 按改动之前的键形往库里写一条不可变记录。
func putLegacy(t *testing.T, svc CacheSvc, key, content string) {
	t.Helper()
	if err := svc.Put(context.Background(), &PutRequest{
		UpstreamID: 9, Key: key, Content: strings.NewReader(content),
		ContentType: dockerManifestType, Immutable: true,
	}); err != nil {
		t.Fatalf("写入缓存失败：%v", err)
	}
}

// headerOf 把几行 Accept 装成请求头。
func headerOf(lines ...string) http.Header {
	header := http.Header{}
	for _, line := range lines {
		header.Add("Accept", line)
	}
	return header
}
