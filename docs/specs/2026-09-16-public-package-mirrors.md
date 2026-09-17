# 公开包管理器镜像加速

> Status: Approved
> Owner: katch maintainers
> Last updated: 2026-09-16

**Objective:** 让常见包管理器在客户端无法访问公网时，仅通过 katch 完成公开包的元数据解析、制品下载和原生完整性校验，并让重复下载按协议语义命中缓存。

**Hard invariant:** katch 保持公开只读代理；不得转发发布、登录、上传、删除或私有仓库凭据，不得改动受摘要或签名保护的制品正文，不得根据上游元数据动态形成开放代理，构建产物继续保持 `CGO_ENABLED=0` 可用。

## Problem

1. **元数据会绕过镜像。** npm packument、PyPI Simple API、Cargo sparse、NuGet V3、Composer v2 和 Homebrew artifact 流程都能把客户端引向绝对公网 URL；真实客户端阻断公网测试已分别在 Bun、pip、uv、Cargo、NuGet、Composer 和 Homebrew 源码下载中复现，详见 [公开包镜像加速兼容性](../package-manager-mirrors.md)。
2. **static 缓存会混淆协商表示。** 同一路径的 npm install-v1/full packument 及 PyPI HTML/JSON 内容不同；当前缓存键不含这些 static 请求的 `Accept`，真实请求已复现后到表示命中先到表示的错误。
3. **缓存命中缺少下载语义。** 当前 static 命中不恢复 ETag、Last-Modified、Vary 或 Accept-Ranges，HEAD、Range 和条件请求只能穿透；请求 gzip 元数据时响应不会进入缓存，真实 npm 请求因而持续 MISS。
4. **协议可变性不能只靠站长填写路径模式。** tag、索引、快照、带版本制品和带摘要对象在各协议中的可变性不同；错误的长期缓存会隐藏新版本或永久保存可变对象。
5. **重定向和嵌套 URL 缺少同一条目标约束。** 元数据 URL、HTTP 重定向和 registry token realm 都可能把服务端带到初始上游之外；只验证首个请求主机不足以维持上游白名单边界。

## Actors and user stories

1. As a katch administrator, I want to select a package metadata adapter for a static upstream and see its required companion upstreams, so that clients receive a complete, closed mirror path instead of partially working configuration.
2. As a developer, I want to point npm-family, Python, Go, JVM, Cargo, NuGet, Ruby, APT, DNF/YUM, APK, Composer and Homebrew clients at katch, so that dependency resolution and artifact verification succeed without direct public network access.
3. As an operator, I want metadata and immutable artifacts to use different cache semantics while preserving validators, so that warm requests reduce origin traffic without serving stale mutable state or invalid bytes.

## Design decisions

| #   | Decision                                                                                                                                                                                                                                                                                                            | Basis and rejected option                                                                                                                                                                                                       |
| --- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1   | Keep `static`, `registry` and `git` as transport protocols and add a separate package adapter/profile: `none`, `npm`, `pypi`, `goproxy`, `maven`, `cargo`, `nuget`, `rubygems`, `apt`, `rpm`, `apk`, `composer` or `homebrew`. Profiles that do not transform metadata still provide protocol cache classification. | Transport, response transformation and package-path semantics are independent from request routing. Rejected: add every ecosystem to `protocols` - this overloads routing and makes byte-transparent static behavior ambiguous. |
| 2   | Transform metadata after a successful origin response and before cache insertion; cache the transformed representation.                                                                                                                                                                                             | Cold and warm clients must observe the same URLs and validators. Rejected: rewrite only while sending a miss - cache hits would leak original URLs or require repeated parsing.                                                 |
| 3   | Rewrite only protocol-defined URL fields, and only to hosts represented by enabled upstream records with a compatible transport.                                                                                                                                                                                    | This preserves a finite administrator-approved destination set. Rejected: recursively rewrite every URL string or auto-create upstreams - project, license and arbitrary links would turn katch into an open proxy.             |
| 4   | Build absolute rewritten URLs from `site_domain`; never use the request Host or forwarding headers. Adapter-backed requests fail as unavailable when no public base URL is configured.                                                                                                                              | Cached metadata is shared across requests and cannot safely depend on attacker-controlled authority. Rejected: derive from each request - this permits cache poisoning and unstable lockfiles.                                  |
| 5   | Parse JSON and HTML structurally, request identity encoding for transformable metadata, and cap transformed metadata at 16 MiB.                                                                                                                                                                                     | Rewriting must preserve escaping and attributes while bounding memory. Rejected: text replacement or buffering artifacts - both are unsafe for signed data and large downloads.                                                 |
| 6   | Store one canonical identity-coded object per cache variant, persist safe validators, and make cache variants protocol-aware.                                                                                                                                                                                       | Real clients rely on validators, ranges and content negotiation. Rejected: cache encoded and identity bodies under one key or cache only Content-Type and bytes - both produce incorrect warm responses.                        |
| 7   | Keep signed indexes and artifact bodies byte-identical; solve Homebrew bottles through its supported OCI artifact-domain mapping and Go checksum through a sumdb bridge instead of body rewriting.                                                                                                                  | Homebrew JWS, APT Release data and APK indexes cannot be modified without invalidating trust. Rejected: rewrite every metadata document or claim Homebrew source builds use artifact-domain - Homebrew 6 does neither safely.   |
| 8   | Deliver all listed ecosystems in dependency order, but do not mark an ecosystem supported until its real client passes with public egress blocked.                                                                                                                                                                  | Shared cache and safety behavior must precede adapters. Rejected: infer compatibility from successful curl requests - earlier research showed this misses nested URLs.                                                          |

