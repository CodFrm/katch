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

### Docker Compose

```bash
cp deploy/config.yaml.example deploy/config.yaml   # 改掉 admin.initialKey
docker compose -f deploy/docker-compose.yml up -d
```

### Kubernetes

```bash
helm install katch deploy/helm/katch --namespace katch --create-namespace \
  --set config.admin.initialKey=$(openssl rand -hex 24)
```

不想用 Helm 就 `kubectl apply -k deploy/kubernetes/`（先改 `secret.yaml` 里的密钥）。

两条路的细节、以及**为什么只能跑一个副本**，都在
[docs/deploy.md](docs/deploy.md)。

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
        "protocols": ["registry"],
        "origin": "https://registry-1.docker.io",
        "enabled": true,
        "library_completion": true,
        "mutable_ttl_seconds": 300
      }'

curl -X POST http://localhost:8080/api/v1/admin/upstreams \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{
        "host": "deb.debian.org",
        "protocols": ["static"],
        "origin": "https://deb.debian.org",
        "enabled": true,
        "immutable_patterns": ["/pool/"],
        "mutable_ttl_seconds": 300
      }'
```

`protocols` 是这条上游开着的协议集合，取值为 `registry`、`static`、`git`
的任意非空子集——一条记录可以同时开几种，比如 `github.com` 既服务 `git clone`
也服务 release 资产下载。

### 缓存策略

不可变判定分两类，边界不重叠：

- **registry 按协议自动判定**，不需要也不看 `immutable_patterns`：blob 与按 digest
  寻址的 manifest（`.../blobs/sha256:...`、`.../manifests/sha256:...`）是内容寻址
  对象，自动长期缓存、只由 LRU 淘汰；tag manifest、`referrers` 与 `tags/list` 会变，
  按 `mutable_ttl_seconds` 过期。
- **static 与 git 路径只认显式模式**，不存在按 URL 里像不像 hash 的外观推断：
  `immutable_patterns` 为空则全部按 TTL；非空时命中的对象长期缓存，其余仍按
  `mutable_ttl_seconds`。模式对整条上游侧路径做**子串**匹配（不锚定首尾）：不含
  通配符时直接找子串，`*` 匹配任意多个字符（含 `/`），`?` 匹配任意一个字符。

管理界面「缓存策略」里的预设只是把下表的模式填进 `immutable_patterns`，保存的是
模式本身而不是预设名称——已登记的上游不会因为预设调整而静默改变；「自定义」可以
逐行编辑任意模式。

| 预设 | 写入的模式 | 判为不可变 | 仍按 TTL 的可变例外 |
| --- | --- | --- | --- |
| APT 软件包 | `/pool/` | `pool/` 下的 deb 包与源码包 | `dists/` 下的 `InRelease`、`Release`、`Packages*` 等索引 |
| Go Module Proxy | `/@v/*.info`、`/@v/*.mod`、`/@v/*.zip` | 具体版本文件 | `@v/list`、`@latest` |
| Git commit 静态文件 | 一整段 40 个 `?`（commit SHA） | 路径中含 40 位 commit 的对象 | 分支名、tag、`HEAD`、`info/refs` 这类会移动的引用 |
| PyPI 文件 | `/packages/??/??/` + 64 个 `?` | hash 目录下的包文件 | `/simple/` 索引与 JSON API |

### 命中验证

`X-Katch-Cache: HIT|MISS` 说明这一次有没有回源。要看到完整的 MISS → HIT，必须用
**GET 读完整响应体**再比对：普通 HEAD 和可本地求值的条件请求在命中时同样报 `HIT`，
但 HEAD 没有响应体、条件请求可能只拿到 `304`，单看一次命中说明不了本地那份副本
完整；`MISS → HIT` 加上两次内容逐字节相同才是完整证据。

请求的对象没有副本（或可变对象已过 TTL）时第一次才是 `MISS`；已在 TTL 内或不可变
对象一上手就是 `HIT`，换一个没缓存过的路径再验。

```bash
URL=http://localhost:8080/deb.debian.org/debian/dists/bookworm/InRelease

curl -s -D /tmp/first.h -o /tmp/first.b "$URL"
grep -i '^x-katch-cache' /tmp/first.h            # MISS：这一次回源

curl -s -D /tmp/second.h -o /tmp/second.b "$URL"
grep -i '^x-katch-cache' /tmp/second.h           # HIT：这一次由本地副本应答
cmp /tmp/first.b /tmp/second.b                   # 命中内容与回源逐字节相同

# 新鲜副本上的 HEAD：本地 200、无响应体、命中
curl -sI "$URL" | grep -i '^x-katch-cache'                              # HIT
# 条件请求命中：星号问的是「还有没有这份表示」，本地 304
curl -s -o /dev/null -w '%{http_code}\n' -H 'If-None-Match: *' "$URL"   # 304
```

TTL 内的副本由本地应答，命中响应原样回放上游保存下来的 `ETag`/`Last-Modified`
（registry 对象没有上游 ETag 时退回内容摘要推导）。由此：

- 普通 `HEAD`：本地 `200`，状态与响应头同一套，只是不带响应体，`X-Katch-Cache: HIT`；
- 条件请求：`If-None-Match` 按弱比较、逗号列表与 `*` 求值，命中返回本地 `304`
  （也是 `HIT`），明确不匹配则本地 `200`；只有没有 `If-None-Match` 时才看
  `If-Modified-Since`，日期不晚于请求时间即 `304`；
- 副本缺少求值所需的 validator（上游从未给过 `ETag`/`Last-Modified`，或副本是加
  该字段之前写下的历史记录）：本地不猜，这一次透传上游（`MISS`）；
- `Range` 与 `If-Range` 一律透传，本地不处理任何分段。

未命中时的 HEAD 与条件请求也不会写缓存——它们可能拿到 `304` 或一个由上游判断的
`200`，不能当成一份完整副本存下来。

### git clone

开了 `git` 协议的上游，仓库地址照同一条规则改写：

```bash
git clone http://localhost:8080/github.com/CodFrm/katch.git
```

第一次拉取原样穿透上游，katch 同时在后台把这个仓库镜像到本地；镜像建成之后，
之后的 clone / fetch 由本地那份应答，一个字节都不再问上游。这一次是谁答的写在
响应头 `X-Katch-Git` 上，取值 `local` 或 `passthrough`：

```bash
curl -sI "http://localhost:8080/github.com/CodFrm/katch.git/info/refs?service=git-upload-pack" \
  | grep -i x-katch-git
```

refs 的新鲜度按上游记录上的 `mutable_ttl_seconds` 算：TTL 内直接用本地那份，
超了先增量同步一次再广播，同步失败就这一次穿透上游，本地镜像照旧可用。

几件本地答不了的事会**自动降级为穿透**，不会失败：`--depth`（浅克隆）、
`--filter`（部分克隆），以及体积超过单仓上限的仓库。push 一律 403——katch 是
镜像不是代码托管。镜像占多少盘由设置里的 `git_mirror_quota_bytes` 管着。

## 管理界面

| 页面 | 内容 |
| --- | --- |
| 首页 | 拉取助手：写下 `redis:7` 直接给出可执行的命令；站点命中率、已缓存体积、支持的上游 |
| 概览 | 请求量与命中率曲线、回源原因分布、上游健康矩阵、事件流、最近请求 |
| 上游 | 增删改、启停，改完下一个请求就生效 |
| 缓存对象 | 搜索、清理、锁定单个对象 |
| git 镜像 | 已建成的仓库镜像：状态、体积、最后同步与最后访问时间，可删除单个镜像 |
| 设置 | 站点名片、首页是否公开、缓存配额与回收水位、回源并发/超时/重试、git 镜像配额与单仓上限、管理密钥轮换 |

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
- [docs/deploy.md](docs/deploy.md) — 部署（compose / helm / 裸 manifests）、反代要调什么、排障对照表
- [docs/architecture.md](docs/architecture.md) — 分层、路由命名空间、拉取路径、数据库
- [docs/frontend.md](docs/frontend.md) — 前端目录、i18n、静态资源缓存
- [docs/observability.md](docs/observability.md) — 日志与指标
- [docs/specs/](docs/specs/) — 需求与设计决策
