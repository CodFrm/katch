package cache_svc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"testing"

	"github.com/smartystreets/goconvey/convey"
)

// TestGet_RegistryHitCarriesContentDigest 命中与未命中必须给出同一套 registry 协议头。
//
// 上游在未命中时给的是 Docker-Content-Digest 与 Etag，命中时却只剩 Content-Type 与
// Content-Length：同一个 URL 的响应头随「这次有没有命中」而变。摘要是 registry 协议里
// 客户端可以依赖的字段，而「有时有、有时没有」恰恰是最难查的那一种不一致。
//
// 值不从上游那份拷贝而来，而是由这份副本自己的摘要推导：命中路径在发第一个字节之前
// 已经校验过盘上的字节与 Digest 相符（见 serveFromDisk），推导出来的值因此不可能和
// 发出去的字节对不上，而存一份副本会多出一个可以和字节分叉的事实。
func TestGet_RegistryHitCarriesContentDigest(t *testing.T) {
	convey.Convey("registry 上游命中时补回 Docker-Content-Digest 与 Etag", t, func() {
		o := manifestOrigin(t)
		svc, repo, _ := setupSvc(t, o, registryUpstream("registry.test"), Options{})
		const path = "/library/redis/manifests/7"

		body, missMeta := pullWith(t, svc, registryTarget("registry.test", path, ociManifestType))
		convey.So(missMeta.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusMiss)

		_, hitMeta := pullWith(t, svc, registryTarget("registry.test", path, ociManifestType))
		convey.So(hitMeta.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusHit)

		// 摘要就是这份响应体的 sha256，客户端拿它去核对必须对得上。
		sum := sha256.Sum256([]byte(body))
		want := "sha256:" + hex.EncodeToString(sum[:])
		convey.So(hitMeta.Header.Get("Docker-Content-Digest"), convey.ShouldEqual, want)
		convey.So(hitMeta.Header.Get("Etag"), convey.ShouldEqual, `"`+want+`"`)
		// 和库里那条记录是同一个事实，不是另算的一份。
		convey.So(repo.all()[0].Digest, convey.ShouldEqual, want)
		// 原有的两个头一个都不能丢。
		convey.So(hitMeta.Header.Get("Content-Type"), convey.ShouldEqual, ociManifestType)
		convey.So(hitMeta.Header.Get("Content-Length"), convey.ShouldEqual, strconv.Itoa(len(body)))
	})
}

// TestGet_RegistryHitPrefersUpstreamValidator registry 上游自己带了 ETag 时，
// 命中原样回放上游那一串；只有它没给时才退回按摘要推导的值。
//
// 未命中发的是上游的 ETag，命中却换成另一个推导值，同一份内容就有了两个互不相认的
// 强校验符，客户端此后拿着命中的那个去做条件请求，只会换回一次整份重传。
// Docker-Content-Digest 是另一回事，它始终由副本自身的摘要推导，与回放的 ETag 无关。
func TestGet_RegistryHitPrefersUpstreamValidator(t *testing.T) {
	convey.Convey("registry 上游提供 ETag 时命中原样回放", t, func() {
		const etag = `"upstream-registry-etag"`
		const manifest = `{"schemaVersion":2,"flavour":"oci"}`
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", ociManifestType)
			w.Header().Set("Etag", etag)
			_, _ = io.WriteString(w, manifest)
		})
		svc, _, _ := setupSvc(t, o, registryUpstream("registry.test"), Options{})
		const path = "/library/redis/manifests/7"

		_, missMeta := pullWith(t, svc, registryTarget("registry.test", path, ociManifestType))
		convey.So(missMeta.Header.Get("Etag"), convey.ShouldEqual, etag)

		_, hitMeta := pullWith(t, svc, registryTarget("registry.test", path, ociManifestType))
		convey.So(hitMeta.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusHit)
		convey.So(hitMeta.Header.Get("Etag"), convey.ShouldEqual, etag)
		// 摘要头仍由副本自身推导，与回放的 ETag 是两个不同的事实。
		sum := sha256.Sum256([]byte(manifest))
		convey.So(hitMeta.Header.Get("Docker-Content-Digest"), convey.ShouldEqual,
			"sha256:"+hex.EncodeToString(sum[:]))
	})
}