## Upstream configuration

An upstream keeps its existing transport protocol set and gains one package adapter/profile value. `none` preserves current behavior. Package profiles are valid only when `static` is enabled; registry and Git behavior remain independently selectable for companion hosts such as `ghcr.io` or public source repositories. Profiles such as `maven`, `rubygems`, `apt`, `rpm` and `apk` leave response bodies untouched and contribute only path classification, client guidance and validation.

The admin create/edit API and interface expose the adapter. The interface explains which companion hosts and companion profiles are required and refuses incompatible protocol/profile combinations. All new interface text uses the existing translation system.

An upstream has exactly one package profile. Companion hosts use the profile named by their protocol when classification is known: for example both `pypi.org` and `files.pythonhosted.org` use `pypi`, and both `index.crates.io` and `static.crates.io` use `cargo`. A host intentionally shared by unrelated ecosystems uses `none` plus explicit administrator patterns; that configuration is not advertised as protocol-complete. The interface lists the required profile for each companion.

Enabling an adapter does not create companion upstreams. If transformed metadata references a required host that is absent, disabled, profile-incompatible or transport-incompatible, the metadata request fails before any transformed body is cached. The failure identifies the missing host in administrator logs but does not expose internal configuration details to anonymous clients.

`site_domain` is mandatory for adapters that emit absolute URLs. Clearing it while such adapters exist is allowed for recovery, but their metadata endpoints return `503 Service Unavailable` until it is restored; byte-transparent artifact endpoints continue to work.

A monotonic rewrite generation changes whenever `site_domain`, an adapter/profile, or an enabled companion-upstream relationship changes. Transformed metadata cache identity includes that generation. The new generation becomes visible atomically with the configuration change, so old transformed bodies and ETags stop matching immediately without deleting unrelated artifact objects.

## Request and response flow

Only existing read methods are accepted. Package adapters do not enable POST endpoints such as npm audit or NuGet publish. Existing Git upload-pack handling remains separate.

For an adapter-recognized metadata path, katch obtains a complete unconditional identity representation from the origin, verifies status and media type, enforces the 16 MiB limit, parses the body and rewrites known fields. This rule also applies when the client sent HEAD, Range or a conditional header: katch first obtains or refreshes the canonical transformed object, then evaluates the client's method and preconditions locally. A malformed, oversized or semantically incomplete document returns `502 Bad Gateway` and is not cached. Non-metadata paths pass through without buffering on a cold HEAD, Range or conditional request.

An origin 404 or 410 remains the same client-visible status so package-not-found behavior is preserved. Other origin 4xx and 5xx statuses are passed through after the existing response-header filtering. No non-200 response is transformed or cached. A redirect is resolved under the destination-safety rules before transformation; a blocked redirect, unexpected success status or wrong metadata media type returns 502. Missing `site_domain` or a required companion upstream is a local availability failure and returns 503. Anonymous error bodies do not disclose which configuration check failed.

Every rewritten absolute URL has the form `<site base>/<registered host>/<original escaped path and query>`. URL fragments are retained where the client uses them for hashes. Relative source URLs are resolved against the source metadata URL before applying the same allowlist rule. Userinfo and non-HTTP(S) URLs are rejected in protocol fields that represent required downloads.

Template-valued protocol fields are not parsed as ordinary URLs. Composer v2 preserves the literal `%package%` token while replacing only the registered authority and base prefix. Cargo emits a plain registered katch download base and preserves the trailing-slash semantics expected by Cargo instead of forwarding unsupported placeholders. NuGet resource bases retain their required trailing slash, and child identifiers are rewritten after normal URL resolution without double escaping.

