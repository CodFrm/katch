# deploy/

部署资源。**完整说明在 [../docs/deploy.md](../docs/deploy.md)**，这里只是目录索引。

| | 用途 |
| --- | --- |
| `Dockerfile` | 三段式构建：前端 → 后端 → alpine 运行期。`make docker` 用它 |
| `config.yaml.example` | 容器部署的配置模板。拷成 `config.yaml`（gitignore 挡着）再改密钥 |
| `docker-compose.yml` | 单机：一个容器 + 一个卷 |
| `helm/katch/` | Helm chart |
| `kubernetes/` | 裸 manifests，`kubectl apply -k` |

这些文件不参与编译，也没有闸盯着。改完自己渲染一遍核对（`helm template`、
`kubectl kustomize`、`docker compose config`）。

三条硬约束，动手之前先看 [docs/deploy.md](../docs/deploy.md#先读这一节三条硬约束)：

1. **只能一个副本**——进程内的上游白名单和设置缓存没有 TTL，只在本进程写入时失效；
2. **滚动更新必须换成 `Recreate`**——RWO 的卷会把新旧 Pod 锁死；
3. **退出要给 45 秒**——后台回源下载的收尾窗口上限是 30 秒。