// TestGet_StaticHitReplaysUpstreamValidators static 上游命中时原样回放上游的
// ETag 与 Last-Modified。
//
// 未命中时客户端拿到的是**上游那一串** validator；命中时若换成 katch 自己推导的
// 摘要，同一份内容就有了两个互不相认的强校验符，客户端此后拿着推导出来的那个去做
// 条件请求，只会换回一次整份重传。Docker-Content-Digest 也要继续缺席：它是 registry
// 协议的字段，APT 源与 Go proxy 的客户端不认它。
func TestGet_StaticHitReplaysUpstreamValidators(t *testing.T) {
	convey.Convey("static 上游的 ETag 与 Last-Modified 命中时原样回放", t, func() {
		const etag = `"origin-own-etag"`
		const lastModified = "Wed, 21 Oct 2015 07:28:00 GMT"
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Etag", etag)
			w.Header().Set("Last-Modified", lastModified)
			_, _ = io.WriteString(w, "hello")
		})
		svc, repo, _ := setupSvc(t, o, staticUpstream("files.test"), Options{})
		const path = "/pool/main/h/hello.txt"

		_, missMeta := pullWith(t, svc, target("files.test", path))
		convey.So(missMeta.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusMiss)
		convey.So(missMeta.Header.Get("Etag"), convey.ShouldEqual, etag)
		convey.So(missMeta.Header.Get("Last-Modified"), convey.ShouldEqual, lastModified)
		// validator 随内容一起落库，命中才有东西可回放。
		row := repo.byKey(path)
		convey.So(row, convey.ShouldNotBeNil)
		convey.So(row.ETag, convey.ShouldEqual, etag)
		convey.So(row.LastModified, convey.ShouldEqual, lastModified)

		_, hitMeta := pullWith(t, svc, target("files.test", path))
		convey.So(hitMeta.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusHit)
		convey.So(hitMeta.Header.Get("Docker-Content-Digest"), convey.ShouldEqual, "")
		convey.So(hitMeta.Header.Get("Etag"), convey.ShouldEqual, etag)
		convey.So(hitMeta.Header.Get("Last-Modified"), convey.ShouldEqual, lastModified)
		convey.So(o.hits.Load(), convey.ShouldEqual, 1)
	})
}

// TestGet_MavenHitReplaysChecksumHeaders covers Maven clients that use checksum response
// metadata instead of fetching checksum sidecars. A cache hit must expose the exact values
// seen on the cold response, or a warm dependency resolution returns to the origin for them.
func TestGet_MavenHitReplaysChecksumHeaders(t *testing.T) {
	convey.Convey("Maven checksum headers survive a cache miss followed by a hit", t, func() {
		checksums := map[string]string{
			"X-Checksum-MD5":    "42F7E9AC3C79F7BA5B3A2E81D2D52A92",
			"X-Checksum-SHA1":   "8f4e3c42b6d9c76e8797c60d345320ef4cf54f39",
			"X-Checksum-SHA256": "8EA44E1F012D62EEB020CD8BE16F9C602EE7AC28E46026D136A29AB80D34F4D2",
			"X-Checksum-SHA512": "1a2b3c4d5e6f77889900aabbccddeeff00112233445566778899aabbccddeeff" +
				"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		}
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/java-archive")
			for name, value := range checksums {
				w.Header().Set(name, value)
			}
			_, _ = io.WriteString(w, "maven artifact")
		})
		svc, _, _ := setupSvc(t, o, staticUpstream("repo.maven.apache.org"), Options{})
		const path = "/maven2/org/example/demo/1.0/demo-1.0.jar"

		_, missMeta := pullWith(t, svc, target("repo.maven.apache.org", path))
		convey.So(missMeta.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusMiss)
		for name, value := range checksums {
			convey.So(missMeta.Header.Get(name), convey.ShouldEqual, value)
		}

		_, hitMeta := pullWith(t, svc, target("repo.maven.apache.org", path))
		convey.So(hitMeta.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusHit)
		for name, value := range checksums {
			convey.So(hitMeta.Header.Get(name), convey.ShouldEqual, value)
		}
		convey.So(o.hits.Load(), convey.ShouldEqual, 1)
	})
}

