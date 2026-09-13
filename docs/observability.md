# 可观测性

katch 的排障入口有两个：结构化日志和 Prometheus 指标。两者都由 cago 提供，
这份文档说明它们在本项目里的约定。

## Logging

### 唯一出口

日志一律走 `logger.Ctx(ctx)`：

```go
import (
    "github.com/cago-frame/cago/pkg/logger"
    "go.uber.org/zap"
)

logger.Ctx(ctx).Info("upstream fetched",
    zap.String("upstream", "docker.io"),
    zap.String("ref", ref),
    zap.Duration("cost", cost),
)
```

`fmt.Print*` 和标准库 `log` 由 golangci-lint 的 forbidigo 拦住，原因不是风格：

- 它们绕过日志系统，**没有级别**，线上无法按严重程度过滤；
- **没有结构化字段**，排障时无法按 upstream / ref 检索；
- **不会写进 logFile**，进程的 stdout 被容器运行时丢弃后就什么都不剩；
- `log.Fatal` 会直接 `os.Exit`，跳过所有 defer 清理。

唯一豁免是 `cmd/katch/main.go`：cago 的 logger 要等 `component.Core()` 跑完才可用，
在那之前只有标准库能把错误说出来。新增豁免必须写进 `.golangci.yml` 的 exclusions
并附理由，不要就地加 `//nolint`。

### 日志落盘

由 `configs/config.yaml` 的 `logger` 段控制：

```yaml
logger:
  level: info
  disableConsole: false
  logFile:
    enable: true
    filename: ./runtime/logs/katch.log
    errorFilename: ./runtime/logs/katch.err.log
```

`errorFilename` 单独收 error 及以上级别，值班时只看这一个文件即可。

### 该记什么

代理站的日志量天然很大，每个请求都记全量字段会让日志本身成为瓶颈。约定：

- **info**：回源（cache miss）、缓存淘汰、上游鉴权刷新——低频且有诊断价值的事件，
  以及下面那条逐请求的拉取行；
- **debug**：逐请求的其余细节（回源的响应头、缓存键的拼法），默认关闭，排障时临时调级别；
- **error**：上游不可达、缓存写入失败、配置加载失败——需要人介入的事件。

记录上游主机名和资源引用，**不要记完整的 Authorization 头或 token**。

### 逐请求的拉取行

每一次拉取，计数中间件在记完指标之后再写一行日志（`internal/metrics` 的 `logPull`，
和计数同一个缝——两处埋点迟早会出现「指标上有、日志里没有」的对不上）：

```json
{"level":"info","ts":"...","msg":"拉取","at":1757700000,"upstream":"deb.debian.org",
 "object":"/pool/main/a/apt_2.7.deb","result":"miss","bytes":4096,"duration_ms":120}
```

几处是刻意的：

- **info 而不是 debug**。后台上游详情的「最近请求」读的就是这些行，级别调到默认看不见
  的地方，那块面板在一台没人动过日志配置的机器上就永远是空的。一行只有这六个字段，
  既不记请求头也不记 token；
- **只记上游表里有的主机**。拉取路径是公开的，把没见过的主机名也写进去，等于让任何人
  都能往这台机器的磁盘上写字符串——和指标那边把未知主机折成 `unknown` 是同一条理由。
  它们的计数照记，只是不落日志；
- **`at` 是这次拉取结束的时刻**，单位是秒。日志按结束顺序落盘，用开始时刻会让尾部的行
  在时间上不再单调，而面板就是按文件顺序从新到旧排的。它和 zap 自己的 `ts` 并存，是因为
  `ts` 的格式由 cago 的 encoder 决定，那不是本仓说了算的东西；
- **`result` 与 `katch_requests_total` 的 `result` 同一套取值**：`hit` / `miss` /
  `denied` / `origin_error`，判定也是同一处。

### 最近请求读的是这份日志的尾部

`GET /api/v1/admin/logs/requests?upstream_id=..&limit=..`（要密钥）给出某个上游最近的
若干次拉取。它不是一张表：决策 16 否掉了每请求写库——那会把拉取热路径拖进事务，而分钟级
的 `traffic_rollup` 答得了「这段时间总共怎么样」，答不了「刚刚发生了什么」。

`internal/service/log_svc` 的两条性质：

- **有界**。只读 `logger.logFile.filename` 那个文件**末尾 256 KiB**，返回条数上限 100
  条（`admin.RecentRequestsMaxLimit`）：更大的 `limit` 在请求校验那一关就被挡掉，服务层
  再夹一道——和事件流同一套写法，服务层被别的调用方直接用时上限一样有效。日志会长到几个
  G，把它读进内存等于给管理接口开一条内存放大路径；窗口左边界切到的那半行直接丢掉。
- **读不到就当没有**。没开落盘、文件刚被 lumberjack 轮转走、目录权限变了，一律返回空列表
  且不报错，界面据此让那块面板消失——排障的辅助块消失，好过让整屏管理界面挂在一句打不开
  文件上。

**端点上没有文件名这个参数。** 读哪个文件只由 `configs/config.yaml` 的 `logger.logFile`
决定。让调用方指定路径，等于给管理接口开一条任意文件读取的路：密钥被拿到过一次，整台
机器的文件就都跟着出去了。

## Metrics

`component.Core()` **内部已经初始化了 metric 组件**，它自行挂上 gin 中间件并暴露
`GET /metrics`（Prometheus 抓取端点），无需额外配置，也无需在 `main.go` 里单独注册。

