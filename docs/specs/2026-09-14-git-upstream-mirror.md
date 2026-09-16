# git 上游：协议集合、穿透与本地镜像

> Status: Approved
> Owner: katch
> Last updated: 2026-09-14

**Objective:** 让使用者把 git 仓库的 URL 前缀换成 katch 的域名就能 `clone` / `fetch`
公开仓库；让被拉取过的仓库在 katch 本地建成镜像并由本地应答，镜像未就绪或请求形态
本地给不了时穿透上游；同时把上游的协议判定从单值 `kind` 统一成协议集合，使同一台
主机可以同时提供 git 与静态资源。

**Hard invariant:** katch 不得成为开放代理——上游表即白名单，「不在表里」「已停用」
「协议没开」三者对外仍是同一个空 404（首版 spec 决策 6）；响应体不被改写；客户端的
`Authorization` 不转发给上游（首版 spec 决策 11）；产物不引入 cgo。

## Problem

1. **git 仓库在受限网络下拉不动，而 katch 现在连门都不开。** 拉取路径对非 GET/HEAD
   一律 405（`internal/web/embed.go:126`），而 git smart HTTP 的对象协商是一个带请求体
   的 POST，`clone` 在第二步就被挡在外面。回源客户端也没有请求体这一路——
   `origin.Request`（`internal/proxy/origin/origin.go:20`）只有 Method/Path/Header。
2. **重复 clone 全额回源。** 同一个仓库被不同机器、不同 CI 任务反复完整克隆，
   而其中绝大部分对象在两次克隆之间根本没变。
3. **单值 `kind` 表达不了「同一主机两种协议」。** 上游表以主机名为 key，一台主机只有
   一条记录；`github.com` 既有 git 端点，又有 `/<owner>/<repo>/releases/download/...`
   这类纯 GET 资产。`kindMatches`（`internal/service/proxy_svc/proxy.go:343`）是条双向
   断言，一条记录声明成什么就只能从对应的路径空间进来，两者无法并存。
4. **架构文档对 git 的判断需要更正。** `docs/architecture.md:129` 把 git 描述为
   「chunked POST、响应不可缓存」，那是穿透方案的性质；katch 采用的是穿透 + 本地镜像
   双路径，本地应答那一侧的响应是 katch 自己按客户端协商现打的包。

## Actors and user stories

1. 作为**使用者**，我希望把 `https://` 换成 `https://katch.example.com/` 就能 `git clone`
   一个公开仓库，这样 git 和其余上游共用同一条规则。
2. 作为**使用者**，我希望 `--depth 1`、`--filter` 这类 katch 本地给不了的用法也不会失败，
   这样我不必先知道 katch 支持到哪一步才敢用。
3. 作为**使用者**，我希望在别人刚 push 完之后 clone 能拿到新提交，这样 katch 不会
   悄悄给我一个过期的仓库。
4. 作为**运维者**，我希望在界面上给某个上游打开 git，并看到哪些仓库已经建成镜像、
   各占多少磁盘，这样加速是可见、可核对的。
5. 作为**运维者**，我希望镜像占用有上限并自动回收，这样一次批量 clone 不会把盘撑满。

## Design decisions