// TestGet_StaticNotModifiedKeepsValidators 本地 304 复用 200 的 validator 与归因，
// 但摘掉实体相关的字段。
//
// 304 不带响应体，声明了长度或类型就与「这次没有实体」自相矛盾。
func TestGet_StaticNotModifiedKeepsValidators(t *testing.T) {
	convey.Convey("本地 304 的形状", t, func() {
		const etag = `"origin-own-etag"`
		const lastModified = "Wed, 21 Oct 2015 07:28:00 GMT"
		o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Etag", etag)
			w.Header().Set("Last-Modified", lastModified)
			_, _ = io.WriteString(w, "hello")
		})
		svc, _, _ := setupSvc(t, o, staticUpstream("files.test"), Options{})
		const path = "/pool/main/h/hello.txt"
		pullWith(t, svc, target("files.test", path))

		tg := target("files.test", path)
		tg.Header.Set("If-None-Match", etag)
		got, meta, err := svc.Get(context.Background(), tg)
		convey.So(err, convey.ShouldBeNil)
		payload, err := io.ReadAll(got)
		convey.So(err, convey.ShouldBeNil)
		convey.So(got.Close(), convey.ShouldBeNil)
		convey.So(string(payload), convey.ShouldBeEmpty)
		convey.So(meta.StatusCode, convey.ShouldEqual, http.StatusNotModified)
		convey.So(meta.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusHit)
		convey.So(meta.Header.Get("Etag"), convey.ShouldEqual, etag)
		convey.So(meta.Header.Get("Last-Modified"), convey.ShouldEqual, lastModified)
		convey.So(meta.Header.Get("Content-Length"), convey.ShouldBeEmpty)
		convey.So(meta.Header.Get("Content-Type"), convey.ShouldBeEmpty)
		convey.So(meta.ContentLength, convey.ShouldEqual, int64(0))
		convey.So(o.hits.Load(), convey.ShouldEqual, 1)
	})
}

// TestGet_RegistryDigestETagAnswersConditional registry 没有上游 ETag 时，
// 本地条件请求用内容摘要推导出来的那一串比较。
//
// 这正是 registry 客户端手里有的那个强校验符（响应里的 Etag 与
// Docker-Content-Digest），不拿它求值，带 If-None-Match 的拉取就永远回源。
func TestGet_RegistryDigestETagAnswersConditional(t *testing.T) {
	convey.Convey("registry 用推导出的摘要 ETag 回答条件请求", t, func() {
		o := manifestOrigin(t)
		svc, _, _ := setupSvc(t, o, registryUpstream("registry.test"), Options{})
		const path = "/library/redis/manifests/7"
		body, _ := pullWith(t, svc, registryTarget("registry.test", path, ociManifestType))

		sum := sha256.Sum256([]byte(body))
		digest := "sha256:" + hex.EncodeToString(sum[:])
		tg := registryTarget("registry.test", path, ociManifestType)
		tg.Header.Set("If-None-Match", `"`+digest+`"`)
		got, meta, err := svc.Get(context.Background(), tg)
		convey.So(err, convey.ShouldBeNil)
		convey.So(got.Close(), convey.ShouldBeNil)
		convey.So(meta.StatusCode, convey.ShouldEqual, http.StatusNotModified)
		convey.So(meta.Header.Get(cacheStatusHeader), convey.ShouldEqual, cacheStatusHit)
		convey.So(meta.Header.Get("Etag"), convey.ShouldEqual, `"`+digest+`"`)
		convey.So(meta.Header.Get("Docker-Content-Digest"), convey.ShouldEqual, digest)
		convey.So(meta.Header.Get("Content-Length"), convey.ShouldBeEmpty)
		convey.So(o.hits.Load(), convey.ShouldEqual, 1)
	})
}
