// katch 是一个多上游镜像代理站。
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"time"

	"github.com/cago-frame/cago"
	"github.com/cago-frame/cago/configs"
	"github.com/cago-frame/cago/database/db"

	// sqlite 驱动必须 blank import 才会注册：cago 的 db 组件默认只注册 mysql。
	// 这个驱动是纯 Go 的 modernc.org/sqlite，不引入 cgo。
	_ "github.com/cago-frame/cago/database/db/sqlite"
	"github.com/cago-frame/cago/pkg/component"
	"github.com/cago-frame/cago/pkg/gogo"
	"github.com/cago-frame/cago/pkg/logger"
	"github.com/cago-frame/cago/server/mux"
	"go.uber.org/zap"

	"github.com/CodFrm/katch/internal/api"
	"github.com/CodFrm/katch/internal/bootstrap"
	"github.com/CodFrm/katch/internal/buildinfo"
	"github.com/CodFrm/katch/internal/cache"
	"github.com/CodFrm/katch/internal/metrics"
	"github.com/CodFrm/katch/internal/proxy/backoff"
	_ "github.com/CodFrm/katch/internal/proxy/packageprofile/builtin"
	"github.com/CodFrm/katch/internal/repository/cache_repo"
	"github.com/CodFrm/katch/internal/repository/event_repo"
	"github.com/CodFrm/katch/internal/repository/git_repo"
	"github.com/CodFrm/katch/internal/repository/request_log_repo"
	"github.com/CodFrm/katch/internal/repository/rollup_repo"
	"github.com/CodFrm/katch/internal/repository/setting_repo"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
	"github.com/CodFrm/katch/internal/service/cache_svc"
	"github.com/CodFrm/katch/internal/service/git_svc"
	"github.com/CodFrm/katch/internal/service/proxy_svc"
	"github.com/CodFrm/katch/internal/service/request_svc"
	"github.com/CodFrm/katch/internal/service/setting_svc"
	"github.com/CodFrm/katch/internal/service/stat_svc"
	"github.com/CodFrm/katch/internal/service/upstream_svc"
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

// gitConfig 是 configs/config.yaml 的 git 段。
//
// 同样只有一个「进程起不来就没法从界面改」的东西：本地镜像的根目录。总配额、
// 单仓上限、同步超时与并发这些跑起来之后才生效的参数一律落 setting 表。
type gitConfig struct {
	Dir string `yaml:"dir"`
}

// defaultGitMirrorDir 配置里没写 git 段时用的镜像目录。
//
// 和缓存目录并排而不是套在它下面：两者的淘汰依据不同（对象按 LRU + 摘要，
// 镜像按最后访问时间 + 仓库粒度），混在一处会让任一侧的配额失去意义。
const defaultGitMirrorDir = "./data/mirrors"

