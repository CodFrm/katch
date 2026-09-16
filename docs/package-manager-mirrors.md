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

katch 当前的 static 协议适合“HTTP GET/HEAD + 同一仓库基地址 + 相对制品路径 +
同一 URL 不按未进入缓存键的请求头返回不同表示”的仓库。它会跟随上游 HTTP 重定向，
但不会改写 JSON、HTML 或 XML 响应体中的绝对 URL。因此，元数据里只要出现另一个
下载域名，客户端就仍可能直连那个域名。当前 HEAD、Range 和条件请求会穿透上游且
不写入缓存；static 缓存命中也不会恢复上游的 ETag 等校验头，所以“传输可用”和
“完整缓存语义”需要分别判断。

## 兼容性与所需处理

| 生态 / 客户端 | 公开仓库特点与当前边界 | 要做到完整支持需要的处理 | 缓存边界 | 建议优先级 / 工作量 |
| --- | --- | --- | --- | --- |
| npm / pnpm / Yarn / Bun | packument 的 `dist.tarball` 以及 lockfile 可能包含 `registry.npmjs.org` 绝对 URL。服务端改写 packument 只影响后续生成的解析结果，不能拦截客户端直接读取既有 lockfile 后发出的请求 | 增加 npm 元数据适配器，结构化解析 packument 并改写 `dist.tarball`，保留 `integrity`，覆盖 scoped package、路径转义和不同 `Accept`；客户端模板设置 registry host 替换；既有 lockfile 需要迁移或重新生成 | packument、dist-tag 短 TTL；按版本发布的 `.tgz` 长期缓存 | P0 / 中 |
| pip / uv / Poetry | PyPI Simple API 会把制品链接指向 `files.pythonhosted.org`，只代理 `pypi.org/simple` 会被绕过 | 同时支持 PEP 503 HTML 与 PEP 691 JSON；结构化改写文件 URL；保留 `#sha256=` fragment 和 `data-*` 属性；要求目标文件域名已注册为上游 | `/simple/` 元数据短 TTL；wheel、sdist 按哈希长期缓存 | P0 / 中 |
| Go Modules | `GOPROXY` 是稳定的只读协议，版本文件路径固定；checksum database 可经 proxy 的 `/sumdb/` 路径访问 | 现有 static 传输路径基本适用，但不可变性仍依赖配置；持续保证模块路径的 `!` 转义、查询串和 `/sumdb/` 原样传递，并补真实 `go mod download` 验收 | 固定版本的 `.zip`、`.mod`、`.info` 可长期缓存；`@v/list`、`@latest` 和 sumdb 最新状态保持可变，不能把整个 `/@v/` 一概设为不可变 | P0 / 小到中 |
| Maven / Gradle / sbt | Maven 仓库通常在同一基地址下使用相对路径；release、SNAPSHOT、校验文件的可变性不同 | static 传输路径基础可用；验证 GET、HEAD、Range 和条件请求，并保存命中所需的校验头；提供 repository 配置模板；逐字节转发 POM、校验及签名文件；多个仓库逐一配置，不能用一个地址猜原始主机 | 确认仓库禁止覆盖的 release 或含强摘要路径可长期缓存；`maven-metadata.xml` 与 SNAPSHOT 短 TTL | P1 / 中 |
| Cargo | sparse index 的 `config.json` 包含 `dl` 绝对地址，crate 通常位于 `static.crates.io`；Git index 中的同一文件受 Git 对象哈希保护，不能就地改写 | 完整支持优先限定为 sparse index：增加 Cargo 适配器，改写 `config.json` 的 `dl`，支持相应内容类型和路径，并要求 crate 下载主机已注册；现有 Git 适配器只能加速 index clone/fetch，不能解决 crate 下载绕过 | sparse index 短 TTL；`.crate` 按版本和 checksum 长期缓存 | P1 / 中 |
| NuGet | V3 Service Index 的 `@id` 会继续指向 registration、flat-container、search 等绝对端点，registration JSON 中还有下一层 URL | 解析并改写 Service Index 和 registration JSON 的已知 URL 字段；保留分页关系；支持内容编码和版本化资源类型；所有目标域名必须预先注册 | service index、registration 和 search 短 TTL；`.nupkg` 长期缓存 | P1 / 大 |
| RubyGems / Bundler | Gem 下载本身较直接，但 Compact Index 依赖 ETag、Range 和增量获取 | 验证 `/versions`、`/info/*`、`/gems/*`；让缓存命中保留校验头并正确处理 Range 和条件请求；提供 Bundler mirror 配置模板 | compact index 短 TTL；确认仓库禁止覆盖的 `.gem` 或含强摘要对象长期缓存 | P1 / 中 |
| APT | 索引与包路径适合 static，但 `InRelease`、`Release` 等内容受签名保护 | 现有 static 传输路径适用；禁止改写响应体；提供 source 配置模板；为 `/pool/`、`/by-hash/` 等路径显式配置不可变规则 | `/pool/` 与 `by-hash` 长期缓存；`dists` 入口元数据短 TTL | P0 / 小 |
| DNF / YUM | 直接 `baseurl` 通常使用相对路径；mirrorlist 和 metalink 会把客户端引向其他镜像 | 推荐配置 katch `baseurl` 并关闭 mirrorlist/metalink；若要支持动态镜像列表，需要专用适配器且不能改写签名覆盖的元数据 | 含强摘要或仓库规范保证不可覆盖的 RPM/repodata 可长期缓存；其余对象使用 TTL 或条件请求 | P1 / 小到中 |
| Alpine APK | APKINDEX 和包通常位于同一个仓库基地址，索引带签名 | static 模式基础可用；保持 APKINDEX 原文；验证客户端实际使用的条件请求和 Range | `.apk` 长期缓存；APKINDEX 短 TTL | P1 / 小 |
| Composer | Packagist 元数据和既有 lockfile 的 `dist`、`source` 常指向 GitHub 等外部域名，目标集合比单一仓库更开放 | 只改写元数据中的已知字段且仅接受已注册的目标域名；dist 下载走 static；source 下载复用 Git 适配器；既有 lockfile 需要迁移或客户端 URL 映射，未知外链不能动态创建上游 | provider/package 元数据短 TTL；只有完整 commit SHA 或强摘要标识的 dist 才长期缓存，tag/branch 等 ref 按可变对象处理 | P2 / 大 |
| Homebrew | API、Bottle、Tap 和源码分属多个域名；Bottle 还可能使用 OCI registry，formula 中的源码 URL 也可能绕过 | 提供并验证 `HOMEBREW_API_DOMAIN`、`HOMEBREW_BOTTLE_DOMAIN`、`HOMEBREW_ARTIFACT_DOMAIN` 和 Git remote 配置；Bottle 复用 registry 适配器，Tap 复用 Git 适配器；不能由客户端映射的源码需要改写非签名元数据 | OCI blob 与 digest manifest 长期缓存，tag manifest 可变；源码只有含强摘要或完整 commit SHA 时长期缓存 | P2 / 中到大 |
| Docker / Podman | Registry 需要 token 交换，并区分 tag manifest、digest manifest 和 blob | 现有 registry 适配器已覆盖主要只读路径；继续用真实客户端验证多架构 manifest、Range 与 token 交换 | blob、digest manifest 长期缓存；tag manifest 短 TTL | 已支持，持续验收 |
| Git | smart HTTP clone/fetch 使用协商式 POST，响应不能作为普通静态对象复用；Git LFS 是另一套 API，`.gitmodules` 也可能包含绝对子模块 URL | 现有 Git 适配器负责 smart HTTP upload-pack 的只读穿透与本地镜像；保持 push 拒绝；明确不含 Git LFS；递归子模块需要客户端 URL rewrite 或分别配置 | 本地镜像按 refs TTL 更新；协商响应不进入普通对象缓存 | smart HTTP 已支持，持续验收 |

