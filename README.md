# katch

多上游镜像代理站。把 container registry、APT 源、Go module proxy、GitHub 静态资源
统一收到一个域名下，**路径的第一段就是上游主机名**。

使用者只需要记一条规则：把 `https://` 换成 `https://<katch>/`，其余照抄。

```bash
docker pull katch.example.com/docker.io/library/redis:7
curl -fLO https://katch.example.com/raw.githubusercontent.com/foo/bar/main/x.sh
```

```
# /etc/apt/sources.list
deb https://katch.example.com/deb.debian.org/debian bookworm main

# Go
export GOPROXY=https://katch.example.com/proxy.golang.org
```

加一个新上游是**在界面上加一条记录**，不是加一条路径前缀约定，也不需要重启进程。

## 它解决什么

受限网络下，容器镜像、系统软件包、Go 模块、GitHub 资源的加速各自要配一套方案：
四套配置、四个域名、四种故障模式，新机器初始化要重复一遍。katch 把它们收到一个
入口下，并且给这些内容寻址的对象加一层本地缓存——同一个镜像层、同一个 deb 包、
同一个模块版本只从上游取一次。

一条硬约束：**katch 不是开放代理**。只有上游表里启用的主机可被访问，不在表里、
已停用、协议类别对不上的一律返回同一个空 404。

## 部署形态

单个静态二进制。前端产物经 `//go:embed` 编进二进制，由后端同源提供，
**不需要另外部署 nginx**——起一个进程就是全部。

```
katch (单进程, :8080)
├─ /                      前端 SPA（embed 进二进制的 dist）
├─ /api/v1/...            管理接口（/admin/* 需密钥）
├─ /metrics               Prometheus 端点
├─ /v2/...                container registry 协议
└─ /<上游主机名>/...       其余上游（APT / Go proxy / GitHub 等）
```

分辨保留段和上游段的规则是**点号**：第一段含 `.` 的是上游主机名，不含 `.` 的是
katch 自己的保留路径。默认数据库是 sqlite（纯 Go 的 `modernc.org/sqlite`，不需要
cgo），落地就是一个文件；缓存对象按内容摘要落在磁盘目录里。

## 跑起来

### Docker

```bash
make docker
docker run -d --name katch -p 8080:8080 -v katch-data:/app/data katch/katch:0.1.0
```

### 从源码

```bash
make install-deps   # 安装前端依赖
make build          # 前端构建 → 拷进 internal/web/dist → CGO_ENABLED=0 编译
cp configs/config.yaml.example configs/config.yaml   # 自己那一份配置，改完再起
./bin/katch -config ./configs/config.yaml
```

入库的是 `configs/config.yaml.example`；`configs/config.yaml` 是本地那一份，
被 `.gitignore` 挡着，改管理密钥和 DSN 不会弄脏工作区。`make dev` 会在缺失时
自动拷一份，已存在的不覆盖。

打开 http://localhost:8080 就是首页，http://localhost:8080/admin 是管理后台，
密钥用 `configs/config.yaml` 里的 `admin.initialKey`。

### 开发

```bash
make dev   # 前端 vite :5173（/api 与 /metrics 转发到 :8080）+ 后端 go run
```

开发模式下前端由 vite 自己提供，二进制里挂的是占位文件。

## 首次配置

**上游表出厂是空的**，而这张表同时就是白名单——不加记录，任何主机都是 404。
在管理后台「上游 → 添加上游」里加，或者走接口：

```bash
KEY=change-me-before-exposing-katch

curl -X POST http://localhost:8080/api/v1/admin/upstreams \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{
        "host": "docker.io",
        "kind": "registry",
        "origin": "https://registry-1.docker.io",
        "enabled": true,
        "library_completion": true,
        "mutable_ttl_seconds": 300
      }'

curl -X POST http://localhost:8080/api/v1/admin/upstreams \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{
        "host": "deb.debian.org",
        "kind": "static",
        "origin": "https://deb.debian.org",
        "enabled": true,
        "immutable_patterns": ["/pool/"],
        "mutable_ttl_seconds": 300
      }'
```

`kind` 只有 `registry` 和 `static` 两种，其余差异靠记录上的字段表达：
`immutable_patterns` 声明哪些路径是内容寻址的（可长期缓存 + LRU 淘汰），
其余路径按 `mutable_ttl_seconds` 走短 TTL。

拉取时响应上的 `X-Katch-Cache: HIT|MISS` 能直接看出这一次有没有回源：

```bash
curl -sI http://localhost:8080/deb.debian.org/debian/dists/bookworm/InRelease | grep -i x-katch-cache
```

## 管理界面

| 页面 | 内容 |
| --- | --- |
| 首页 | 拉取助手：写下 `redis:7` 直接给出可执行的命令；站点命中率、已缓存体积、支持的上游 |
| 概览 | 请求量与命中率曲线、回源原因分布、上游健康矩阵、事件流、最近请求 |
| 上游 | 增删改、启停，改完下一个请求就生效 |
| 缓存对象 | 搜索、清理、锁定单个对象 |
| 设置 | 站点名片、首页是否公开、缓存配额与回收水位、回源并发/超时/重试、管理密钥轮换 |

访问规则分全局与上游内两层，同层内按**具体度**排序、首个匹配者决定结果；
保存前可以用规则测试器验证某个具体地址会被哪条规则决定。

## 配置的边界

`configs/config.yaml` 只放「**进程起不来就没法从界面改的东西**」：监听地址、
数据库 DSN、日志、缓存目录、初始管理密钥。配额、TTL、并发这些进程跑起来之后才
生效的参数一律落 `setting` 表，改完不用重启。

入库的只有 `configs/config.yaml.example`（出厂默认值 + 每一项为什么这么写的注释）。
实际读的 `configs/config.yaml` 从它拷一份，不入库——否则每个人改自己的密钥和 DSN
都会留下一片待提交的改动，真密钥被顺手带进历史只是时间问题。Docker 镜像里的那份
同样来自 `.example`。

管理密钥以 bcrypt 哈希存在库里，`admin.initialKey` 只在库中尚无密钥时落一次；
之后的轮换在界面上完成，改配置文件不会把已轮换的密钥覆盖回去。留空则不设密钥：
管理接口全量 401，拉取路径照常服务。

切 MySQL 只改 `db` 段的 `driver` 与 `dsn`。

## 开发命令

```bash
make test       # test-backend + test-frontend，唯一的测试入口
make lint       # golangci-lint v2 + eslint
make fmt        # golangci-lint fmt + prettier
make smoke      # 启动真实二进制验证基本行为（需先 make build）
make mock       # go generate ./...（mockgen）
```

`make smoke` 除了验启动路径，还守着扩展点那条边界：只经管理接口加一条上游记录，
就要能拉通并在第二次命中缓存，全程不改代码、不重启进程。

## 文档

- [AGENTS.md](AGENTS.md) — 工程约定（硬约束，包括测试先行、产物不引入 cgo、分层单向）
- [docs/architecture.md](docs/architecture.md) — 分层、路由命名空间、拉取路径、数据库
- [docs/frontend.md](docs/frontend.md) — 前端目录、i18n、静态资源缓存
- [docs/observability.md](docs/observability.md) — 日志与指标
- [docs/specs/](docs/specs/) — 需求与设计决策
