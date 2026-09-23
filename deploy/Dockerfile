# ── 运行 ──────────────────────────────────────────────────────────────
FROM alpine:3.21
# ca-certificates 不可省：katch 全部的回源都是 HTTPS，缺了它每个上游都会
# 报 x509: certificate signed by unknown authority。
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
# 二进制由 CI runner 在可用 GOPROXY 的宿主环境中构建，避免在 BuildKit
# 容器里重新下载 Go 依赖。make build 已经完成前端嵌入和 CGO_ENABLED=0 闸门。
COPY bin/katch /app/katch
# 拷的是入库的 .example：configs/config.yaml 是开发者本地那一份，被 .gitignore 和
# .dockerignore 一起挡在构建上下文外，镜像里放的必须是出厂默认值。
COPY configs/config.yaml.example /app/configs/config.yaml
# sqlite 库文件与缓存都落在这里，部署时挂卷。
VOLUME ["/app/data"]
EXPOSE 8080
ENTRYPOINT ["/app/katch"]
