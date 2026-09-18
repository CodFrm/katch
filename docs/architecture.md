# 架构

## 分层

依赖只能单向流动，反向引用一律不允许：

```
app/controller  →  service  →  repository  →  model/entity
```

- **controller**（`internal/controller/<domain>_ctr/`）：薄层。校验入参、转发给 service，
  不写业务逻辑。
- **service**（`internal/service/<domain>_svc/`）：业务逻辑。只依赖 repository 的**接口**，
  通过 `Register`/accessor 注入实现（DIP）。
- **repository**（`internal/repository/<domain>_repo/`）：数据访问，用 `db.Ctx(ctx)` 查询。
- **entity**（`internal/model/entity/<domain>_entity/`）：充血模型。存在性检查、状态校验、
  字段格式化这类只依赖自身的规则写在实体上；跨实体协调和依赖外部服务的逻辑放 service。

`internal/api/` 只放请求/响应结构与路由注册，不含逻辑。按面分子包：
`internal/api/admin/` 是需要密钥的管理接口，公开接口另开子包。
横切层（`pkg/`）不得反向引用 service / repository。

service 的方法直接收发 `internal/api/` 里的结构体（cago 的惯例），因此
**api 子包不许反向 import service**，否则会构成循环。路由鉴权中间件之所以写在
`internal/api/router.go` 里而不是单开一个包，就是这个原因：`internal/api` 自身
没有任何包反向引用它，把「要调 service」的那点代码放在这里不会闭环。

新领域开新的一组包（`<domain>_entity` / `_repo` / `_svc`），不要往已有领域里塞。

## 路由命名空间

katch 的 HTTP 路径被两类完全不同的东西共用，必须分清：

| 路径                | 归属                                                                      |
| ------------------- | ------------------------------------------------------------------------- |
| `/api/v1/...`       | katch 自身的管理接口（`internal/api/router.go`）                          |
| `/metrics`          | cago 的 metric 组件自动挂载                                               |
| `/v2/...`           | container registry 协议（客户端固定请求这个前缀，上游主机名在它**之后**） |
| `/<上游主机名>/...` | 其余上游（APT、Go proxy、GitHub 静态资源等）                              |
| 其余                | 前端 SPA 路由，回落 index.html                                            |

分辨保留段和上游段的规则是**点号**：第一段含 `.` 的是上游主机名（公网主机名必然含点），
不含 `.` 的是 katch 自己的保留路径。这条规则不会随着上游增加而退化。

