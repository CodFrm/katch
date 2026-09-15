# 部署

katch 是**一个静态二进制**，前端经 `//go:embed` 编在里面、由后端同源提供。
部署一台 katch 就是跑一个进程 + 挂一个卷，不需要 nginx、不需要外部数据库、
不需要 Prometheus。

`deploy/` 下有三条路，选一条：

| | 适合 | 入口 |
| --- | --- | --- |
| Docker Compose | 单机 | [`deploy/docker-compose.yml`](../deploy/docker-compose.yml) |
| Helm | k8s，想用 values 管配置 | [`deploy/helm/katch/`](../deploy/helm/katch/) |
| 裸 manifests | k8s，不想引入 Helm | [`deploy/kubernetes/`](../deploy/kubernetes/) |

`deploy/` 下的东西不参与编译，也没有任何闸盯着，写错了要等到真去部署那天才知道。改完
只有自己渲染一遍核对。

## 先读这一节：三条硬约束

这三条不是调优建议，违反任何一条都会在生产上表现为难查的故障。

### 为什么只能一个副本

katch 把**上游白名单**和**设置表**缓存在进程内
（`proxy_svc.NewCachedUpstreamRepo`、`setting_svc.NewCachedSettingRepo`）。
拉取是热路径——一次 `docker pull` 是几十上百个请求，每个请求都查一次库等于把镜像站的
吞吐绑在 sqlite 上，所以这层缓存是必需的。

代价是：**这两层缓存都没有 TTL，只在本进程发生写入时失效。**

于是多开一个副本之后，你在界面上（请求落到副本 A）停用一个上游，副本 B 的快照
不会收到任何通知——它会**永远**继续代理那个上游，直到那个 Pod 因为别的原因被重建。
症状是「界面上明明停了，可还在回源」，而且只有一半的请求会这样。

换 MySQL 解决不了这件事：失效是进程内的事，和数据存在哪没关系。要横向扩，
得先让这两层缓存能跨进程失效。在那之前：

- Helm chart **没有 `replicaCount`**，模板里写死 `replicas: 1`；
- 裸 manifests 同样写死 1。

单副本对镜像站来说没有看起来那么糟：拉取是只读的，缓存命中不花回源成本，
瓶颈通常在上游带宽而不是 katch 自己。

### 为什么滚动更新必须换成 Recreate

数据卷是 `ReadWriteOnce`，同一时刻只能被一个节点挂上。默认的 `RollingUpdate` 会让
新 Pod 卡在 `ContainerCreating` 等旧 Pod 放开卷，而旧 Pod 要等新 Pod 就绪才会退——
死锁。而且它不报错，只是「升级一直没完成」。

所以 `strategy.type: Recreate`。代价是升级期间有几十秒的中断，这对镜像站可以接受
（客户端重试即可），对换来的确定性是划算的。

### 为什么退出要给 45 秒

katch 的回源下载是**脱离客户端**跑的：客户端断开了，那趟下载还会继续跑完并落盘，
下一个人来取就能命中。代价是进程退出那一刻，盘上可能正躺着一个刚提交、记录还没
写完的对象。`cmd/katch/main.go` 因此留了一个收尾窗口，上限是
`cacheDrainTimeout = 30s`。

k8s 的 `terminationGracePeriodSeconds` 默认 30 秒、compose 的 `stop_grace_period`
默认 10 秒，两个都会把这段收尾拦腰砍断，在盘上留下一份谁也查不到的字节。
两边都调到了 **45 秒**（30 秒收尾 + 余量）。

## Docker Compose

```bash
cp deploy/config.yaml.example deploy/config.yaml
# 把 admin.initialKey 换成 openssl rand -hex 24 生成的值
docker compose -f deploy/docker-compose.yml up -d
```

`deploy/config.yaml` 被 `.gitignore` 挡着，理由和 `configs/config.yaml` 一样：
里面有真的管理密钥和 DSN。

打开 `http://<机器>:8080`，后台在 `/admin`。

编排里只有一个服务和一个卷。几个不显然的地方都在文件里写了注释：
`read_only: true` + `tmpfs: /tmp`（进程要写的只有数据卷）、`stop_grace_period: 45s`、
healthcheck 打的是 `/api/v1/system/version`、以及给容器 stdout 的日志轮转上限。

**这里没有 MySQL。** 默认 sqlite 就是卷里的一个文件，而切 MySQL 也换不来多副本
（上一节），编排里摆一个只会让人误以为可以。真要切，改 `deploy/config.yaml` 的
`db` 段就行。

