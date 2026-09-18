# 公开包镜像加速兼容性

本文说明 katch 对常见公开包仓库的适用边界，以及要做到完整镜像加速时需要的处理。

## 范围

只考虑公开仓库的只读操作：

- 查询包、版本和依赖元数据；
- 下载公开发布的制品；
- 校验公开制品的摘要或签名；
- 对元数据使用短 TTL，对不可变制品使用长期缓存。

不考虑发布、登录、上传、删除、私有仓库鉴权，以及 `npm audit` 这类需要 POST 的附加
服务。即使客户端把这些请求发给 katch，也不应为了兼容而把 static 上游变成可写代理。

## 什么算完整支持

“能请求到一个 URL”不等于完整支持。一个包管理器只有同时满足以下条件，才算完整支持：

1. 元数据查询和制品下载都经过 katch，客户端不会根据元数据里的绝对 URL 绕过 katch；
2. 元数据、不可变制品和可变快照使用各自正确的缓存策略；
3. 签名文件和制品正文不被改写，摘要、签名和 lockfile 校验保持有效；
4. GET、HEAD、Range、条件请求及内容协商满足客户端的只读下载流程；
5. 冷缓存可以回源，热缓存不再回源，并发拉取不会重复下载同一对象。

katch 的 generic static 协议适合“HTTP GET/HEAD + 同一仓库基地址 + 相对制品路径 +
同一 URL 不按未进入缓存键的请求头返回不同表示”的仓库；包管理器 profile 则可以声明
实际使用的 `Accept` 变体、结构化改写元数据中的已知 URL，并在目标主机已经登记为兼容
companion 时把绝对外链改回 katch。签名元数据和制品字节不改写。完整缓存对象保存
validator 与安全响应头，可以从本地回答 GET、HEAD、条件请求及单段/多段 Range；冷缓存
拿到的 partial 或 304/412/416 不会被误存成完整对象。重定向目标也必须通过已登记主机、
transport、HTTPS 降级和实际拨号地址检查。

## 当前实现、就绪判定与证据状态

上游记录可以选择包管理器 `package_profile`。选项来自后端实际注册的适配器，不是前端
硬编码；`none` 表示只走通用 static/registry/git 传输。非 `none` profile 必须开启
`static`。适配器已经覆盖以下只读范围：

| profile    | 客户端 / 范围                            | 必需 companion（transport / profile）                                                 |
| ---------- | ---------------------------------------- | ------------------------------------------------------------------------------------- |
| `npm`      | npm、pnpm、Yarn Classic、Yarn Berry、Bun | `registry.npmjs.org`（static / npm）                                                  |
| `pypi`     | pip、uv、Poetry                          | `files.pythonhosted.org`（static / pypi）                                             |
| `goproxy`  | Go modules + sumdb                       | `sum.golang.org`（static / goproxy）                                                  |
| `maven`    | Maven、Gradle、sbt                       | 无                                                                                    |
| `cargo`    | Cargo sparse index                       | `static.crates.io`（static / cargo）                                                  |
| `nuget`    | dotnet/NuGet restore、search             | `api.nuget.org`、`nuget.azure.cn`、`azuresearch-usnc.nuget.org`、`azuresearch-ea.nuget.org`、`azuresearch-sea.nuget.org`、`globalcdn.nuget.org`、`www.nuget.org`（均为 static / nuget） |
| `rubygems` | gem、Bundler                             | 无                                                                                    |
| `apt`      | APT 固定 source                          | 无                                                                                    |
| `rpm`      | DNF、YUM 固定 baseurl                    | 无                                                                                    |
| `apk`      | Alpine apk 固定 repository               | 无                                                                                    |
| `composer` | Composer 2 dist 安装                     | `api.github.com`、`codeload.github.com`（均为 static / composer）                      |
| `homebrew` | Homebrew 标准 Bottle 安装                | `ghcr.io`（registry / none）、`pkg-containers.githubusercontent.com`（static / none）、`github.com`（git / none） |

后端对每条 profile 上游返回 `package_readiness`，后台还能用未保存草稿请求同一套 preview。
只有以下条件全部满足时 `ready=true`：