`/api/` 和 `/assets/` 下未命中一律 404，不回落 index.html——理由见
[frontend.md](frontend.md#静态资源缓存)。

## 拉取路径

拉取路径和前端 SPA **共用 gin 的 NoRoute**，不另开通配路由：通配路由会和
`/api/v1` 以及将来任何一条真实路由抢同一段前缀，而 NoRoute 天然是「所有已注册
路由都没命中之后」，顺序问题不存在。`web.MountSPA` 因此必须排在 `mux.HTTP` 之前
注册（`cmd/katch/main.go`）。

一个请求进来后的顺序是：

1. `dispatch.Classify` 判定归属（纯函数，只看路径字符串）；
2. katch 自身端点未命中 → 404，绝不回落 index.html；
3. dist 里真有这个文件 → 给文件。**这一步排在上游分发之前**：`/favicon.ico`
   这类根目录产物名含点，按分段规则会被当成上游主机名，而 dist 是编译期固定的
   一小撮文件，让它优先才不会因为谁加了一条上游就把界面打坏；
4. 上游请求 → `proxy_svc.Fetch` 回源，响应体 `io.Copy` 流式转发；
5. 其余 → 回落 index.html。

三个包各管一段，彼此不互相知道：

| 包                           | 职责                                             |
| ---------------------------- | ------------------------------------------------ |
| `internal/proxy/dispatch`    | 纯函数分段：哪一段是主机名、剩下的是上游路径     |
| `internal/service/proxy_svc` | 白名单判定 + 回源编排，以及上游表的进程内缓存    |
| `internal/proxy/origin`      | 回源的 HTTP 客户端：头部过滤、302 跟随、流式响应 |

几条不能松的规则：

- **不在上游表里、已停用、协议类别对不上**，三者对外是同一个 404：空响应体、
  不加任何头，主机名一个字节都不出现。任何可观察的差别都在告诉探测者「这台主机
  存在，只是被停用了」，那就把 katch 变成了内网主机探测器。502 同理。
- **上游路径以转义形态原样送达**。先解码再拼回去会改写请求行——Go module proxy
  的 `!` 转义、APT 文件名里的 `+` 和 `~` 都会因此请求到别的对象上。路径里出现
  `..`（含 `%2e%2e`）一律 404：回源地址可以带基路径，回溯段能爬出那个基路径。
- **请求头按白名单转发**，客户端的 `Authorization` 不给上游（决策 11）。白名单
  而不是黑名单：黑名单每出现一个新的凭据头都要记得补一条，漏一次就是一次泄漏。
- **generic static 的响应体、签名覆盖内容和制品正文不得改写**，状态码原样透传（含
  4xx）；只有连不上、超时这类拿不到响应的情况才是 502。APT 的响应体在 `InRelease`
  的 GPG 签名覆盖范围内，改一个字节整个源就验不过。只有明确的协议适配器可以结构化
  改写非签名元数据中的已知字段；转换发生在回源与缓存之间，缓存的是转换后的表示，
  并且必须同步处理内容长度、编码、ETag 与缓存变体。
- **302 由 katch 自己跟随**。GitHub 的 release 资产会跳到另一台主机，把重定向
  透给客户端等于让它直连一个在受限网络下不通的地址。当前实现尚未校验跳转目标的
  主机和地址范围；完整的公开镜像支持必须补上公网地址、允许主机和 DNS 重绑定检查，
  见[公开包镜像加速兼容性](package-manager-mirrors.md#通用实现要求)。

上游表的进程内缓存做成**包在 `upstream_repo` 外面的一层**
（`proxy_svc.NewCachedUpstreamRepo`，由 main 装配），而不是在 service 里各存一份：
所有写入都要经过这个接口，装在这里，管理接口增删改停任何一条上游都会自动失效，
将来多一个写入口也不会漏——靠每个写入方自己记得失效一次的方案，漏掉的那次表现为
「界面上停用了但还在回源」，且只在生产上才看得见。缓存的是**整张表的快照**：
上游是人工维护的白名单，规模是几十条，一次装载之后未知主机也能在内存里直接答
「没有」，不给探测流量留一条打到库上的通路。管理接口的读（`Find`/`List`）直穿到库，
它要看到的是刚写进去的那条。

## 数据库

默认 sqlite，配置可切 MySQL，**因此迁移的 DDL 必须两种方言都成立**，不能随手用
MySQL 特有语法。DDL 优先写原生 SQL 而不是依赖 gorm 的 AutoMigrate。

迁移只追加不修改：新迁移加到 `migrations.migrationList()` 末尾。已经在环境里跑过的
迁移即使改了也不会重跑，只会让新旧环境的表结构悄悄分叉；需要修正时追加一条补丁迁移。

表名带 `db.prefix` 前缀，由 gorm 的 NamingStrategy 生成。迁移里的原生 DDL 因此
不写死表名，而是从实体反解（`migrations.tableName`）——写死会让改过前缀的部署建出
一张 repository 永远查不到的表。代价是表名跟着实体的结构体名走，**重命名实体必须
配一条补丁迁移**。

sqlite 的 DSN 里两个 pragma 不能省（见 `configs/config.yaml` 的注释）：
`journal_mode(WAL)` 让读不阻塞写，`busy_timeout` 让遇锁时等待而不是立刻返回
`SQLITE_BUSY`——缺了它并发写会直接报 "database is locked"。

## 上游适配

container registry、APT、Go module proxy、GitHub 静态资源共用一套「按上游主机名
分发 + 内容寻址缓存」的骨架；registry 因为鉴权和路径形态特殊，单独成一类，
git 因为要处理带请求体的协商，是另一个适配器（见下）。

一条已经确定的原则：上游配置以**主机名**为 key，加一个新上游应当是加一行配置，
而不是加一个新的路径前缀约定。

### 扩展点的边界与守卫

边界有两侧。**只加一条记录就能支持的**：任何「HTTP GET/HEAD + 同一仓库基地址 +
相对制品路径 + 同一 URL 不按未进入缓存键的请求头返回不同表示」形态的新上游——再来
一个 APT 镜像或结构相同的静态仓库，都只是 `upstream` 表里多一条记录，不改代码、
不重启进程。npm、PyPI 等仓库虽然同样以 GET 下载为主，但元数据可能给出另一个域名的
绝对制品 URL，或根据 `Accept` 返回不同格式；要保证客户端全程经过 katch 且缓存不串
表示，需要结构化改写元数据并扩展缓存变体，因此属于协议适配器的范围。具体兼容边界见
[公开包镜像加速兼容性](package-manager-mirrors.md)。**必须写新适配器的**：需要 GET/HEAD
之外的协议语义，或需要改写元数据中已知字段的上游，比如 git 的 `git-upload-pack`
（带请求体的 POST）——这类差异是行为而不是数据，记录上的字段表达不了，所以 git
的支持是一个适配器加一个协议取值，而不只是一条记录。

### 包管理器 profile 与就绪状态

`internal/proxy/packageprofile` 的注册表是包管理器能力的唯一事实来源。每个内建适配器
声明 `Describe`、`Companions` 与 `Guidance`；`upstream_svc.PackageProfiles` 把实际注册
结果和 `none` 暴露给 `/api/v1/site`。前端只渲染这个列表和后端返回的稳定枚举，不维护
另一份 profile、客户端或 companion 对照表。

上游列表的公开与管理 DTO 都带 `package_profile`；非 `none` 时还带按当次数据库快照
计算的 `package_readiness`。就绪要求同时满足：规范 `site_domain` 非空，主上游启用并
包含 `static`，以及每个 companion host 都存在、启用、包含声明的 transport 且 profile
精确相等。后台编辑页把未保存草稿作为 preview query 交给同一个 service 计算，因此
保存前后不会出现两套兼容规则。companion 不满足时返回具体 host 和 `missing`、`disabled`、
`transport` 或 `profile` 原因；服务端绝不据此自动创建或修正白名单记录。

「规范」不只是非空：`site_domain` 在写入时就要能拼成一个绝对的 http(s) 基地址，
接受光主机名（补 `https://`）和带端口、带一段基路径的完整地址，拒绝用户名密码、
查询串、片段、非 http(s) 协议和坏端口。校验挂在设置项定义上，因此库里一行旧的
坏值在读出来时同样退回空值——设置页、readiness 与改写快照看到的都是「还没配」，
而不是一个拼不通的地址。读到坏值会留一条 warn，只说原因不带原值：被拒的头号原因
正是「带了用户名密码」，把它抄进日志只是换个地方泄漏。

`Guidance` 只描述已实现的只读客户端配置和限制，所有 URL 都从数据库里的
`site_domain` 派生。`runtime_verified` 是独立的证据门：只有客户端在阻断公网的运行时
矩阵中通过后才置真；协议单测或 harness case 存在本身不能置真。2026-09-17 至
2026-09-18 在 `coding.local` 完成的矩阵已经覆盖 `npm`、`pypi`、`goproxy`、`maven`、
`cargo`、`nuget`、`rubygems`、`apt`、`rpm`、`apk`、`composer`、`homebrew` 全部内建
package profile，因此这些明确 allowlist 的 profile 返回 `runtime_verified=true`；`none`、
空值和未来未知 profile 仍返回 `false`。界面只有在 `runtime_verified` 和当次数据库快照
算出的 `package_readiness.ready` 同时为真时才显示可复制配置。共享回归中的真实 Git
clone 已验证；Docker、Podman 改由独立的 privileged 回归用例执行原生 pull、digest 校验和
容器内版本运行，blocked/version 诊断不再构成 runtime 成功证据。

git 的适配器走的是**穿透 + 本地镜像双路径**：镜像还没建成、或者请求带着 shallow、
`--filter` 这类本地给不了的形态时，请求原样转给上游，同时在后台把这个仓库镜像
下来；镜像就绪之后，clone / fetch 由 katch 自己按客户端这一次的协商现打一个包
答复，一个字节都不再问上游。所以「响应不可缓存」这句话只对其中一侧成立，而且
成立的理由不是「它太大」——一次协商的结果因客户端而异，它根本不是一个内容寻址
的对象，进了对象缓存就是把一个客户端的协商结果发给另一个客户端。这一次是谁答的
写在响应头 `X-Katch-Git` 上（`local` / `passthrough`），与 `X-Katch-Cache` 各答
各的一件事。

记录上的 `protocols` 是一个**集合**而不是单值：git 的端点寄生在静态资源的路径
空间里（`<仓库路径>/info/refs`、`<仓库路径>/git-upload-pack`），同一条 `github.com`
必须能一边服务 clone、一边服务 release 资产。判定因此是「这个主机开没开这一种」，
而不是「这个主机是哪一种」；空集合对任何形态都答否——判定的默认值必须是拒绝。

「不改代码、不重启」这句话不写成用例就只是一句愿望：它会在某次「顺手加个 host
判断」之后悄悄失效，而所有既有用例仍然全绿——它们测的都是已经存在的那几个上游。
所以它由两处守卫盯着，缺一不可：

- `internal/proxy/extension`：在一个已经装配好的进程里，经 HTTP 打管理接口注册一个
  此前不存在的假上游，再经拉取路径完成「拉取 → 缓存 → 命中」。仓储是真 sqlite、
  缓存是真磁盘，一个 mock 都没有；用例还会自己去核实那个主机名没有出现在任何生产
  Go 代码里——否则它守的就成了一条内置上游，而不是扩展点。
- `scripts/smoke.sh`：用 `make build` 出来的真二进制把同一件事再走一遍，补上用例
  够不到的那一段（真配置、真启动路径、生产的 NoRoute 处理器），并确认从注册到命中
  全程是同一个进程。

两处的判据都是「源站有没有再被打到」和响应上的 `X-Katch-Cache`，而不是某个内部
标志位：标志位只能证明代码走了我们以为的那条分支。

## 配置的边界与管理密钥

`configs/config.yaml` 只放「**进程起不来就没法从界面改的东西**」：监听地址、
数据库 DSN、日志、缓存目录、初始管理密钥。这是一条可判定的规则，不是逐项拍脑袋
——数据库连不上时界面本身就不可用，所以 DSN 必须在文件里；配额、TTL、并发这些
进程跑起来之后才生效的参数一律落 `setting` 表，改完不用重启。

入库的只有 `configs/config.yaml.example`；进程实际读的 `configs/config.yaml` 从它拷
一份，被 `.gitignore` 挡着。这不是洁癖——配置文件里有初始管理密钥和 DSN，入库那份
一旦成为大家实际编辑的文件，真密钥进历史就只是时间问题。`make dev` 在缺失时自动拷
一份（已存在的不覆盖），`scripts/smoke.sh` 和 Docker 镜像都直接用 `.example`。

上游同理：上游是 `upstream` 表里的记录，不是配置项。增删上游、暂停上游是运维日常
动作，做成配置项意味着每次变更都要改文件并重启。这张表同时就是白名单——不在表里
或 `enabled` 为假的主机一律 404，否则 katch 就是一个开放代理。

管理密钥以 bcrypt 哈希存在 `setting` 表的 `admin_key_hash` 键上。`config.yaml` 里的
`admin.initialKey` 是**初始**密钥，只在库中尚无密钥时落一次；之后的轮换在界面上完成，
改配置文件不会把已轮换的密钥覆盖回去。没配初始密钥不阻断启动：管理接口全量 401，
拉取路径照常服务。

`/api/v1/admin/*` 一律要密钥，拉取路径公开。**密钥错误与未提供密钥必须返回完全相同的
401**——状态码、响应体、响应头都一致，且不带 `WWW-Authenticate`。任何差异都在告诉
探测者「密钥这个字段你找对了，只是值不对」。校验路径上即使没带密钥也照样比一次
bcrypt，否则两者的耗时差本身就是一个可区分的信号。