## Helm

```bash
helm install katch deploy/helm/katch \
  --namespace katch --create-namespace \
  --set config.admin.initialKey=$(openssl rand -hex 24)
```

想从界面访问：

```bash
kubectl -n katch port-forward svc/katch 8080:8080
```

### 配置怎么给

`values.yaml` 里 `config:` 段下面的东西会被**原样序列化**成容器里的
`/app/configs/config.yaml`，装在一个 Secret 里（里面有管理密钥和 DSN，所以是
Secret 不是 ConfigMap）。改其中一项就用 `--set`：

```bash
helm upgrade katch deploy/helm/katch \
  --set config.logger.level=debug \
  --set config.db.driver=mysql \
  --set config.db.dsn='user:pass@tcp(mysql:3306)/katch?charset=utf8mb4&parseTime=True&loc=Local'
```

想完全自己管这份文件（External Secrets、Sealed Secrets 之类），用
`--set existingSecret=my-secret`，Secret 里的键名必须是 `config.yaml`；
那时 `config:` 段整个不参与渲染。

Deployment 上有一条 `checksum/config` 注解：配置一变 Pod 就重建。没有它，
`helm upgrade` 改掉密钥之后 Pod 照旧跑着老配置，而 helm 会告诉你升级成功了。

### 存储

一个 PVC 挂在 `/app/data`，里面是 sqlite 库文件、缓存对象、日志三样。

- **日志也在卷里**（`config.logger.logFile.filename: ./data/logs/katch.log`）：
  根文件系统是只读的，进程能写的地方只剩这一个挂载点。
- **没有用 subPath。** kubelet 建出来的 subPath 目录是 `root:root 0755`，
  `fsGroup` 管不到它，而容器以 65532 跑——写不进去，症状是启动即 panic。
- PVC 带 `helm.sh/resource-policy: keep`：一次手滑的 `helm uninstall` 不该把攒了
  很久的缓存和统计历史带走。要删就显式 `kubectl delete pvc`。
- **卷的大小要大于界面里配的缓存配额。** katch 按配额回收，撑破的是卷而不是配额。

### 安全上下文

默认以 uid 65532 跑，根文件系统只读，`drop: [ALL]`。

镜像里没有建非 root 用户、`/app` 是 root 的，能跑起来全靠 `fsGroup: 65532`——
kubelet 会把卷 chgrp 成这个组并加上组写权限，而进程要写的只有那个卷。
**改 `runAsUser` 时 `fsGroup` 要跟着改**，否则新用户写不进数据卷。

`/tmp` 是一个 emptyDir：根文件系统只读，而 sqlite 在排序、VACUUM 这类操作上可能
落临时文件到 `TMPDIR`。缓存对象的临时文件不走这里（在缓存目录下的 `tmp/`）。

### 探针

三个探针打的都是 `/api/v1/system/version`。这是刻意选的：它只读编译期注入的
`buildinfo`，**一个字节都不碰数据库**。

库挂掉的时候，katch 的拉取路径仍然靠磁盘缓存服务（管理接口那边会答 503）。
如果探针打一个查库的端点，库一抖 Pod 就被判死重启，等于把唯一还能用的那部分也
弄没了。库的健康状况在界面上看，不在探针上看。

`startupProbe` 给了 60 秒（30 × 2s）：首次启动要跑完全部迁移。

## 裸 manifests

```bash
# 先改 deploy/kubernetes/secret.yaml 里的 admin.initialKey
kubectl apply -k deploy/kubernetes/
```

和 chart 默认值渲染出来的东西等价，只是把值写死了。要 Ingress 的话，改掉
`ingress.yaml` 里的 host，再在 `kustomization.yaml` 的 `resources` 里放开那一行。

这组文件入库就带着占位密钥，所以别把改完的 `secret.yaml` 提交回来——用 kustomize 的
`secretGenerator` 从集群外的文件生成，或者换成 External Secrets / Sealed Secrets。

**它和 chart 是两份，改了一边记得看另一边**——没有任何东西会告诉你两边已经不一致了。

## 放在反代后面

katch 自己不做 TLS，要 HTTPS 就在前面加一层。那一层有两件事必须调，否则症状
不是「不能用」而是「巨慢」或者「大文件传一半断」：