1. `site_domain` 已配置，能派生出规范的公开 Base URL；
2. 主上游已启用并包含 `static` transport；
3. 每个 companion 主机都存在且启用，transport 与 profile 精确匹配。

缺项不会被合并成含糊的“不支持”：响应会给出主项 `site_domain`、`upstream_disabled`、
`static_transport`，或 companion 的确切 host 及 `missing`、`disabled`、`transport`、
`profile` 原因。katch 不会根据这些结果自动新增上游。

客户端配置由后端根据 profile、当前 host 与 `site_domain` 生成，包含尾斜杠、npm 旧
lockfile/registry-host 约束、Go 与 Homebrew no-fallback、DNF/YUM 固定 `baseurl` 并关闭
`mirrorlist`/`metalink`、Composer dist-only 等边界。Composer 的 Packagist 根元数据、
`api.github.com` dist 入口和重定向后的 `codeload.github.com` 下载都必须显式注册为
`static / composer`；GitHub 完整 commit SHA dist 才按不可变对象缓存，tag/ref 仍按可变或
未知路径处理。Homebrew 的 GHCR blob 会重定向到签名 CDN URL，因此必须显式注册精确主机
`pkg-containers.githubusercontent.com` 为 `static / none`；不会自动新增该记录，也不接受
通配的 `githubusercontent.com` 主机。前端只翻译和展示这些 DTO，不复制兼容规则。可复制
配置有两道同时生效的门：profile 必须有阻断公网 runtime matrix 证据，并且当前上游的
`package_readiness.ready` 必须为 `true`。2026-09-17 至 2026-09-18 在 `coding.local`
完成的矩阵覆盖全部 12 个 profile，冷缓存源站请求均大于 0、暖缓存源站请求均为 0，
连接审计没有发现非 katch 目标，因此这些 profile 的 `runtime_verified` 为 `true`；缺少
`site_domain`、主上游或 companion 时配置仍然保持锁定。

| runtime case | profile | 冷 / 暖源站请求 |
| --- | --- | ---: |
| npm-family | `npm` | 4 / 0 |
| pypi-family | `pypi` | 5 / 0 |
| goproxy | `goproxy` | 9 / 0 |
| maven-gradle-sbt | `maven` | 267 / 0 |
| cargo | `cargo` | 3 / 0 |
| nuget | `nuget` | 5 / 0 |
| rubygems-bundler | `rubygems` | 6 / 0 |
| apt-update-install-signature | `apt` | 3 / 0 |
| rpm-dnf-yum | `rpm` | 10 / 0 |
| apk | `apk` | 2 / 0 |
| composer-dist | `composer` | 3 / 0 |
| homebrew-bottle | `homebrew` | 7 / 0 |

共享回归用例还以 5 / 0 的冷暖源站请求完成了真实 Git clone、`git fsck` 和本地镜像应答
验证。该用例里的 Docker、Podman `version` 只用于记录“不可用”或“daemon 被阻断”的诊断，
没有执行镜像拉取，**不构成 Docker 或 Podman runtime 验证**。完整现场记录写在忽略目录
`.dev-kit/runtime/2026-09-16-public-package-mirrors/report.md`。

## 2026-09-16 真实客户端调研（实现前基线）

本轮在本机启动真实 katch 二进制，监听 `127.0.0.1:18081`。客户端环境只对
`127.0.0.1` 设置 `NO_PROXY`，其余 HTTP(S) 流量统一发往不可达的 `127.0.0.1:9`；
因此只要元数据让客户端下载公网 URL，用例就会在该 URL 处确定失败。所有临时配置、缓存
和工具链均放在 `/tmp`，没有修改生产代码或系统工具链。

