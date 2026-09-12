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
	"github.com/cago-frame/cago/server/mux"

	"github.com/CodFrm/katch/internal/api"
	"github.com/CodFrm/katch/internal/bootstrap"
	"github.com/CodFrm/katch/internal/buildinfo"
	"github.com/CodFrm/katch/internal/web"
	"github.com/CodFrm/katch/migrations"
)

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
		// SPA 必须挂在 mux 之前：它注册的是 gin 的 NoRoute，而 mux.HTTP 一旦
		// 启动就不再接受新的中间件注册。
		Registry(cago.FuncComponent(web.MountSPA)).
		RegistryCancel(mux.HTTP(api.Router)).
		Start()
	if err != nil {
		log.Fatalf("server start: %v", err)
	}
}