func main() {
	// 这里还用标准库 log：cago 的 logger 要等 component.Core() 跑完才可用，
	// 在那之前只有标准库能把错误说出来。.golangci.yml 里对本文件豁免了 forbidigo。
	log.Printf("katch %s (%s) starting", buildinfo.Version, buildinfo.Commit)

	configPath := flag.String("config", "./configs/config.yaml", "配置文件路径")
	flag.Parse()

	// 上游退避状态，进程内唯一一份（决策 17，不落库）：计数中间件往里喂回源的
	// 成败，管理接口从里面读出「这个上游正在降级」。
	backoffTracker := backoff.New(backoff.Options{})
	// 包一层，让退避的**状态转换**落进事件流：界面上那条时间线要把自动告警和
	// 人为变更放在一起。包装的只是转换那一刻，状态本身仍然只有上面那一份内存态。
	// 计数中间件和回源闸拿到的必须是同一个包装：转换只有在成败被喂进来时才看得见。
	backoffGate := proxy_svc.NewEventGate(backoffTracker)
	// 闸装在回源那一缝上：上游降级期间拉取直接快速失败，不再每个请求都去等一次
	// 连不上的拨号。装在这里而不是缓存前面——缓存命中不花上游任何成本，降级期间
	// 盘上已有的副本必须照常服务。
	//
	// 回源并发上限、上游超时与重试次数不在这里给：同样是运行时项，由拉取路径
	// 每次回源时现读，改完不必重启（决策 3/4）。
	proxy_svc.Register(proxy_svc.New(proxy_svc.Options{Gate: backoffGate}))

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

	// 两个落库服务注册成真组件，而不是挂在 gogo.Go(Run) 上：框架停止时先取消
	// 所有组件的 ctx，再按注册逆序同步调 CloseHandle，最后才 gogo.Wait()。挂在
	// Run 的 ctx.Done 分支里落库会和框架关库赛跑（实测报 database is closed 并
	// 偶发丢最后一批）；作为组件注册在仓储装配之后，CloseHandle 同步落退出那批，
	// 排在数据库关闭之前。注册顺序仍由下面链条里的位置决定。
	statSvc := stat_svc.New(stat_svc.Options{Degraded: backoffTracker})
	stat_svc.Register(statSvc)
	requestSvc := request_svc.New(request_svc.Options{})
	request_svc.Register(requestSvc)

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
			// rewrite 快照与 generation 必须落在同一个真实数据库事务里；
			// RegisterUpstream 的隔离实现只用于兼容旧测试注入。
			upstream_repo.RegisterRewriteConfig(upstream_repo.NewRewriteConfig(db.Default()))
			// 设置表同样包一层进程内缓存，理由和上游表一样：每次回源都要读一遍
			// 超时/并发/重试，每次写缓存都要读一遍配额，逐个请求查库等于把吞吐
			// 绑在 sqlite 上。缓存在管理接口写设置时自动失效，所以界面上改完
			// 下一个请求就按新值走。
			setting_repo.RegisterSetting(setting_svc.NewCachedSettingRepo(setting_repo.NewSetting()))
			cache_repo.RegisterCacheObject(cache_repo.NewCacheObject())
			rollup_repo.RegisterTrafficRollup(rollup_repo.NewTrafficRollup())
			request_log_repo.RegisterRequestLog(request_log_repo.NewRequestLog())
			git_repo.RegisterGitMirror(git_repo.NewGitMirror())
			// 事件仓储不装配的话，退避转换、缓存回收和管理写入都记不下来——
			// event_svc 遇到 nil 是丢掉事件并留一条日志，不会连累被观察的操作。
			event_repo.RegisterEvent(event_repo.NewEvent())
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
			// 配额、回收水位、可变对象 TTL 不在这里给：它们是 setting 表里的
			// 运行时项，由缓存层每次用到时现读（决策 3/4）。
			cache_svc.Register(cache_svc.New(store, cache_svc.Options{}))
			gogo.Go(func() error {
				// 定期收走过期的可变对象并按当下的配额回收一次。
				// 没有这一趟，一个再也没人来取的过期对象会连记录带字节一直留着，
				// 还一直算进配额，而它又进不了 LRU 的候选（只挑不可变的）。
				runCacheSweep(ctx)
				// ctx 结束就是进程要停了。回源下载脱离客户端跑（决策 9），此刻
				// 盘上可能正躺着一个刚提交、记录还没写完的对象；直接退出会把它
				// 留成一份谁也查不到的字节。给存量一个有上限的收尾窗口——
				// 上限是必须的：一个卡在上游那边的大对象不该把退出拖到天荒地老。
				drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cacheDrainTimeout)
				defer cancel()
				if err := cache_svc.Cache().Quiesce(drainCtx); err != nil {
					logger.Ctx(ctx).Warn("仍有后台缓存写入没能在退出前收尾", zap.Error(err))
				}
				return nil
			})
			return nil
		})).
		// git 本地镜像：目录建不出来就保持出厂的「关着」形态，clone 照常穿透
		// （决策 5）。必须排在仓储装配之后——镜像记录要走 git_repo。
		Registry(cago.FuncComponent(func(ctx context.Context, cfg *configs.Config) error {
			gitCfg := gitConfig{Dir: defaultGitMirrorDir}
			has, err := cfg.Has(ctx, "git")
			if err != nil {
				return err
			}
			if has {
				if err := cfg.Scan(ctx, "git", &gitCfg); err != nil {
					return err
				}
			}
			if gitCfg.Dir == "" {
				gitCfg.Dir = defaultGitMirrorDir
			}
			// 启动时就把根目录建出来，而不是等第一次建镜像才发现写不了：
			// 那时报出来的是一条淹没在拉取日志里的失败，而这里报出来的是
			// 「这台 katch 不会建镜像」这个结论。
			if err := os.MkdirAll(gitCfg.Dir, gitMirrorDirPerm); err != nil {
				logger.Ctx(ctx).Error("镜像目录不可用，git 只穿透不建本地镜像",
					zap.String("dir", gitCfg.Dir), zap.Error(err))
				return nil
			}
			// 配额、单仓上限、同步超时与并发不在这里给：它们是 setting 表里的
			// 运行时项，由镜像层每次用到时现读（决策 3/4）。
			git_svc.Register(git_svc.New(git_svc.Options{Dir: gitCfg.Dir}))
			gogo.Go(func() error {
				// 定期把镜像总量按配额收一次，理由同 runCacheSweep：站长在设置页
				// 把配额调小之后，不该要等到下一次有人拉新仓库才回到配额之下。
				runGitMirrorSweep(ctx)
				// ctx 结束就是进程要停了。建镜像脱离请求跑，此刻可能正有一趟
				// clone 在往盘上写；直接退出会留下半个仓库，下次启动时那条
				// pending 记录会让它重来一次——但等一等更省事，也更干净。
				drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mirrorDrainTimeout)
				defer cancel()
				if err := git_svc.Mirror().Quiesce(drainCtx); err != nil {
					logger.Ctx(ctx).Warn("仍有后台建镜像没能在退出前收尾", zap.Error(err))
				}
				return nil
			})
			return nil
		})).
		// 统计：拉取路径的计数中间件 + 每分钟把进程内计数器落成分钟桶。
		// 必须排在仓储装配之后（要写 traffic_rollup）。
		Registry(statSvc).
		// 最近请求：每秒把计数中间件攒下的拉取明细落进 recent_request，
		// 并按设置里的保留期裁剪。必须排在仓储装配之后（它要写这张表）。
		Registry(requestSvc).
		Registry(metrics.Mount(metrics.Hooks{
			Gate: backoffGate,
			// 只用来判定「这个主机名该不该有自己的标签」，不是白名单那道闸——
			// 判定仍然只有 proxy_svc 一个出处。
			Lookup: func(ctx context.Context, host string) bool {
				upstream, err := upstream_svc.Upstream().FindByHost(ctx, host)
				return err == nil && upstream != nil
			},
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

// cacheSweepInterval 多久收一次过期的缓存对象。
//
// 比分钟桶疏得多：过期对象晚几分钟被收走没有任何坏处，而每分钟扫一次
// cache_object 只是在给库添活。
const cacheSweepInterval = 10 * time.Minute

// gitMirrorDirPerm 镜像根目录的权限，同 git_svc 里建各仓库目录时用的那个。
const gitMirrorDirPerm = 0o750

// gitSweepInterval 多久按配额收一次镜像。
//
// 建镜像与增量同步本身已经在每次落盘之后触发一次淘汰（决策 3/4），这里补的是
// 「配额被调小、但没人再拉新仓库」这一种情况——不必等到下一次有人来才发现
// 总量早就超了。
const gitSweepInterval = 10 * time.Minute

// mirrorDrainTimeout 退出时留给存量建镜像的收尾窗口。
//
// 比缓存那个长一些但仍然有限：一趟 clone 是分钟级的事，等它跑完等于把退出
// 交给上游决定。等不到就记一条日志走人——那条记录还停在 pending，下次启动
// 时的第一次拉取会重新驱动它。
const mirrorDrainTimeout = 60 * time.Second

// cacheDrainTimeout 退出时留给存量后台缓存写入的收尾窗口。
//
// 取一个比回源超时略长的值：正常收尾只差提交文件与写一条记录，是毫秒级的事；
// 真的等满这段时间，说明有一趟下载卡在上游那边，那就该记一条日志然后走人。
const cacheDrainTimeout = 30 * time.Second

// runCacheSweep 跑定时的过期清理，直到 ctx 结束。
//
// 启动后先跑一次再进循环：进程可能已经停了几天，重启那一刻盘上大概率躺着
// 一批早就过期的对象，等十分钟才动手没有道理。
func runCacheSweep(ctx context.Context) {
	ticker := time.NewTicker(cacheSweepInterval)
	defer ticker.Stop()
	for {
		if removed, err := cache_svc.Cache().Sweep(ctx); err != nil {
			logger.Ctx(ctx).Error("清理过期缓存对象失败", zap.Error(err))
		} else if removed > 0 {
			logger.Ctx(ctx).Info("清理过期缓存对象", zap.Int64("removed", removed))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runGitMirrorSweep 跑定时的配额回收，直到 ctx 结束。
//
// 和 runCacheSweep 同一个形状：启动后先跑一次再进循环，进程可能已经停了几天，
// 重启那一刻配额是否超了不该等十分钟才知道。
func runGitMirrorSweep(ctx context.Context) {
	ticker := time.NewTicker(gitSweepInterval)
	defer ticker.Stop()
	for {
		if removed, err := git_svc.Sweep(ctx); err != nil {
			logger.Ctx(ctx).Error("按配额回收镜像失败", zap.Error(err))
		} else if removed > 0 {
			logger.Ctx(ctx).Info("按配额回收镜像", zap.Int64("removed", removed))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