| # | 决策 | 依据与被否方案 |
|---|---|---|
| 1 | 上游的协议判定从单值 `kind` 改为集合 `protocols`（JSON 列），`kind` 列删除 | git 端点寄生在 static 的路径空间里，同一条记录必须能同时开两种协议；`PatternList` 已经是 JSON 列的先例。否决：新增 `kind=git`——那条 `github.com` 记录就再也服务不了 release 资产。否决：三个布尔列——每多一个协议就多一列加一条迁移，且「一个协议都不开」这种无效状态要靠额外校验拦 |
| 2 | `kind` 列在同一条迁移里回填后删除，不保留 | 留一个没人读的旧真相，迟早有人照着它写判断，两份真相就此分叉 |
| 3 | git 端点由**路径形态**识别（`.../info/refs?service=git-upload-pack` 与 `.../git-upload-pack`），不占用独立路径前缀 | 这两个端点本来就是任意仓库路径下的固定后缀，识别是确定的；加前缀等于要求使用者改写 URL，违背「把 https:// 换掉，其余照抄」 |
| 4 | **只读**：`git-receive-pack`（push）与其 ref 广播一律拒绝 | katch 是镜像不是代码托管；且不转发客户端凭据（首版 spec 决策 11）意味着写操作也无从鉴权 |
| 5 | **双路径**：镜像就绪且形态支持 → 本地应答；否则穿透上游，并在后台建镜像 | ref 广播必须先于任何字节：客户端 want 的是 katch 广播出去的 hash，镜像建成前 katch 没有任何可以诚实广播的东西。且上游给 katch 的 pack 不等于客户端要的 pack（两侧协商出的能力集与 ref 子集都不同），静态对象那种边落盘边转发在 git 上不成立。否决：未就绪时让客户端等——首次不可用，且 `--depth` 永远不可用。否决：只做穿透——没有任何加速 |
| 6 | 本地 git 用纯 Go（go-git），不依赖 git 二进制 | 保住「单个静态二进制」这一条部署承诺。否决：镜像里装 git——协议 v2、shallow、partial clone 全部白送且大仓库性能有保障，但产物不再自包含，非 Docker 部署要自备 git。代价以明确的非目标写下，见「本地应答的能力边界」 |
| 7 | 镜像**按需懒建**（谁拉过就镜像谁），配总量配额与单仓上限，按最后访问时间淘汰 | 与现有 static 上游「拿来就用」的体验一致，不需要人工登记。否决：管理面显式登记——加速要人工开，且新仓库永远是冷的 |
| 8 | refs 的新鲜度复用上游记录上的 `mutable_ttl_seconds` | refs 正是典型的可变对象，与首版 spec 决策 7 的分档一致。TTL 内直接本地广播；超了先做一次增量同步再广播；同步失败或超时降级为穿透。陈旧因此永远不超过 TTL。否决：先答旧的、后台再同步——push 完立刻 clone 会拿到旧提交，而使用者无从判断 |
| 9 | git 的任何响应都不进对象缓存 | 协商结果因客户端而异，不是内容寻址的对象；写进缓存就是把一个客户端的协商结果发给另一个客户端 |
| 10 | 穿透路径转发请求体，并把 `Content-Type` 与 `Git-Protocol` 加入转发头白名单 | 前者是 upload-pack 请求的载体，后者缺了会让协议退回 v0。白名单仍是白名单（首版 spec 决策 11），只是多了这两项确定用途的头 |

## 数据模型

`upstream`
: 去掉 `kind`，新增 `protocols`——协议集合，库里存 JSON 文本，取值为 `static`、
  `registry`、`git` 的任意非空子集。一条追加迁移完成建列、回填、删列三步：
  `kind='registry'` 回填成 `["registry"]`，`kind='static'` 回填成 `["static"]`。
  出现第三种 `kind` 值说明库被手改过，迁移报错停下，不猜默认值。

`git_mirror`（新表）
: `host` + `repo`（仓库在上游侧的路径，两者共同唯一）、`state`
  （`pending` / `ready` / `failed` / `rejected`）、`last_sync_at`、`last_access_at`、
  `size_bytes`、`last_error`、时间戳。它是镜像的**目录之外的那份真相**：进程重启后
  据此知道盘上哪些镜像可用，不必扫盘推断。

`setting`（运行时项，沿用现有键值表）
: git 镜像总配额、单仓体积上限、同步超时、同步并发上限。这些都是进程跑起来之后
  才生效的参数，按首版 spec 决策 4 一律落库而不进 `config.yaml`。

镜像本身落在缓存目录旁的独立目录下，与内容寻址的对象缓存分开：两者的淘汰依据不同
（对象按 LRU + 摘要，镜像按最后访问时间 + 仓库粒度），混在一处会让任一侧的配额
失去意义。

## 协议判定与路径分发

`dispatch.Classify` 的分段规则不变：第一段含点即上游主机名。git 端点在此之后由路径
后缀识别——`<仓库路径>/info/refs` 且查询串 `service=git-upload-pack`，以及
`<仓库路径>/git-upload-pack`。前者是 GET，后者是 POST。两者之外的路径仍按 static 处理，
因此同一条 `github.com` 记录可以一边服务 `clone`，一边服务 release 资产下载。

`kindMatches` 改为查协议集合是否包含对应协议，安全性质逐条保持：`docker.io` 那条记录
回填后只有 `["registry"]`，`https://katch/docker.io/foo` 照旧 404；三种拒绝理由仍折叠
成同一个不带主机名的空 404。