| 客户端                            | 结果       | 观察                                                                                                                                                                                                                                                                                 |
| --------------------------------- | ---------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| npm 11.12.1                       | 条件通过   | 默认配置和 `replace-registry-host=never` 会按 packument 中的绝对 URL 直连 tarball；`replace-registry-host=always` 完整成功                                                                                                                                                           |
| pnpm 11.9.0、Yarn Classic 1.22.17 | 通过       | 设置 katch registry 后，解析和 tarball 下载均未绕过；生成的 lockfile 仍可能保留官方 URL，需单独验证旧 lockfile                                                                                                                                                                       |
| Bun 1.3.11                        | 失败       | packument 经 katch，随后直接请求 `registry.npmjs.org` 的 tarball，证明仍需服务端改写 `dist.tarball`                                                                                                                                                                                  |
| pip 26.0、uv 0.12.9               | 失败       | Simple API 经 katch，wheel 和 PEP 658 metadata 随即跳到 `files.pythonhosted.org`                                                                                                                                                                                                     |
| Go 1.26.3                         | 条件通过   | module `.info/.mod/.zip` 均成功；默认 checksum database 会直连 `sum.golang.org`，把它注册为独立 static 上游并显式设置 `GOSUMDB` 后完整成功                                                                                                                                           |
| Gradle 9.2.1                      | 通过       | 真实 `compileJava` 下载 POM、Gradle module metadata 和全部 JAR，并成功编译；所有请求只经过 katch 指定的 Maven Central base URL                                                                                                                                                       |
| RubyGems 3.0.3.1、Bundler 1.17.2  | 通过       | `gem install` 和 Bundler compact index 的 `/versions`、`/info/*`、`.gem` 下载完整成功                                                                                                                                                                                                |
| Composer 2.9.5                    | 失败       | 根 `packages.json` 的 `metadata-url` 仍指向 `repo.packagist.org`；后续 `dist.url` 还会指向 GitHub                                                                                                                                                                                    |
| NuGet / .NET 10.0.103             | 失败       | restore 从本地 Service Index 继续访问官方 `repository-signatures` URL；registration 中另有 `packageContent`、`catalogEntry` 绝对 URL                                                                                                                                                 |
| Cargo 1.98.1 sparse               | 失败       | index 和包条目经 katch 成功，`config.json` 的 `dl` 使 crate 直连 `static.crates.io`；把同一路径手工放到 katch 下可正常下载 immutable crate                                                                                                                                           |
| Homebrew 6.0.19                   | 条件通过   | `HOMEBREW_API_DOMAIN` 经 static 成功；`HOMEBREW_ARTIFACT_DOMAIN=http://<katch>/registry/ghcr.io` 为 Homebrew 自行追加的 `/v2/...` 保留明确边界，并以 `HOMEBREW_ARTIFACT_DOMAIN_NO_FALLBACK=1` 禁止回退，可让 jq 的 manifest/blob 全部复用 registry 适配器并通过 checksum；普通源码 URL 不会被 Homebrew 6 改写，`--build-from-source` 仍绕过 |
| APT / Alpine APK / RPM baseurl    | 传输层通过 | Debian `InRelease`、Alpine `APKINDEX.tar.gz`、CentOS Stream `repomd.xml` 首次 MISS、第二次 HIT，缓存体与源站 SHA-256 一致；本机没有相应原生客户端，尚不能记为客户端端到端通过                                                                                                        |

调研还复现了两个通用缓存问题：

- npm 同一路径的 install-v1 packument 为 8,488 字节，完整 JSON 为 22,573 字节；PyPI
  同一路径的 HTML 为 4,305 字节，JSON 为 5,712 字节。当前 static 缓存键不含
  `Accept`，后来的请求会命中先写入的错误表示及 Content-Type。
- npm 等客户端会请求 gzip 元数据。当前 katch 为避免丢失 `Content-Encoding` 而不缓存
  编码响应，因此真实客户端的元数据会反复 MISS。元数据适配器需要向上游请求 identity
  表示，或扩充缓存记录并完整保存编码相关响应头；前者更适合需要解析正文的适配器。

## 调研时的兼容性与所需处理（历史基线）

下表记录上述调研时发现的缺口，用来解释各 profile 为什么需要当前实现；表中的“需要
增加适配器”等措辞不是当前代码状态。实现是否存在看上一节，完整支持的运行时证据看
上一节已完成的阻断公网矩阵。

