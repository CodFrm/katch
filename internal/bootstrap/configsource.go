// Package bootstrap 装配 katch 的启动期依赖。
package bootstrap

import (
	"context"
	"fmt"
	"os"

	"github.com/cago-frame/cago/configs/source"
	"gopkg.in/yaml.v3"
)

// readOnlyYAMLSource 是只读的 YAML 配置源。
//
// 存在的理由是 cago 自带的文件源有个副作用：Scan 到一个不存在的 key 时，它会把
// 零值写进内存，**再把整个配置 map 重新序列化覆写回文件**，最后才返回 not found。
// 于是只要有任何一个组件读了配置里没写的 key：
//
//   - configs/config.yaml 里的注释和键顺序全部丢失；
//   - 本地跑一次服务就在 git 工作区留下一份无意义的配置改动，很容易被误提交；
//   - 文件里还会多出一批零值键（比如 source: ""），看起来像是有人特意配的。
//
// 配置文件是入库的、带解释的资产，不该被进程改写。这里只保留「读」。
type readOnlyYAMLSource struct {
	config map[string]any
}

// NewConfigSource 从 path 读取 YAML 配置，返回一个不会回写文件的配置源。
func NewConfigSource(path string) (source.Source, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	cfg := map[string]any{}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	return &readOnlyYAMLSource{config: cfg}, nil
}

// Scan 读取 key 对应的配置。key 不存在时返回 source.ErrNotFound，不写文件。
//
// 错误信息保持和 cago 文件源一致的 "file <err>: <key>" 形状：调用方（包括 cago
// 自身）是按 errors.Is(err, source.ErrNotFound) 判断的，文案一致只是为了排障时
// 两者的日志读起来一样。
func (s *readOnlyYAMLSource) Scan(_ context.Context, key string, value any) error {
	raw, ok := s.config[key]
	if !ok {
		return fmt.Errorf("file %w: %s", source.ErrNotFound, key)
	}
	// 先 Marshal 再 Unmarshal 到目标类型，和 cago 文件源同样的做法：配置值在
	// map 里是 any，没法直接赋给调用方给的具体结构体。
	b, err := yaml.Marshal(raw)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(b, value)
}

// Has 报告 key 是否存在。
func (s *readOnlyYAMLSource) Has(_ context.Context, key string) (bool, error) {
	_, ok := s.config[key]
	return ok, nil
}

// Watch 不做任何事：katch 还没有配置热更新的需求，cago 自带的文件源同样是空实现。
func (s *readOnlyYAMLSource) Watch(_ context.Context, _ string, _ func(event source.Event)) error {
	return nil
}
