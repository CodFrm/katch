.PHONY: install-deps dev build test test-backend test-frontend lint lint-backend lint-frontend \
        fmt prepare-web-dist mock docker smoke package-mirror-image test-package-mirror-harness

# 版本号显式给定，不从 git describe 反推：没打 tag 的分支上 describe 会退成 "dev"，
# 带 tag 时又多一个 "v" 前缀，两种情况对不上号。排障要的 commit 维度由 COMMIT 单独注入。
VERSION ?= 0.1.0
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w \
  -X github.com/CodFrm/katch/internal/buildinfo.Version=$(VERSION) \
  -X github.com/CodFrm/katch/internal/buildinfo.Commit=$(COMMIT)

install-deps:
	cd frontend && pnpm install

# 本地配置：入库的是 configs/config.yaml.example，configs/config.yaml 是每个人自己的
# 那一份（.gitignore 挡着）。新克隆下来没有它，go run 会直接 load config 失败。
# 只在缺失时拷：已经改过的本地配置绝不能被覆盖回出厂值，所以这是一个文件目标而不是
# 每次都执行的 phony。
configs/config.yaml: configs/config.yaml.example
	@test -f $@ || cp $< $@

dev: configs/config.yaml
	@trap 'kill 0' INT; \
	  (cd frontend && pnpm install --silent && pnpm dev) & \
	  go run ./cmd/katch & wait

# CGO_ENABLED=0 不只是为了静态链接，它同时是一道闸：sqlite 驱动必须保持纯 Go
# （modernc.org/sqlite）。哪天有人引入 mattn/go-sqlite3 这类 cgo 依赖，这里会当场
# 编译失败，而不是悄悄产出一个依赖 glibc 的二进制。
build:
	cd frontend && pnpm install --frozen-lockfile && pnpm build
	# 先清空再拷：vite 产物带内容哈希，文件名每次都不一样，只 cp 不删会把历史
	# chunk 全部留下、一起 //go:embed 进二进制。
	rm -rf internal/web/dist
	mkdir -p internal/web/dist
	cp -r frontend/dist/* internal/web/dist/
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/katch ./cmd/katch

# //go:embed 不接受空目录，也会忽略 . 开头的文件，所以入库的 .gitkeep 不算数。
# 没有这一步，go build 和 go test 都会直接报 "contains no embeddable files"。
prepare-web-dist:
	mkdir -p internal/web/dist
	@test -f internal/web/dist/index.html || printf '<!doctype html>\n' > internal/web/dist/index.html

# 唯一的测试入口。别加 build tag——tag 会让测试被静默跳过而输出仍然是绿的。
test: test-backend test-frontend

# 这里**不**设 CGO_ENABLED=0：race detector 依赖 C 运行时，关掉 cgo 会直接报
# "-race requires cgo"。这跟「产物不要 cgo」不矛盾——限制的是 make build 的产物，
# 不是测试时的工具链。
test-backend: prepare-web-dist
	go test -race ./...

# typecheck 必须和 vitest 一起跑：vitest 走 esbuild 只转译不查类型，类型闸只挂在
# build 上的话，make test 全绿而 make build 红，等于没有闸。
test-frontend:
	cd frontend && pnpm install --frozen-lockfile --silent && pnpm typecheck && pnpm test

# 启动真实二进制验证基本行为。lint 和单测都发现不了「闸全绿但服务起不来」，
# 这个项目初始化时就踩到三次。需要先 make build。
# 它同时守着扩展点那条边界：只经管理接口加一条上游记录，就要能拉通并命中缓存，
# 全程不改代码、不重启进程（docs/architecture.md 的「扩展点的边界与守卫」）。
smoke:
	./scripts/smoke.sh

lint: lint-backend lint-frontend

lint-backend: prepare-web-dist
	golangci-lint run ./...

lint-frontend:
	cd frontend && pnpm lint

fmt:
	golangci-lint fmt ./...
	cd frontend && pnpm format

mock:
	go generate ./...

# 镜像名和 deploy/ 下的编排文件保持一致：compose、helm values、裸 manifests 引用的
# 都是 IMAGE。本地 build 出来的 tag 要是对不上，compose up 会去拉一个远端的同名镜像，
# 而你以为它跑的是刚编的那个。
IMAGE ?= ghcr.io/codfrm/katch

docker:
	docker build -f deploy/Dockerfile -t $(IMAGE):$(VERSION) \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT=$(COMMIT) .

PACKAGE_MIRROR_CLIENT_IMAGE ?= katch/package-client-tools:2026-09-16
PACKAGE_MIRROR_HOMEBREW_IMAGE ?= katch/package-client-homebrew:4.6.20

package-mirror-image:
	docker build -f e2e/package-mirrors/images/client-tools.Dockerfile \
	  -t $(PACKAGE_MIRROR_CLIENT_IMAGE) .
	docker build -f e2e/package-mirrors/images/homebrew.Dockerfile \
	  -t $(PACKAGE_MIRROR_HOMEBREW_IMAGE) .

test-package-mirror-harness:
	./e2e/package-mirrors/tests/harness_test.sh