| 生态 / 客户端           | 公开仓库特点与当前边界                                                                                                                                                                                             | 要做到完整支持需要的处理                                                                                                                                                                                             | 缓存边界                                                                                                                                               | 建议优先级 / 工作量         |
| ----------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------ | --------------------------- |
| npm / pnpm / Yarn / Bun | packument 的 `dist.tarball` 以及 lockfile 可能包含 `registry.npmjs.org` 绝对 URL。服务端改写 packument 只影响后续生成的解析结果，不能拦截客户端直接读取既有 lockfile 后发出的请求                                  | 增加 npm 元数据适配器，结构化解析 packument 并改写 `dist.tarball`，保留 `integrity`，覆盖 scoped package、路径转义和不同 `Accept`；客户端模板设置 registry host 替换；既有 lockfile 需要迁移或重新生成               | packument、dist-tag 短 TTL；按版本发布的 `.tgz` 长期缓存                                                                                               | P0 / 中                     |
| pip / uv / Poetry       | PyPI Simple API 会把制品链接指向 `files.pythonhosted.org`，只代理 `pypi.org/simple` 会被绕过                                                                                                                       | 同时支持 PEP 503 HTML 与 PEP 691 JSON；结构化改写文件 URL；保留 `#sha256=` fragment 和 `data-*` 属性；要求目标文件域名已注册为上游                                                                                   | `/simple/` 元数据短 TTL；wheel、sdist 按哈希长期缓存                                                                                                   | P0 / 中                     |
| Go Modules              | `GOPROXY` 是稳定的只读协议，版本文件路径固定；Go 会先探测每个 proxy 的 `/sumdb/<name>/supported`，katch 当前把该路径原样交给 `proxy.golang.org` 并得到 404，随后客户端直连 checksum database                       | 增加 Go 适配器：模块文件继续走 static；对已注册且允许的 sumdb 主机响应 `supported`，并把同前缀下的 lookup/tile 请求桥接到对应 sumdb 上游。短期也可显式配置 `GOSUMDB='sum.golang.org https://<katch>/sum.golang.org'` | 固定版本的 `.zip`、`.mod`、`.info` 与 sumdb lookup/tile 可长期缓存；`@v/list`、`@latest` 和 sumdb `/latest` 保持可变，不能把整个 `/@v/` 一概设为不可变 | P0 / 中                     |
| Maven / Gradle / sbt    | Maven 仓库通常在同一基地址下使用相对路径；release、SNAPSHOT、校验文件的可变性不同                                                                                                                                  | static 传输路径基础可用；验证 GET、HEAD、Range 和条件请求，并保存命中所需的校验头；提供 repository 配置模板；逐字节转发 POM、校验及签名文件；多个仓库逐一配置，不能用一个地址猜原始主机                              | 确认仓库禁止覆盖的 release 或含强摘要路径可长期缓存；`maven-metadata.xml` 与 SNAPSHOT 短 TTL                                                           | P1 / 中                     |
| Cargo                   | sparse index 的 `config.json` 包含 `dl` 绝对地址，crate 通常位于 `static.crates.io`；Git index 中的同一文件受 Git 对象哈希保护，不能就地改写                                                                       | 完整支持优先限定为 sparse index：增加 Cargo 适配器，改写 `config.json` 的 `dl`，支持相应内容类型和路径，并要求 crate 下载主机已注册；现有 Git 适配器只能加速 index clone/fetch，不能解决 crate 下载绕过              | sparse index 短 TTL；`.crate` 按版本和 checksum 长期缓存                                                                                               | P1 / 中                     |
| NuGet                   | V3 Service Index 的 `@id` 会继续指向 registration、flat-container、search 等绝对端点，registration JSON 中还有下一层 URL                                                                                           | 解析并改写 Service Index 和 registration JSON 的已知 URL 字段；保留分页关系；支持内容编码和版本化资源类型；所有目标域名必须预先注册                                                                                  | service index、registration 和 search 短 TTL；`.nupkg` 长期缓存                                                                                        | P1 / 大                     |
| RubyGems / Bundler      | Gem 下载本身较直接，但 Compact Index 依赖 ETag、Range 和增量获取                                                                                                                                                   | 验证 `/versions`、`/info/*`、`/gems/*`；让缓存命中保留校验头并正确处理 Range 和条件请求；提供 Bundler mirror 配置模板                                                                                                | compact index 短 TTL；确认仓库禁止覆盖的 `.gem` 或含强摘要对象长期缓存                                                                                 | P1 / 中                     |
| APT                     | 索引与包路径适合 static，但 `InRelease`、`Release` 等内容受签名保护                                                                                                                                                | 现有 static 传输路径适用；禁止改写响应体；提供 source 配置模板；为 `/pool/`、`/by-hash/` 等路径显式配置不可变规则                                                                                                    | `/pool/` 与 `by-hash` 长期缓存；`dists` 入口元数据短 TTL                                                                                               | P0 / 小                     |
| DNF / YUM               | 直接 `baseurl` 通常使用相对路径；mirrorlist 和 metalink 会把客户端引向其他镜像                                                                                                                                     | 推荐配置 katch `baseurl` 并关闭 mirrorlist/metalink；若要支持动态镜像列表，需要专用适配器且不能改写签名覆盖的元数据                                                                                                  | 含强摘要或仓库规范保证不可覆盖的 RPM/repodata 可长期缓存；其余对象使用 TTL 或条件请求                                                                  | P1 / 小到中                 |
| Alpine APK              | APKINDEX 和包通常位于同一个仓库基地址，索引带签名                                                                                                                                                                  | static 模式基础可用；保持 APKINDEX 原文；验证客户端实际使用的条件请求和 Range                                                                                                                                        | `.apk` 长期缓存；APKINDEX 短 TTL                                                                                                                       | P1 / 小                     |
| Composer                | Packagist v2 元数据和既有 lockfile 的 `dist` URL 常指向 GitHub 等外部域名，目标集合比单一仓库更开放                                                                                                                | 只改写 `metadata-url` 模板和包元数据中的 `dist.url`，且仅接受已注册目标域名；完整支持限定为 Composer 2 dist 安装；既有 lockfile 需要迁移，`--prefer-source` 不在本轮支持范围                                         | provider/package 元数据短 TTL；只有完整 commit SHA 或强摘要标识的 dist 才长期缓存，tag/branch 等 ref 按可变对象处理                                    | P2 / 大                     |
| Homebrew                | API、Bottle、Tap 和源码分属多个域名；Bottle 使用 OCI registry，API 的 JWS 数据不能为了改 URL 而破坏签名；Homebrew 6 的实际下载策略只对 GHCR Bottle 应用 artifact domain，普通 formula 源码不会按环境变量说明加前缀 | 保持 API/JWS 原文；Bottle 使用 `<katch>/registry/ghcr.io` 作为会由 Homebrew 追加 `/v2` 的显式 registry base，复用 registry 适配器并强制 `HOMEBREW_ARTIFACT_DOMAIN_NO_FALLBACK=1`；Tap 复用 Git 适配器；完整支持限定为标准 Bottle 安装，不宣称 `--build-from-source` 被镜像 | OCI blob 与 digest manifest 长期缓存，tag manifest 与 API 数据可变                                                                                     | P2 / 中                     |
| Docker / Podman         | Registry 需要 token 交换，并区分 tag manifest、digest manifest 和 blob                                                                                                                                             | 现有 registry 适配器已覆盖主要只读路径；继续用真实客户端验证多架构 manifest、Range 与 token 交换                                                                                                                     | blob、digest manifest 长期缓存；tag manifest 短 TTL                                                                                                    | 已支持，持续验收            |
| Git                     | smart HTTP clone/fetch 使用协商式 POST，响应不能作为普通静态对象复用；Git LFS 是另一套 API，`.gitmodules` 也可能包含绝对子模块 URL                                                                                 | 现有 Git 适配器负责 smart HTTP upload-pack 的只读穿透与本地镜像；保持 push 拒绝；明确不含 Git LFS；递归子模块需要客户端 URL rewrite 或分别配置                                                                       | 本地镜像按 refs TTL 更新；协商响应不进入普通对象缓存                                                                                                   | smart HTTP 已支持，持续验收 |