## 通用实现要求

| 能力 | 处理方式 |
| --- | --- |
| 协议适配器 | 适配器按协议和路径处理元数据，不在 generic static 代理中加入 `if host == ...`；元数据转换位于回源响应与缓存之间，缓存的是转换后的表示；只让明确需要协议语义的生态新增适配器 |
| 受控 URL 改写 | 只解析和改写协议定义中的已知字段；目标主机必须已存在于启用的上游白名单，绝不根据元数据动态创建上游或代理任意 URL |
| 对外基地址 | 优先生成客户端可解析的相对 URL；协议要求绝对 URL 时使用 `site_domain` 站点设置派生出的 Base URL，不能根据未受信任的 Host 或转发头拼接；未配置 `site_domain` 时拒绝启用这类改写 |
| 结构化解析 | JSON 使用 JSON parser，PyPI HTML 使用 HTML parser；不得做字符串替换；APT、APK、签名文件和制品正文不得改写 |
| 元数据缓冲上限 | 只缓冲需要改写且尺寸受限的元数据，例如上限 16 MiB；tarball、wheel、crate、RPM、镜像层等制品继续流式转发 |
| 改写后的响应头 | 响应体发生变化后移除上游 ETag，并移除或重算 `Content-Length`、`Content-Encoding`；需要条件请求时生成 katch 表示自己的校验符；制品的摘要、签名和 PyPI hash fragment 保持原值 |
| 内容协商与缓存变体 | 缓存键包含协议真正使用的 `Accept` 等维度，避免把 PyPI HTML 返回给请求 JSON 的客户端；正确处理 `Vary` |
| 下载请求语义 | 通用只读路径支持 GET、HEAD、Range、`If-None-Match`、`If-Modified-Since`、`If-Range`；部分响应和 304 不得作为完整对象写入缓存；要从缓存响应条件请求，必须持久化并恢复必要校验头，当前 static 缓存尚未做到 |
| 重定向约束 | 当前回源客户端会直接跟随重定向，尚未校验目标主机或地址；需要为下载重定向和 registry token realm 增加允许主机、公网 IP、DNS 重绑定检查，防止公开上游把服务端引向内网地址 |
| 缓存分类 | 每个适配器明确区分可变入口、可变索引和不可变制品；只有协议保证不可覆盖、路径含强摘要或对象身份纳入元数据摘要时才长期缓存；非标准路径继续使用 `immutable_patterns` |
| 客户端配置模板 | 管理界面和文档为每个生态给出可直接使用的配置，明确尾斜杠、路径前缀和需要注册的附属下载域名 |

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

建议按 npm、PyPI、通用下载语义、Cargo、NuGet 的顺序补齐。Maven、APT、DNF 和 APK
优先完善配置模板与真实客户端验收；它们不应仅为了“看起来分类完整”就各自新增适配器。