A transformed response has identity encoding, a recalculated Content-Length and a katch ETag over the transformed bytes. Origin Content-Encoding, ETag and Last-Modified are removed; transformed metadata therefore supports ETag conditions but does not claim an origin modification time. Vary retains only adapter-declared request dimensions that still change the transformed representation. Artifact and signed-index responses retain their original body and safe origin validators.

## Cache and download semantics

Cache identity includes path, query, adapter-declared request variants and, for transformed metadata, the rewrite generation. npm packument and PyPI Simple metadata vary on normalized `Accept`; registry manifests retain their existing manifest Accept variant. Header ordering, casing and an explicit default quality value do not create duplicate variants.

Every cache-fill request sets `Accept-Encoding: identity`, regardless of the client's accepted content codings. The stored object therefore never varies on `Accept-Encoding`; katch deliberately removes that dimension from Vary on its canonical identity response. A client that explicitly forbids identity bypasses object caching for byte-transparent content and receives `406 Not Acceptable` for transformable metadata. If an origin ignores the identity request and returns a content-encoded transformable document, katch returns 502 rather than parsing ambiguous bytes; encoded byte-transparent content may pass through but is not cached. `Vary: *` and Vary dimensions other than the removed `Accept-Encoding` or those declared by the active profile make a response uncacheable.

Cache records retain the safe response metadata required by supported read clients: Content-Type, representation ETag, Last-Modified, effective Cache-Control, origin Date/Age/Expires, Vary, Accept-Ranges, Content-Disposition and Docker-Content-Digest. Hop-by-hop, Content-Encoding, authentication, cookie, account quota and upstream transport-policy headers are never stored or replayed.

Local freshness is no longer than either the profile/upstream TTL or a more restrictive origin directive. `no-store` and `private` responses are not stored; `no-cache` and `must-revalidate` objects are stored only as bodies and refreshed before reuse. Warm responses expose an Age derived from the origin Age and local residence time, so cache hits do not restart the origin freshness lifetime.

A fresh cached full object answers GET and HEAD without origin access. Preconditions are evaluated in RFC order against the selected katch representation: If-Match uses strong comparison and a mismatch returns 412; If-Unmodified-Since is evaluated only when If-Match is absent and failure returns 412; If-None-Match uses weak comparison and a GET/HEAD match returns 304; If-Modified-Since is evaluated only when If-None-Match is absent and an unchanged GET/HEAD returns 304. Transformed metadata without Last-Modified ignores date-based conditions and remains ETag-addressable.

Range applies only after preconditions and only to GET. It supports closed, open-ended, suffix and multiple ranges: one satisfiable range returns 206 with Content-Range, multiple ranges return `multipart/byteranges`, and no satisfiable range returns 416 with `Content-Range: bytes */<length>`. If-Range requires a strong matching ETag or valid matching date; otherwise katch sends the full representation. HEAD ignores Range. Unsupported conditional methods are already rejected by the read-only method boundary.

A cold HEAD, Range or conditional request for byte-transparent content passes through and never stores a partial response or 304 as a full object. The same request for transformable metadata first performs the canonical unconditional identity GET described above, then applies HEAD, range and conditions to the transformed representation. If that representation is uncacheable, katch may still answer the current request from the bounded transformed buffer but does not retain it.

Expired mutable objects are refreshed with an unconditional canonical GET in this change; katch does not send its representation ETag to the origin. After refresh, client conditions are evaluated against the new representation validator. An origin failure does not silently promote expired metadata to fresh.

Adapters provide protocol-owned classification before administrator `immutable_patterns`; explicitly mutable protocol paths cannot be overridden into immutable ones. Unknown non-standard paths continue to use administrator patterns and the upstream mutable TTL.

## Ecosystem contracts

### npm family

For packument responses, katch rewrites every `versions.*.dist.tarball` URL and preserves `integrity`, `shasum`, version data and dist-tags. Full and install-v1 representations remain distinct. Scoped package escaping is preserved. Versioned `.tgz` artifacts are immutable; packuments and dist-tags are mutable.

The supported client matrix is npm, pnpm, Yarn Classic, current Yarn Berry and Bun. Client guidance includes registry configuration and npm registry-host replacement for existing lockfiles. Existing lockfiles containing public absolute URLs are supported only when that client can map the registry host; otherwise they must be regenerated through katch.

### Python package index

The PyPI adapter supports PEP 503 HTML and PEP 691 JSON Simple responses. It rewrites distribution links while preserving hash fragments, `data-*` attributes, requires-python, yanked state and metadata hashes. PEP 658 `.metadata` requests follow the rewritten distribution host. Simple pages are mutable; hash-addressed wheels, sdists and metadata sidecars are immutable.