1. **关掉响应缓冲。** 不关的话，一个几 GB 的镜像层会先被反代整个收下来再转给客户端，
   既把首字节推迟到整体下载完成，又能把反代的盘写满。katch 这边是 `io.Copy` 流式
   转发的，缓冲会把这个设计整个抵消掉。
2. **把读写超时调大。** 大对象在慢网络上会传很久，nginx 默认 60 秒会在中途把连接
   切断，客户端看到的是一个下到一半的文件。

ingress-nginx 的写法（chart 和裸 manifests 里都已经是默认值）：

```yaml
nginx.ingress.kubernetes.io/proxy-buffering: "off"
nginx.ingress.kubernetes.io/proxy-read-timeout: "900"
nginx.ingress.kubernetes.io/proxy-send-timeout: "900"
nginx.ingress.kubernetes.io/proxy-body-size: "0"
```

独立 nginx 的对应写法是 `proxy_buffering off;`、`proxy_read_timeout 900s;`、
`client_max_body_size 0;`。

还有一条：**反代必须把整个 `/` 都转给 katch**，不能按前缀挑。路径的第一段就是上游
主机名，挑路径等于把上游挑着代理，而且每加一个上游都要回来改一次反代配置。

## 装完之后

**上游表出厂是空的，而它同时就是白名单**——不加记录，任何主机都是 404。
在后台「上游 → 添加上游」里加，或者：

```bash
curl -X POST http://<katch>/api/v1/admin/upstreams \
  -H "Authorization: Bearer <管理密钥>" -H 'Content-Type: application/json' \
  -d '{"host":"docker.io","protocols":["registry"],"origin":"https://registry-1.docker.io",
       "enabled":true,"library_completion":true,"mutable_ttl_seconds":300}'
```

配额、回收水位、可变对象 TTL、回源并发/超时/重试都是**运行时项**，在界面的
「设置」里改，改完不用重启——它们不在配置文件里，那条边界见
[architecture.md](architecture.md#配置的边界与管理密钥)。

管理密钥以 bcrypt 哈希存在库里，`admin.initialKey` **只在库中尚无密钥时落一次**。
也就是说改配置救不回一个已经轮换过又忘掉的密钥——那种情况只能清掉 `setting` 表里
的 `admin_key_hash`。轮换在界面上做。

## 监控

`/metrics` 一直都在，由 cago 的 metric 组件自动挂载，没有开关。
chart 里 `metrics.serviceMonitor.enabled=true` 可以生成一个 ServiceMonitor
（需要集群里有 Prometheus Operator）。

**界面上的命中率不依赖 Prometheus**：那是库里的分钟桶（决策 16），
装不装外部监控都不影响后台能看。

## 升级

```bash
# compose
docker compose -f deploy/docker-compose.yml pull
docker compose -f deploy/docker-compose.yml up -d

# helm
helm upgrade katch deploy/helm/katch --reuse-values --set image.tag=0.2.0
```

迁移在启动时自动跑，只追加不修改（[architecture.md](architecture.md#数据库)）。
升级期间有几十秒中断，原因见上面「为什么滚动更新必须换成 Recreate」。

## 排障

| 症状 | 多半是 |
| --- | --- |
| 启动就 panic，`unable to open database file: out of memory (14)` | 数据卷没挂上。这是 sqlite 在说**父目录不存在**，那句话里一个字都没提目录。`cache/` 和 `logs/` 进程会自己建，但 `./data` 本身不会 |
| 服务正常，命中率一直是 0 | 缓存目录不可写。katch 会降级成纯透传（拉取照常，只是不缓存）而不是崩掉，所以只能从这个数字上看出来 |
| 拉取全是 404，响应体是空的 | 主机不在上游表里，或者被停用了。这三种情况对外故意长得一模一样——任何可观察的差别都在告诉探测者「这台主机存在，只是被停用了」 |
| 后台全是 401 | `admin.initialKey` 没配，或者库里已经有一个轮换过的密钥了（配置文件覆盖不了它） |
| 后台「最近请求」整块是空的 | 这个上游确实没被拉过，或者历史已经被 `recent_request_retention_seconds` 裁掉。日志开关、日志文件在不在卷里都不影响这块面板 |
| 升级卡在 `ContainerCreating` | `strategy` 不是 `Recreate`，RWO 的卷把新旧 Pod 锁死了 |
| 界面上停用了上游，可还在回源 | 开了多副本。见「为什么只能一个副本」 |
| 大文件传一半断 / 首字节特别慢 | 反代的响应缓冲没关、超时没调大。见「放在反代后面」 |
