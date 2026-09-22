# AGENTS.md

katch 是一个多上游镜像代理站：把 container registry、APT 源、Go module proxy、
GitHub 静态资源统一收到一个域名下，路径的第一段就是上游主机名。

```
https://<katch>/docker.io/library/redis:7
https://<katch>/deb.debian.org/debian
https://<katch>/proxy.golang.org
https://<katch>/raw.githubusercontent.com/foo/bar/main/x.sh
```

使用者只需要记一条规则：把 `https://` 换成 `https://<katch>/`，其余照抄。

## 技术栈

| 层 | 选型 |
| --- | --- |
| 后端 | Go 1.26 + [cago](https://github.com/cago-frame/cago)（Gin / GORM / zap / OpenTelemetry） |
| 数据库 | 默认 sqlite（纯 Go 的 `modernc.org/sqlite`），配置可切 MySQL |
| 前端 | React 19 + TypeScript + Vite + Tailwind v4 + shadcn/ui |
| 构建产物 | 单个静态二进制，前端经 `//go:embed` 嵌入 |

## 命令

```bash
make install-deps   # 安装前端依赖
make dev            # 前端 vite + 后端 go run 并行
make test           # test-backend + test-frontend，唯一的测试入口
make test-backend   # go test -race ./...
make test-frontend  # pnpm typecheck && pnpm test
make lint           # golangci-lint v2 + eslint
make fmt            # golangci-lint fmt + prettier
make build          # 前端构建 → 拷进 internal/web/dist → CGO_ENABLED=0 编译
make smoke          # 启动真实二进制验证基本行为（需先 make build）
make mock           # go generate ./...（mockgen）
```

单测聚焦某个用例：`go test -race -run TestName ./internal/web/...`，
前端：`cd frontend && pnpm test -- tests/App.test.tsx`。

## 工程约定

这些是硬约束，与任务冲突时先停下来问，不要绕开。

1. **测试先行**：先写会失败的测试，跑一次确认它以正确的理由失败，再写实现。
   修 bug 同理——没有能复现的测试就不要动手改，复现不出来要明说。
2. **产物不许引入 cgo**。sqlite 驱动必须保持纯 Go（`modernc.org/sqlite`）。
   `make build` 的 `CGO_ENABLED=0` 就是这道闸，引入 `mattn/go-sqlite3` 之类会当场编译失败。
   注意 `make test-backend` 不设这个变量——race detector 依赖 C 运行时，关掉 cgo 会直接报错，
   这限制的是产物而不是测试工具链。
3. **分层单向**：`controller → service → repository → model/entity`，反向依赖一律不允许。
   详见 [docs/architecture.md](docs/architecture.md)。
4. **日志只有一个出口**：`logger.Ctx(ctx)`。`fmt.Print*` 和标准库 `log` 由 forbidigo 拦住，
   唯一豁免是 `cmd/katch/main.go`（logger 尚未初始化的启动窗口）。见 [docs/observability.md](docs/observability.md)。
5. **界面文案一律走 `t()`**，由 `i18next/no-literal-string` 强制。见 [docs/frontend.md](docs/frontend.md)。
6. **迁移只追加不修改**：新迁移加到 `migrationList()` 末尾。已经跑过的迁移改了不会重跑，
   只会让新旧环境的表结构悄悄分叉；要修正就追加一条补丁迁移。
7. **repository 单测用 sqlmock，不连真库**；service 单测用 mockgen 注入 repo mock。
8. **改动涉及启动路径、配置或组件注册时，跑一遍 `make smoke`**。
   配置文件被框架回写、迁移列表为空导致启动 panic、组件重复注册让 `/metrics` 返回 500
   这三类问题，lint 和单元测试全都发现不了。
9. **不动与当前任务无关的文件**。看到无关的脏数据就报告，不要顺手修——
   那会把真正的改动埋掉，也会让 `git bisect` 失效。

## 提交

gitmoji 风格，提交信息用中文。**开头写 emoji 字符本身（✨ 🐛 🔧 📝 ✅ 🎉），不要写 `:sparkles:` 这类 shortcode**——shortcode 只有在 GitHub 网页上才会被渲染成图形，`git log`、终端和大多数客户端里看到的是原样的冒号串。
