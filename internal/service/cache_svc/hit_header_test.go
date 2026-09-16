package cache_svc

import (
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
