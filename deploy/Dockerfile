# ── 前端构建 ──────────────────────────────────────────────────────────
# 钉在 BUILDPLATFORM 上：pnpm build 吐出来的是一堆 JS，没有架构之分。不钉的话
# buildx 会按目标架构跑这一整个 stage，交叉构建 arm64 时整个 Node 都在模拟层里。
FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /app/frontend
RUN corepack enable
# 先只拷依赖清单，让依赖层能被缓存：改源码不必重装依赖。
COPY frontend/package.json frontend/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY frontend/ ./
RUN pnpm build

# ── 后端构建 ──────────────────────────────────────────────────────────
# 同样钉在 BUILDPLATFORM：go 自己就会交叉编译，让编译器原生跑、只把 GOARCH 换掉，
# 比让整个工具链在模拟层里跑快一个数量级。
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 从 web stage 干净地拷产物，而不是用宿主机上可能残留旧 chunk 的目录。
RUN rm -rf internal/web/dist && mkdir -p internal/web/dist
COPY --from=web /app/frontend/dist/ internal/web/dist/

ARG VERSION=dev
ARG COMMIT=unknown
# TARGETARCH 由 buildkit 按目标平台自动填（amd64 / arm64），单平台的 docker build
# 也会填成宿主机的架构，所以本地 make docker 照旧。
ARG TARGETARCH
# CGO_ENABLED=0 既产出静态二进制，也是「sqlite 驱动必须保持纯 Go」这道闸：
# 引入 cgo 依赖时这里会编译失败。它同时是交叉编译能成立的前提——一旦有人引进 cgo，
# 这里不只是编译失败，而是连「原生编译器 + 换 GOARCH」这条路都走不通了。
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -ldflags "-s -w \
      -X github.com/CodFrm/katch/internal/buildinfo.Version=${VERSION} \
      -X github.com/CodFrm/katch/internal/buildinfo.Commit=${COMMIT}" \
      -o /out/katch ./cmd/katch

# ── 运行 ──────────────────────────────────────────────────────────────
FROM alpine:3.21
# ca-certificates 不可省：katch 全部的回源都是 HTTPS，缺了它每个上游都会
# 报 x509: certificate signed by unknown authority。
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /out/katch /app/katch
# 拷的是入库的 .example：configs/config.yaml 是开发者本地那一份，被 .gitignore 和
# .dockerignore 一起挡在构建上下文外，镜像里放的必须是出厂默认值。
COPY configs/config.yaml.example /app/configs/config.yaml
# sqlite 库文件与缓存都落在这里，部署时挂卷。
VOLUME ["/app/data"]
EXPOSE 8080
ENTRYPOINT ["/app/katch"]
