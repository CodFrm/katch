# 静态资源长期缓存策略与本地条件请求

> Status: Approved
> Owner: katch
> Last updated: 2026-09-16

**Objective:** 让运维者能从管理界面明确配置静态上游的内容寻址路径，并让持有新鲜缓存副本的 katch 在具备上游校验信息时本地回答 GET/HEAD 条件请求，避免不会改变的资源反复回源。

**Hard invariant:** 不得根据未经声明的 URL 外观猜测静态资源永久不可变；不得为可变资源返回伪造的 `304`，也不得改写上游响应体。

## Problem

1. **静态资源的长期缓存策略无法在管理界面配置。** 上游接口和数据模型已有 `immutable_patterns` 与 `mutable_ttl_seconds`，但当前上游表单只保留这两个隐藏字段，新建记录固定使用空模式和 300 秒 TTL。通过界面登记的 APT、Go proxy、GitHub raw、PyPI 等静态上游因此会把内容寻址资源当作可变对象，TTL 到期后再次回源。
2. **条件请求无条件绕过对象缓存。** 当前缓存入口看到 `If-None-Match` 或 `If-Modified-Since` 就直接透传，即使本地持有未过期的完整副本；经常重验证的客户端因此不能从服务端缓存受益。
3. **现有 Go proxy 示例模式过宽。** `"/@v/"` 同时匹配版本文件和会变化的 `@v/list`，会把后者永久缓存，与 Go module proxy 的新鲜度要求相反。
4. **现有命中检查示例使用 HEAD。** 当前 HEAD 直接透传，README 中的 `curl -I` 不能证明对象缓存是否命中，容易把正常行为判断成持续回源。

## Actors and user stories

1. 作为运维者，我希望在添加或编辑静态上游时选择常见缓存策略并检查实际模式，这样内容寻址资源能长期缓存，而可变索引仍按 TTL 更新。
2. 作为资源使用者，我希望对一个已有新鲜副本发起普通 GET、HEAD 或条件 GET 时由 katch 本地回答，这样重复验证不会继续消耗回源连接与流量。
3. 作为运维者，我希望响应上的 `X-Katch-Cache` 能区分本地回答与真实回源，这样可以直接验证策略是否生效。

## Design decisions

| # | Decision | Basis and rejected option |
|---|---|---|
| 1 | registry 标准端点继续按协议判断可变性，静态资源继续由每条上游的模式列表声明 | 把一段看似 hash 的字符自动判为永久对象会把误判固化。否决：通用 hash 猜测——无法证明任意 URL 中的十六进制段就是内容地址。 |
| 2 | 常见策略是表单预设，只填充普通 `immutable_patterns`，不形成新的后端枚举或 host 分支 | 上游记录仍是唯一真相，新上游无需发版即可配置。否决：后端按已知 host 自动识别——新增镜像站要改代码，且同一 host 可能承担不同用途。 |
| 3 | 条件请求只在缓存副本新鲜且具备对应 validator 时本地求值 | 这样减少回源仍以 HTTP 校验事实为依据。否决：忽略条件头直接返回缓存 `200`，或没有 validator 也猜测 `304`——都会破坏调用方可观察到的协议语义。 |
| 4 | Range 与 If-Range 继续透传 | 正确的范围响应需要解析多种 byte range、生成 `206`/`416` 并处理 If-Range，不与条件重验证混在本轮。 |
| 5 | 历史静态缓存不补造 validator | 上游原始 ETag 与内容摘要未必是同一个标识。否决：把 katch 摘要冒充历史上游 ETag——客户端可能拿着另一串 validator，得到错误结果。 |

## 缓存策略表单

添加和编辑上游时展示“缓存策略”菜单，选项为“自定义”、“APT 软件包”、“Go Module Proxy”、“Git commit 静态文件”和“PyPI 文件”。菜单旁展示逐行编辑的不可变路径模式，以及可变对象 TTL 数字输入。

选择预设只修改当前表单草稿中的模式列表；使用者随后可以继续编辑。保存时只提交最终的 `immutable_patterns` 和 `mutable_ttl_seconds`，不提交预设名称。编辑已有上游时，模式与某个预设逐项完全相同才显示该预设，否则显示“自定义”。未来调整预设不得静默改变已经保存的上游。

预设填充值如下：

| 预设 | `immutable_patterns` | 保持可变的典型路径 |
|---|---|---|
| APT 软件包 | `["/pool/"]` | `InRelease`、`Release`、`Packages*` |
| Go Module Proxy | `["/@v/*.info", "/@v/*.mod", "/@v/*.zip"]` | `@v/list`、`@latest` |
| Git commit 静态文件 | `["/????????????????????????????????????????/"]` | 分支名、tag 名路径 |
| PyPI 文件 | `["/packages/??/??/????????????????????????????????????????????????????????????????/"]` | 项目索引与元数据 |
| 自定义 | 不替换当前模式；新建上游初始为空 | 由最终模式决定 |

模式编辑器按一行一个模式显示。保存前去掉空行、每行首尾空白与重复项，同时保留首次出现的顺序。TTL 为非负整数；`0` 继续表示使用全局默认 TTL。

预设不按 host 自动套用。运维者必须显式选择，以避免同名服务或兼容镜像被错误归类。registry 的 blob 与 digest manifest 不要求选择这些静态预设。

## Validator 状态

每条新写入或重写的缓存记录保存上游成功响应中的 `ETag` 与 `Last-Modified`；上游未提供时保存为空。缓存命中返回 `200` 或 HEAD 元数据时，同时回放已有的这两个头。