## 通用实现要求

| 能力               | 处理方式                                                                                                                                                                                                 |
| ------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 协议适配器         | 适配器按协议和路径处理元数据，不在 generic static 代理中加入 `if host == ...`；元数据转换位于回源响应与缓存之间，缓存的是转换后的表示；只让明确需要协议语义的生态新增适配器                              |
| 受控 URL 改写      | 只解析和改写协议定义中的已知字段；目标主机必须已存在于启用的上游白名单，绝不根据元数据动态创建上游或代理任意 URL                                                                                         |
| 对外基地址         | 优先生成客户端可解析的相对 URL；协议要求绝对 URL 时使用 `site_domain` 站点设置派生出的 Base URL，不能根据未受信任的 Host 或转发头拼接；未配置 `site_domain` 时拒绝启用这类改写                           |
| 结构化解析         | JSON 使用 JSON parser，PyPI HTML 使用 HTML parser；不得做字符串替换；APT、APK、签名文件和制品正文不得改写                                                                                                |
| 元数据缓冲上限     | 只缓冲需要改写且尺寸受限的元数据，例如上限 16 MiB；tarball、wheel、crate、RPM、镜像层等制品继续流式转发                                                                                                  |
| 改写后的响应头     | 响应体发生变化后移除上游 ETag，并移除或重算 `Content-Length`、`Content-Encoding`；需要条件请求时生成 katch 表示自己的校验符；制品的摘要、签名和 PyPI hash fragment 保持原值                              |
| 内容协商与缓存变体 | 缓存键包含协议真正使用的 `Accept` 等维度，避免把 PyPI HTML 返回给请求 JSON 的客户端；正确处理 `Vary`                                                                                                     |
| 下载请求语义       | 通用只读路径支持 GET、HEAD、Range、`If-None-Match`、`If-Modified-Since`、`If-Range`；部分响应和 304 不得作为完整对象写入缓存；要从缓存响应条件请求，必须持久化并恢复必要校验头，当前 static 缓存尚未做到 |
| 重定向约束         | 当前回源客户端会直接跟随重定向，尚未校验目标主机或地址；需要为下载重定向和 registry token realm 增加允许主机、公网 IP、DNS 重绑定检查，防止公开上游把服务端引向内网地址                                  |
| 缓存分类           | 每个适配器明确区分可变入口、可变索引和不可变制品；只有协议保证不可覆盖、路径含强摘要或对象身份纳入元数据摘要时才长期缓存；非标准路径继续使用 `immutable_patterns`                                        |
| 客户端配置模板     | 管理界面和文档为每个生态给出可直接使用的配置，明确尾斜杠、路径前缀和需要注册的附属下载域名                                                                                                               |

