# ── 前端构建 ──────────────────────────────────────────────────────────
FROM node:22-alpine AS web
WORKDIR /app/frontend
RUN corepack enable
# 先只拷依赖清单，让依赖层能被缓存：改源码不必重装依赖。
COPY frontend/package.json frontend/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY frontend/ ./
RUN pnpm build

# ── 后端构建 ──────────────────────────────────────────────────────────
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 从 web stage 干净地拷产物，而不是用宿主机上可能残留旧 chunk 的目录。
RUN rm -rf internal/web/dist && mkdir -p internal/web/dist
COPY --from=web /app/frontend/dist/ internal/web/dist/

ARG VERSION=dev
ARG COMMIT=unknown
# CGO_ENABLED=0 既产出静态二进制，也是「sqlite 驱动必须保持纯 Go」这道闸：
# 引入 cgo 依赖时这里会编译失败。
RUN CGO_ENABLED=0 go build -ldflags "-s -w \
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