The supported client matrix is pip, uv and current Poetry.

### Go modules

Module proxy paths remain byte-transparent, including escaped uppercase module segments. Fixed-version `.info`, `.mod` and `.zip` objects are immutable; list and latest endpoints are mutable.

The public checksum database supported in this change is `sum.golang.org`. A request to `/sumdb/sum.golang.org/supported` returns a synthetic empty 200 response only when an enabled `sum.golang.org` static upstream with the `goproxy` profile exists; missing or invalid configuration returns a non-fallback 503 rather than 404/410. For `/sumdb/sum.golang.org/lookup/...`, `/tile/...` and `/latest`, katch removes the `/sumdb/sum.golang.org` prefix and sends the remaining `/lookup/...`, `/tile/...` or `/latest` path to that upstream. Other paths under the prefix return 404 without origin access. Origin lookup/tile/latest statuses and bytes pass through unchanged. Lookup records and complete tiles are immutable; `/latest` and partial `.p/<width>` tiles are mutable. A Go client configured with katch as its only GOPROXY must not access `sum.golang.org` directly.

### Maven repositories

Maven, Gradle and sbt use a fixed katch repository base URL. POM, module metadata, checksum and signature files remain byte-identical. Release artifacts covered by repository immutability guarantees are immutable; `maven-metadata.xml`, changing snapshots and non-unique snapshot paths remain mutable. Multiple repositories require separate registered hosts.

### Cargo

Only the maintained sparse-index flow receives metadata transformation. The adapter replaces the `config.json` `dl` value with a plain katch base for the registered `static.crates.io` companion and preserves Cargo's standard appended download path; unsupported template forms from nonstandard registries are rejected rather than copied ambiguously. Crate index entries and their checksums remain unchanged. Versioned `.crate` downloads are immutable. Git-index content is never rewritten; users choosing the Git index must separately configure both Git and crate downloads.

### NuGet

NuGet V3 support rewrites protocol navigation fields in the Service Index, registration pages/leaves and search resources, including resource `@id`, registration links, `packageContent` and `catalogEntry`. Project, license, icon and documentation links are not proxy download fields and remain unchanged. Repository-signature resources remain reachable through katch, while `.nupkg` and embedded package signatures remain byte-identical. Service, search and registration metadata are mutable; versioned package content is immutable.

Only read restore/install/search behavior is supported. API keys, push, delete and authenticated feeds remain rejected.

### Ruby packages

RubyGems and Bundler use the configured katch source. Legacy indexes, compact-index `/versions` and `/info/*`, gemspecs and `.gem` files remain byte-identical. Compact-index validators and range behavior work from warm cache. Versioned `.gem` artifacts are immutable; index state is mutable.

### Linux distribution repositories

APT source entries, DNF/YUM `baseurl` entries and Alpine repository entries point to a fixed registered host under katch. Signed metadata and package bodies remain byte-identical.

APT pool artifacts and by-hash objects are immutable; Release/InRelease and suite indexes are mutable. Versioned APK files are immutable and APKINDEX is mutable. RPMs or repodata objects addressed by a strong digest are immutable; repomd and non-digest metadata are mutable.

Dynamic DNF/YUM mirrorlist and metalink selection is not supported; clients use the fixed katch baseurl so an external mirror cannot bypass the proxy.

### Composer

Composer support targets maintained Composer 2 / Packagist v2 metadata. The root `metadata-url` is treated as a protocol template: katch rewrites its authority and base prefix while preserving the literal `%package%` substitution token. Resolved package metadata rewrites only registered `dist.url` fields. Package dependency data, dist references and available hashes remain unchanged. Supported installs use dist artifacts and fail closed when a required dist host is not registered.

Composer 1 provider metadata is not transformed. Existing lockfiles with public dist URLs must be regenerated through katch unless the client configuration can map those URLs.

### Homebrew

Homebrew API and JWS documents pass through byte-identically via `HOMEBREW_API_DOMAIN`. Bottle manifest/blob downloads use `HOMEBREW_ARTIFACT_DOMAIN=<site base>/registry/ghcr.io` and the existing registered `ghcr.io` registry upstream. Homebrew appends `/v2/<repository>/...` to that base; the explicit `/registry/<host>/v2` route consumes only this protocol boundary and leaves a legitimate repository path such as `v2/foo` intact. `HOMEBREW_ARTIFACT_DOMAIN_NO_FALLBACK=1` is mandatory so a mirror failure cannot fall back to public GHCR. Tap Git remotes use the existing Git path. Bottle digest and checksum verification remain unchanged.

