package api

import (
	"context"

	"github.com/cago-frame/cago/server/mux"

	"github.com/CodFrm/katch/internal/controller/system_ctr"
)

// Router 注册全部 HTTP 路由。
//
// 注意：这里只挂 katch 自身的管理接口，全部在 /api/ 前缀下。镜像代理走的是
// 另一套路径（/v2/... 和 /<上游host>/...），由代理层单独接管——两者必须分开，
// 否则上游 host 和管理接口会在同一个命名空间里抢路径。
func Router(_ context.Context, root *mux.Router) error {
	r := root.Group("/api/v1")

	sysCtr := system_ctr.NewSystem()
	r.Group("/").Bind(sysCtr.Version)

	return nil
}