数据库变更只追加迁移。已有记录保持原内容、键、TTL、不可变状态和访问统计，新增 validator 字段为空。registry 对象可继续使用已经由内容 digest 推导的 ETag；不得为历史静态对象推导或伪造上游 validator。

同一路径重新回源并写入新内容时，validator 与内容记录一起覆盖，不能保留上一份内容的校验值。直接写缓存但没有上游响应头的管理/内部路径继续留下空 validator。

## 本地请求处理

普通 GET 的现有行为不变：新鲜副本返回本地 `200`，未命中或过期才回源并写缓存。

HEAD 在存在新鲜完整副本时由本地返回与 GET 相同的状态和元数据，但不发送响应体，并设置 `X-Katch-Cache: HIT`。没有可用副本时 HEAD 继续透传且不创建缓存记录。

带 `If-None-Match` 的 GET 或 HEAD 优先按已保存或 registry 可推导的 ETag 求值。GET/HEAD 使用弱比较语义，并支持逗号分隔的 tag 列表与 `*`：任一项匹配时返回本地 `304`，不发送响应体；均不匹配时返回本地缓存的 `200`。请求值语法无效时按未提供该条件处理。

仅在没有 `If-None-Match` 时求值 `If-Modified-Since`。缓存记录有可解析的 `Last-Modified` 且资源未晚于请求时间时返回本地 `304`；资源较新时返回本地 `200`。请求日期无效时按未提供该条件处理。

有效条件存在但缓存记录缺少对应 validator 时，本次请求继续透传上游。历史静态记录因此不会得到推测结果；该对象将来正常重写后才具备本地条件请求能力。

本地 `200`、`304` 和 HEAD 都设置 `X-Katch-Cache: HIT`。`304` 不带响应体，也不声明缓存对象的实体长度。缓存未命中、已过期、损坏或 validator 不足而发生的真实回源仍按现有指标与响应头归为未命中。

带 `Range` 或 `If-Range` 的请求继续完整透传，不读取或写入对象缓存。git 协商请求继续使用 git 镜像/穿透路径，不进入对象缓存。

## 失败与恢复

validator 写库失败与现有缓存记录写入失败采用相同降级：当前响应仍完成，但该对象不能被当作一次成功持久化。读取历史空 validator、无法解析保存的日期或无法判断条件时，不返回猜测结果，而是回源。

本地副本完整性校验、TTL、LRU、pin、配额和并发回源合并规则保持不变。不可变表示“不因时间过期”，不表示绝不淘汰；超配额时仍可按 LRU 收走未 pin 对象。

## 文档与可观测性

README 的命中示例改为真正读取 GET 响应而不是 `curl -I`。文档说明 registry digest 自动识别、静态资源由缓存策略/模式声明，以及 `Range` 仍会回源。

运维验证以 `X-Katch-Cache` 为准：首次完整 GET 为 `MISS`，后续普通 GET、HEAD 或可本地求值的条件请求为 `HIT`。条件命中的 HTTP 状态可以是 `304`，它仍表示本次没有回源。

## Out of scope

- 根据任意 URL 中的 hash 外观自动判定静态资源不可变。
- 在后端按 docker.io、proxy.golang.org、GitHub、PyPI 等具体 host 写分支。
- 从历史静态缓存内容反推或批量补齐上游 ETag、Last-Modified。
- 本地处理单段、多段或后缀 Range，以及 If-Range。
- 用上游 `Cache-Control: max-age` 动态覆盖上游记录或全局 TTL。
- 可变对象过期后的条件回源 `304` 自动续期；本轮仍由既有未命中路径处理过期对象。

## Testing decisions

| Seam | What it verifies | Prior art |
|---|---|---|
| 缓存策略表单 | 预设填充、可继续编辑、TTL 写回、自定义模式归一化、已有配置不被自动覆盖 | 模式语义由 `internal/cache/pattern_test.go` 覆盖；表单交互（预设填充与归一化）暂无前端自动化用例 |
| 模式判定 | Go 版本文件命中而 `@v/list`、`@latest` 不命中；各预设的代表路径与反例 | `internal/cache/pattern_test.go` |
| 缓存记录迁移 | 新旧数据库均得到 validator 字段，已有缓存行内容和状态不变；sqlite 与 MySQL 兼容的 DDL 形状 | `migrations/migrations_test.go`、缓存迁移测试 |
| 缓存写入与命中 | 上游 validator 随内容保存、覆盖并在本地响应回放 | `internal/service/cache_svc/cache_test.go` |
| 条件请求 | ETag 弱比较、列表、`*`、If-None-Match 优先级、If-Modified-Since 日期、匹配 304、不匹配 200、缺 validator 透传 | `internal/service/cache_svc/cache_test.go` |
| HEAD 与 Range | 新鲜缓存的 HEAD 本地命中且无响应体；无副本 HEAD 与所有 Range/If-Range 继续透传 | `internal/service/cache_svc/cache_test.go`、`internal/web/cache_test.go` |
| HTTP 端到端 | 经真实 NoRoute 拉取后，第二次 GET、HEAD 和条件请求可观察到正确状态与 `X-Katch-Cache` | `internal/web/cache_test.go` |
| 启动与迁移 | 带新增迁移的真实二进制能够启动、拉取、命中并干净退出 | `make smoke` |

自动化无法证明任意第三方上游遵守所选预设的不可变约定；这是运维选择预设时承担的上游契约。收尾时使用公开测试上游做小对象的 `MISS -> HIT` 观察，不修改线上配置，也不以该观察替代自动化测试。

## Open questions

（无）