OCI blobs and digest manifests are immutable; tag manifests and API data are mutable. Homebrew 6 does not apply artifact-domain rewriting to ordinary formula source URLs despite its environment-variable description, as confirmed by its download strategy and a blocked-egress run. Standard bottle installation is supported; `--build-from-source` is not presented as mirrored.

## Destination safety

A client-controlled path, transformed metadata URL, HTTP redirect or registry token realm must not cause an outbound request to an unregistered host. Redirects are followed only when their target maps to an enabled upstream and the redirect preserves the operation's read-only contract. HTTPS-to-HTTP downgrades and redirect URLs containing userinfo are rejected. A cross-host registry token realm must have its own enabled static upstream record; registry configuration guidance lists required realms such as `auth.docker.io`, and no client Authorization header is forwarded to that realm.

For package adapters, dynamically resolved public origins must not dial loopback, link-local, multicast or private addresses. Explicit administrator-configured private origins used by generic non-package upstreams retain existing behavior. Address validation is applied to the actual dial target so DNS rebinding cannot pass validation with one address and connect to another.

Anonymous responses do not reveal whether a blocked host was absent, disabled or address-rejected. Administrator logs retain enough reason and host information to repair configuration without logging credentials or query secrets.

## Client configuration surface

Documentation and the public/admin interface provide copyable configuration for every supported client family, including required trailing slashes, insecure-localhost exceptions used only for development, companion upstream hosts and old-lockfile limitations. Configuration never contains administrator credentials.

An ecosystem is presented as supported only after its real-client case has passed with public client egress blocked. Partial states identify the exact missing companion upstream or site-domain requirement instead of producing configuration that is known to bypass katch.

## Out of scope

- Package publication, login, upload, delete, private feeds, credential forwarding and npm audit-style POST services.
- Automatic creation of upstream records from metadata, redirects, mirrorlists or lockfiles.
- Transparent interception of arbitrary absolute URLs already stored in lockfiles.
- Composer 1 provider metadata, Composer source installs such as `--prefer-source`, DNF/YUM dynamic mirrorlist or metalink operation, and Homebrew `--build-from-source` downloads that Homebrew does not route through its artifact domain.
- Git LFS, recursive Git submodule URL rewriting and Homebrew cask application behavior beyond public artifact download.
- Changing signatures, package archives, checksum fields or signed repository indexes.

## Testing decisions

| Seam                                      | What it verifies                                                                                                          | Prior art                                                       |
| ----------------------------------------- | ------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------- |
| Adapter unit tests with protocol fixtures | Exact known-field rewriting, escaping, hashes, size limits, media types and fail-closed companion hosts                   | Existing proxy and cache service unit-test patterns             |
| Cache service tests                       | Accept variants, persisted headers, HEAD, conditions, ranges, protocol mutability and transformed ETag behavior           | `internal/service/cache_svc` tests                              |
| Origin/redirect tests                     | Registered-target enforcement, downgrade/userinfo rejection and dial-address validation                                   | Existing `internal/proxy/origin` tests                          |
| Admin API/repository tests                | Adapter persistence, validation, migration compatibility and API round trips                                              | Existing upstream service/repository tests with sqlmock         |
| Frontend tests                            | Adapter controls, translated guidance, invalid combinations and partial-configuration states                              | Existing Vitest component tests                                 |
| Real-client runtime harness               | Cold and warm installs with client public egress blocked, native checksum/signature verification and no nested direct URL | 2026-09-16 manual research in `docs/package-manager-mirrors.md` |

The runtime matrix requires every named client to pass: npm, pnpm, Yarn Classic, current Yarn Berry, Bun, pip, uv, current Poetry, Go, Maven, Gradle, sbt, Cargo, dotnet/NuGet, gem, Bundler, APT, DNF, YUM compatibility mode, Alpine apk, Composer 2 dist installation and Homebrew standard bottle installation. Missing host-platform clients run in a Linux container or CI runner using the same built katch binary; unavailability is a blocked verification result, not a skip. Docker, Podman and Git also run as regression cases for the shared cache and destination-safety changes.

Runtime clients execute in a network namespace or container whose firewall permits only the katch listener and loopback, while katch runs outside that namespace with origin access. DNS and rejected connection attempts are recorded; any attempted non-katch connection fails the case even if the client later succeeds. Proxy environment variables may supplement this boundary for diagnostics but never serve as the isolation mechanism.

Some public repositories change independently during a run. Automated protocol tests therefore own exact fields and cache decisions; runtime tests own client interoperability, blocked-egress behavior, cold/warm origin counts and native integrity checks without pinning mutable metadata bytes.

## Open questions

None.