`service=git-receive-pack` 的 ref 广播与 `POST .../git-receive-pack` 一律拒绝，
返回 403 且响应体为空。它与「协议没开」的 404 有意区分：请求打到了一个确实开着 git
的上游，拒绝的是这个**动作**而不是这个主机的存在，不泄漏任何白名单信息。

## 穿透路径

镜像未就绪、或请求形态本地应答不了时，请求原样转给上游：方法、路径、查询串、请求体
照抄，响应状态码与响应体原样透传。回源客户端因此需要一路请求体，并把 `Content-Type`
与 `Git-Protocol` 纳入转发头白名单。

穿透的响应不进缓存。穿透期间若该仓库尚无镜像记录，登记一条 `pending` 并在后台开始建
镜像；已经是 `failed` 或 `rejected` 的不重复触发。

既有的退避闸（首版 spec 决策 17）对穿透同样成立：上游在退避窗口里时穿透直接失败，
返回 502，不绕过闸。

## 本地应答的能力边界

go-git v5.19.2 的服务端实现（源码核实）决定了本地能答什么：

- 广播的能力集只有 `agent` 与 `ofs-delta`（`plumbing/transport/server/server.go:191`），
  没有协议 v2——整个 plumbing 里没有 v2 的痕迹；
- shallow 明确不支持：收到 shallow 请求直接报错（同文件 `:160`），
  即 `--depth` 无法由本地应答；
- 没有 partial clone（`--filter`）；
- 每次应答都全量遍历对象图并从零重打包（`revlist.Objects` + `packfile` 编码，
  无 pack 复用），因此大仓库的本地应答不可行。

据此，以下请求一律走穿透而不是失败：带 shallow / `--filter` 的请求；体积超过单仓上限
的仓库（登记为 `rejected`，此后永久穿透，不反复尝试建镜像）。这条降级是本设计里
`--depth 1` 仍然可用的唯一原因，它必须是**默认**行为而不是错误处理的兜底。

smart HTTP 那一层的胶水由 katch 自己写：`packp.AdvRefs` 的 `Prefix` 字段正是为
HTTP 的 `# service=` 前导行准备的，请求体按 `application/x-git-upload-pack-request`
解析，响应以 `application/x-git-upload-pack-result` 返回。

## 镜像生命周期

一个仓库第一次被拉到时登记为 `pending`，后台开始 mirror；此间的请求全部穿透。
mirror 成功转 `ready`，失败转 `failed` 并记下原因，超过单仓上限转 `rejected`。

`ready` 的仓库收到请求时：`last_sync_at` 在 TTL 内直接本地广播 refs；超出 TTL 则先做
一次增量同步再广播，同步失败或超过同步超时就这一次降级为穿透，镜像状态不变——一次
上游抖动不该把一个可用的镜像作废。

同一个仓库的并发同步合并为一次，与对象缓存的并发回源合并（首版 spec 决策 9）同理：
冷启动时多个客户端同时 clone 同一个仓库，不合并会把并发原样放大到上游。

总配额超限时按 `last_access_at` 最旧的顺序删整个仓库镜像，删到配额之下。正在被读的
镜像不删。被删的仓库回到「没有镜像」的状态，下次拉取重新走穿透 + 后台重建。

上游被停用或删除时，其下的镜像一并失效：停用等同于不在白名单里，盘上有副本也不发出去。

## 可观测性

git 的每一次拉取记一次请求事件，标签区分**本地应答**与**穿透**，与现有
`katch_requests_total` 的上游/类别/结果三个标签同构。镜像状态变化（建成、失败、
超限拒绝、被淘汰）记入事件流，与缓存回收事件同一套出口。

`X-Katch-Cache` 在 git 上不复用：它的既有语义是「这个对象有没有回源」，而 git 的一次
应答不是一个对象。git 的应答改带 `X-Katch-Git`，取值 `local`（本地镜像应答）或
`passthrough`（穿透上游），让两个头各自只回答一件事。它同时是端到端用例的判据——
判据必须是客户端能观察到的东西，而不是某个内部标志位。

