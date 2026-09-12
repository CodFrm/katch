// katch 是一个多上游镜像代理站。
package main

import (
	"context"
	"flag"
	"log"

	"github.com/cago-frame/cago"
	"github.com/cago-frame/cago/configs"
	"github.com/cago-frame/cago/database/db"

	// sqlite 驱动必须 blank import 才会注册：cago 的 db 组件默认只注册 mysql。
	// 这个驱动是纯 Go 的 modernc.org/sqlite，不引入 cgo。
	_ "github.com/cago-frame/cago/database/db/sqlite"
	"github.com/cago-frame/cago/pkg/component"
	"github.com/cago-frame/cago/pkg/logger"
	"github.com/cago-frame/cago/server/mux"
	"go.uber.org/zap"

	"github.com/CodFrm/katch/internal/api"
	"github.com/CodFrm/katch/internal/bootstrap"
	"github.com/CodFrm/katch/internal/buildinfo"
	"github.com/CodFrm/katch/internal/cache"
	"github.com/CodFrm/katch/internal/repository/cache_repo"
	"github.com/CodFrm/katch/internal/repository/setting_repo"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
	"github.com/CodFrm/katch/internal/service/cache_svc"
	"github.com/CodFrm/katch/internal/service/proxy_svc"
	"github.com/CodFrm/katch/internal/service/setting_svc"
	"github.com/CodFrm/katch/internal/web"
	"github.com/CodFrm/katch/migrations"
)

// adminConfig 是 configs/config.yaml 的 admin 段。
//
// 这里只有初始密钥：按「进程起不来就没法从界面改的东西才进配置文件」的边界，
// 首次启动必须有办法登进去，所以它在文件里；之后的轮换在界面上完成。
type adminConfig struct {
	InitialKey string `yaml:"initialKey"`
}

// cacheConfig 是 configs/config.yaml 的 cache 段。
//
// 同样只有一个「进程起不来就没法从界面改」的东西：缓存目录。配额、回收水位、
// 可变对象 TTL 这些跑起来之后才生效的参数一律落 setting 表。
type cacheConfig struct {
	Dir string `yaml:"dir"`
}

// defaultCacheDir 配置里没写 cache 段时用的缓存目录。
const defaultCacheDir = "./data/cache"

func main() {
	// 这里还用标准库 log：cago 的 logger 要等 component.Core() 跑完才可用，
	// 在那之前只有标准库能把错误说出来。.golangci.yml 里对本文件豁免了 forbidigo。
	log.Printf("katch %s (%s) starting", buildinfo.Version, buildinfo.Commit)

	configPath := flag.String("config", "./configs/config.yaml", "配置文件路径")
	flag.Parse()

	ctx := context.Background()
	// 自己装配配置源，而不是让 cago 用默认的文件源：默认那个在读到配置里没写的
	// key 时会覆写整个配置文件，把注释和键顺序一起擦掉。详见 bootstrap 包。
	src, err := bootstrap.NewConfigSource(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	cfg, err := configs.NewConfig("katch", configs.WithSource(src))
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	err = cago.New(ctx, cfg).
		// Core 内部已经初始化了 logger 和 metric，**不要**再单独注册 metric.Metrics：
		// 那会让 otel 的 prometheus exporter 创建两次、双双注册进默认 registry，
		// 于是 target_info 被重复收集，GET /metrics 直接返回 500。
		// metric 组件自行挂上 gin 中间件并暴露 /metrics，无需额外配置。
		Registry(component.Core()).
		Registry(component.Database()).
		Registry(cago.FuncComponent(func(_ context.Context, _ *configs.Config) error {
			return migrations.RunMigrations(db.Default())
		})).
		// 仓储的实现在这里装配（DIP：service 只认接口），随后按决策 18 处理
		// 初始管理密钥。必须排在迁移之后——它要读写 setting 表。
		Registry(cago.FuncComponent(func(ctx context.Context, cfg *configs.Config) error {
			// 上游仓储包一层进程内缓存：拉取是热路径（一次 docker pull 是几十上百
			// 个请求），每个请求查一次库等于把镜像站的吞吐绑在 sqlite 上。缓存在
			// 管理接口写入上游时自动失效，所以界面上改完不必重启。
			upstream_repo.RegisterUpstream(proxy_svc.NewCachedUpstreamRepo(upstream_repo.NewUpstream()))
			setting_repo.RegisterSetting(setting_repo.NewSetting())
			cache_repo.RegisterCacheObject(cache_repo.NewCacheObject())
			admin := adminConfig{}
			has, err := cfg.Has(ctx, "admin")
			if err != nil {
				return err
			}
			if has {
				if err := cfg.Scan(ctx, "admin", &admin); err != nil {
					return err
				}
			}
			// 没配初始密钥不阻断启动：管理接口会全量 401，拉取路径照常服务。
			return setting_svc.Setting().EnsureAdminKey(ctx, admin.InitialKey)
		})).
		// 缓存层：目录打不开就保持 cache_svc 出厂的纯透传形态，服务照常起来。
		// 缓存是优化，它坏掉不该让拉取整体失败（失败与降级）。必须排在仓储装配
		// 之后——缓存记录要走 cache_repo。
		Registry(cago.FuncComponent(func(ctx context.Context, cfg *configs.Config) error {
			cacheCfg := cacheConfig{Dir: defaultCacheDir}
			has, err := cfg.Has(ctx, "cache")
			if err != nil {
				return err
			}
			if has {
				if err := cfg.Scan(ctx, "cache", &cacheCfg); err != nil {
					return err
				}
			}
			if cacheCfg.Dir == "" {
				cacheCfg.Dir = defaultCacheDir
			}
			store, err := cache.NewStore(cacheCfg.Dir)
			if err != nil {
				logger.Ctx(ctx).Error("缓存目录不可用，降级为纯透传",
					zap.String("dir", cacheCfg.Dir), zap.Error(err))
				return nil
			}
			cache_svc.Register(cache_svc.New(store, cache_svc.Options{}))
			return nil
		})).
		// SPA 与拉取路径共用这一个 NoRoute，必须挂在 mux 之前：它注册的是 gin 的
		// NoRoute，而 mux.HTTP 一旦启动就不再接受新的中间件注册。
		Registry(cago.FuncComponent(web.MountSPA)).
		RegistryCancel(mux.HTTP(api.Router)).
		Start()
	if err != nil {
		log.Fatalf("server start: %v", err)
	}
}