不要再写 `Registry(cago.FuncComponent(metric.Metrics))`：那会让 otel 的 prometheus
exporter 创建两次、双双注册进 prometheus 的默认 registry，于是 `target_info` 被重复
收集，`GET /metrics` 直接返回 500——而 lint、单元测试和启动日志都不会有任何异常，
只有真正访问这个端点才看得见。`make smoke` 就是为这类问题准备的。

开发时 vite 把 `/metrics` 一并转发到后端，所以本地访问
`http://127.0.0.1:5173/metrics` 也能看到。

指标命名沿用 Prometheus 惯例：`katch_<子系统>_<名称>_<单位>`，
计数器以 `_total` 结尾。

### 拉取路径的指标

`internal/metrics` 把 katch 自己的指标注册进 **prometheus 的默认 registry**——
`component.Core()` 暴露的 `/metrics` 就是从那里收集的，另起一个 registry 的指标
在那个端点上根本看不见。

| 指标 | 标签 | 含义 |
| --- | --- | --- |
| `katch_requests_total` | `upstream`、`kind`、`result` | 拉取请求数。`result` ∈ `hit` / `miss` / `denied` / `origin_error` |
| `katch_request_duration_seconds` | `upstream`、`kind` | 拉取耗时直方图 |
| `katch_bytes_served_total` | `upstream`、`source` | 发给客户端的字节数，`source` ∈ `cache` / `origin` |
| `katch_origin_backoff` | `upstream` | 上游是否处于回源退避（降级）状态 |

计数发生在一个 gin 中间件里，它排在 SPA 与拉取共用的 NoRoute 之前，按响应本身
判定结果：502 是回源失败、403 是规则拒绝、带 `X-Katch-Cache: HIT` 的是命中、
其余算回源取回（包括上游自己的 404——那是上游对这个对象的判断，不是 katch 拒绝了谁）。
好处是埋点只有一处，不必在缓存层和代理层各插一次。

`upstream` 标签只取**上游表里有的**主机名，其余一律折叠成 `unknown`：拉取路径是
公开的，把请求里的主机名原样当标签，等于给任何人开了一条不需要密码的时间序列
放大路径。

**不导出命中率这类比值。** 它由 `hit / (hit + miss)` 推导，导出成 gauge 只会在
采样窗口不一致时给出两个互相矛盾的数字。界面上的命中率同样是推导值，
`traffic_rollup` 里也没有 `misses` 列——它是 `requests` 减去其余三项。

以下指标在 spec 的可观测性一节里列出，但它们的埋点在别的缝上（回源客户端、
缓存淘汰、规则求值、token 交换），由对应的任务补齐，这里不先占名字：
`katch_origin_requests_total`、`katch_origin_duration_seconds`、
`katch_origin_inflight`、`katch_cache_objects`、`katch_cache_bytes`、
`katch_cache_evictions_total`、`katch_cache_integrity_failures_total`、
`katch_rule_decisions_total`、`katch_token_exchanges_total`。

### 界面上的统计不走 Prometheus

`/metrics` 是给外部采集用的标准端点；界面上的请求量、命中率、省下的流量来自
**同一批进程内计数器**的分钟级快照（决策 16）：`stat_svc` 每分钟把计数器取走一次，
按上游累加成 `traffic_rollup` 的一行，保留 90 天，24 小时 / 7 天 / 30 天都从这张表
聚合。一个自部署的镜像站不该为了看自己的命中率就要先搭一套监控，而每请求写一次库
会把拉取热路径拖进事务。

- 公开：`GET /api/v1/stats/overview?range=24h`，只给全站合计；
- 要密钥：`GET /api/v1/admin/stats/upstreams?range=24h`，按上游一行，附带降级标记。

### 上游降级

`internal/proxy/backoff` 按上游记连续的回源失败，达到阈值后进入指数退避。状态
**只在内存里**（决策 17）：退避说的是「此刻」，落库只会把一段早已过期的状态带到
下一个进程。它同时出现在 `katch_origin_backoff` 和上面那个管理接口的 `degraded`
字段上。

计数中间件只负责把回源的成败喂给它（命中不算——命中根本没碰上游，拿它当恢复的
证据会让一个还在挂的上游刚出退避就被全量打回去）。

`Tracker.Allow` 这道快速失败的闸问在回源那一步：`proxy_svc.Fetch` 查完白名单、
正要调 `origin.Do` 之前。窗口里的请求当场拿到 `proxy_svc.ErrUpstreamBackoff`
（对外是 502），不再各自去等一次连不上的拨号。

闸只能装在这个位置，装在更外层的中间件上会连缓存命中一起拒掉——上游挂了还能把
缓存发出去，正是这个镜像站存在的理由。它也必须排在白名单判定之后：表里没有的
主机名要先拿到那个统一的 404，否则「退避中」和「不存在」之间的差别就成了探测
内网主机的信号（决策 6）。

由此还有一条：**窗口期内的失败不计数**。被闸挡住的请求同样是一个失败的响应，
会被计数中间件原样喂回来，但上游根本没被碰过；算成新证据的话，docker / apt 的
自动重试会把退避一路顶到上限并不断顺延，上游恢复了也等不到那次探测。

## Traces

暂未接入。cago 的 trace 组件需要配置文件里有 `trace` 段，缺失时直接注册会让服务
启动即 panic——所以在真正需要链路追踪之前，这里不注册该组件，配置里也不留空的
`trace` 段。
