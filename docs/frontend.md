# 前端

React 19 + TypeScript + Vite + Tailwind v4 + shadcn/ui，构建产物经 `//go:embed`
嵌入 Go 二进制，生产环境由后端同源提供，不单独部署。

## 目录

```
frontend/
  src/
    components/ui/   # shadcn 生成的组件，视作上游代码，不手改
    i18n/            # i18next 初始化与语言资源
    lib/             # 纯函数工具（shadcn 的 cn 等）
  tests/             # vitest 用例与 setup
```

## 命令

```bash
pnpm dev          # 开发服务器，:5173，/api 与 /metrics 转发到后端 :8080
pnpm build        # tsc -b && vite build
pnpm typecheck    # tsc -b --noEmit
pnpm lint         # eslint . --max-warnings 0
pnpm format       # prettier --write .
pnpm test         # vitest run
```

`typecheck` 必须和 `vitest` 一起跑：vitest 走 esbuild 只转译不查类型，类型闸只挂
在 build 上的话，`make test` 会全绿而 `make build` 红，等于没有闸。

## i18n

界面文案一律走 `t()`，由 `i18next/no-literal-string` 以 error 级别强制：

```tsx
const { t } = useTranslation()
return <h1>{t('app.name')}</h1>
```

强制它的理由是：漏掉的硬编码文案在切换语言时**不会报错**，只会安静地显示成另一种
语言，而且往往要等到用户反馈才被发现。

几类不算文案、已在规则里排除：

- 纯标点、数字、分隔符；
- 形如 `docker.io`、`deb.debian.org` 的上游主机名——它们是机器面向的字面量，翻译反而是错的；
- `src/components/ui/` 下 shadcn 生成的组件。

语言资源在 `src/i18n/locales/`，`fallbackLng` 是 `zh-CN`。新增语言只需在
`src/i18n/index.ts` 注册，组件不必改动。

测试里断言文案时要先 `await i18n.changeLanguage('zh-CN')` 固定语言，否则断言会跟着
运行环境的语言检测结果飘。

## 静态资源缓存

`internal/web/embed.go` 负责把 dist 挂到 gin 的 NoRoute 上，其中两条规则由测试锚定：

- `/assets/` 下是 vite 带内容 hash 的产物，给 `immutable` + 一年 max-age。
  embed.FS 的 ModTime 是零值，不显式给缓存头的话连 Last-Modified 和 ETag 都没有，
  每次打开页面都要把整个 bundle 重新下一遍；
- `index.html` 给 `no-cache`，它是指向当前一组 hash 的名片，被永久缓存就意味着
  滚动更新后新版本再也上不去。

`/assets/` 和 `/api/` 下未命中一律 404，不回落 index.html——回落会返回 200 + HTML，
而 200 不会被任何监控计成失败，问题就被藏在一个成功状态码里。
