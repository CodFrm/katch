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

- **info**：回源（cache miss）、缓存淘汰、上游鉴权刷新——低频且有诊断价值的事件；
- **debug**：逐请求的命中/未命中，默认关闭，排障时临时调级别；
- **error**：上游不可达、缓存写入失败、配置加载失败——需要人介入的事件。

记录上游主机名和资源引用，**不要记完整的 Authorization 头或 token**。

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

## Traces

暂未接入。cago 的 trace 组件需要配置文件里有 `trace` 段，缺失时直接注册会让服务
启动即 panic——所以在真正需要链路追踪之前，这里不注册该组件，配置里也不留空的
`trace` 段。