管理界面上，上游表单的协议从单选换成多选；git 镜像有独立的列表页，显示仓库、状态、
体积、最后同步与最后访问时间，并可删除单个镜像。全部文案走 `t()`，两份 locale 同步。

## 契约变更与兼容

管理接口的既有契约就此变更，不保留兼容别名：上游的读写结构体上 `kind`（字符串）
换成 `protocols`（字符串数组），必填且非空。保留别名意味着
两个字段长期并存、且要定义「两个都传且互相矛盾」的行为，代价高于一次性换掉。

连带要改的还有两处 `kind` 的读取点与一处写出点：命中 registry 缓存时补
`Docker-Content-Digest` 的判断（`internal/service/cache_svc/cache.go:377`）改为查协议
集合；上游变更事件的负载里 `kind` 换成 `protocols`
（`internal/controller/upstream_ctr/upstream.go:93`），前端事件流据此渲染。

README 与 `docs/architecture.md` 里所有以 `kind` 为前提的说明一并更正，
包括 `docs/architecture.md:129` 对 git 的那句过时判断（见 Problem 4）。

## Out of scope

- **push / `git-receive-pack`**：一律 403，不在本 spec 内，也不在计划内。
- **私有仓库与任何需要客户端凭据的操作**：与首版 spec 决策 11 冲突，永久非目标。
- **SSH 协议**：katch 是 HTTP 入口。
- **本地应答支持协议 v2、shallow、partial clone**：受 go-git 服务端能力限制，
  这些形态走穿透。换成 git 二进制才能解除，不在本 spec 内。
- **大仓库的本地镜像**：超过单仓上限的仓库永久穿透。
- **git LFS 对象**：它们在另一台主机上，需要单独加一条上游，不在本 spec 内。
- **`registry` 与 `static` 同时开在一条记录上**：模型允许，但现有上游都不会用到；
  判定与校验必须对这种组合成立，界面不为它做专门引导。

## Testing decisions

| Seam | 验证什么 | 现有同类 |
|---|---|---|
| `migrations` 包用例 | 造 `kind` 两种值的旧记录，跑迁移后 `protocols` 回填正确、`kind` 列已删；出现未知 `kind` 时迁移报错而不是猜默认值 | `migrations/access_rule_test.go` |
| 协议判定函数（替代 `kindMatches`） | 请求归属 × 协议集合的全部组合，含空集合与未知值 | `internal/service/proxy_svc/proxy_test.go` |
| git 端点识别（纯函数） | 穷举：`info/refs` 带/不带 `service`、`upload-pack` 与 `receive-pack`、仓库路径里含点或 `.git` 后缀、以及形似而非 git 端点的普通路径 | `internal/proxy/dispatch/dispatch_test.go` |
| 拉取路径的 HTTP 层 | 方法闸只对 git 端点放行 POST，其余路径仍 405；`receive-pack` 返回 403 空体；协议没开的主机返回 404 且响应里不含主机名 | `internal/web/cache_test.go` |
| 回源客户端 | 请求体原样送达上游；`Content-Type` 与 `Git-Protocol` 被转发；`Authorization` 仍然不被转发 | `internal/proxy/origin/origin_test.go` |
| 镜像状态机（service 层，repo 用 mock） | pending → ready / failed / rejected 的每条转移；TTL 内不同步、超 TTL 先同步；同步失败降级穿透且状态不变；并发同步合并为一次 | `internal/service/cache_svc/*_test.go` |
| 配额与淘汰 | 超配额时按最后访问时间淘汰到配额之下；正在被读的镜像不删 | `internal/service/cache_svc/sweep_test.go` |
| 真 git 客户端对拉 | 起一个本地 upstream 仓库，经 katch 走完「穿透 clone → 后台建镜像 → 本地应答 clone」，两次拿到的提交一致；`--depth 1` 走穿透且成功 | `internal/proxy/extension` |
| `scripts/smoke.sh` | 用 `make build` 的真二进制，经管理接口开 git 协议、真实 clone 一次、确认应答来源头的取值 | 现有 smoke 的上游注册与命中判据 |

真 git 客户端对拉这一条是唯一能证明「协议真的对」的判据——协议层的用例只能证明
katch 发出的字节符合我们的理解，不能证明 git 认这些字节。它需要环境里有 git 客户端；
若 CI 环境不具备，这一条降级为以本地的运行时观察覆盖。

## Open questions

无。