## 验收方法

每个宣称支持的客户端至少通过以下端到端用例：

1. 在客户端侧阻断所有公网访问，只允许访问 katch，确认解析和下载没有绕过；
2. 空缓存执行一次真实安装或下载，确认元数据和制品都能完成校验；
3. 再执行一次相同操作，确认不可变制品命中缓存且源站不再收到请求；
4. 更新可变元数据，确认 TTL 或条件请求后能看到新版本；
5. 使用已有 lockfile 重建依赖；若其中存在绝对 URL，确认客户端 URL 映射或迁移步骤生效，
   不把服务端元数据改写误当成既有 lockfile 的解决方案；
6. 并发请求同一制品，确认只发生一次回源；
7. 对带签名或摘要的生态执行原生校验，确认代理没有改变受保护的字节。

当前各 profile 的适配器、缓存分类、结构化改写和 runtime case 已实现并由协议级测试
覆盖；`coding.local` 上的全量阻断公网矩阵进一步验证了真实客户端、冷/暖缓存、源站计数、
签名/摘要以及没有非 katch 连接。运行时证据只打开 `runtime_verified` 这一道门；部署现场
仍必须让 `site_domain`、主上游和全部 companion 满足 readiness，界面才会开放配置片段。
Docker、Podman 不在本次通过项内；Git 的真实 clone 回归已通过。
