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

`internal/api/` 只放请求/响应结构与路由注册，不含逻辑。
横切层（`pkg/`）不得反向引用 service / repository。

新领域开新的一组包（`<domain>_entity` / `_repo` / `_svc`），不要往已有领域里塞。

## 路由命名空间

katch 的 HTTP 路径被两类完全不同的东西共用，必须分清：

| 路径 | 归属 |
| --- | --- |
| `/api/v1/...` | katch 自身的管理接口（`internal/api/router.go`） |
| `/metrics` | cago 的 metric 组件自动挂载 |
| `/v2/...` | container registry 协议（客户端固定请求这个前缀，上游主机名在它**之后**） |
| `/<上游主机名>/...` | 其余上游（APT、Go proxy、GitHub 静态资源等） |
| 其余 | 前端 SPA 路由，回落 index.html |

分辨保留段和上游段的规则是**点号**：第一段含 `.` 的是上游主机名（公网主机名必然含点），
不含 `.` 的是 katch 自己的保留路径。这条规则不会随着上游增加而退化。

`/api/` 和 `/assets/` 下未命中一律 404，不回落 index.html——理由见
[frontend.md](frontend.md#静态资源缓存)。

## 数据库

默认 sqlite，配置可切 MySQL，**因此迁移的 DDL 必须两种方言都成立**，不能随手用
MySQL 特有语法。DDL 优先写原生 SQL 而不是依赖 gorm 的 AutoMigrate。

迁移只追加不修改：新迁移加到 `migrations.migrationList()` 末尾。已经在环境里跑过的
迁移即使改了也不会重跑，只会让新旧环境的表结构悄悄分叉；需要修正时追加一条补丁迁移。

sqlite 的 DSN 里两个 pragma 不能省（见 `configs/config.yaml` 的注释）：
`journal_mode(WAL)` 让读不阻塞写，`busy_timeout` 让遇锁时等待而不是立刻返回
`SQLITE_BUSY`——缺了它并发写会直接报 "database is locked"。

## 上游适配

第一版覆盖 container registry、APT、Go module proxy、GitHub 静态资源，共用一套
「按上游主机名分发 + 内容寻址缓存」的骨架；registry 因为鉴权和路径形态特殊，
单独成一类。具体的接口形态、缓存键与回源策略由该轮的 spec 确定，这里不预先假定。

一条已经确定的原则：上游配置以**主机名**为 key，加一个新上游应当是加一行配置，
而不是加一个新的路径前缀约定。
