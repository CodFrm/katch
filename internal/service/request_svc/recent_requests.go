package request_svc

import (
	"context"
	"errors"

	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/repository/request_log_repo"
)

// RecentRequests 某个上游最近的若干次拉取。
//
// 面板读的是 recent_request 这张表，不是日志文件的尾部（决策 2/11）：窗口由
// 保留期和条数上限决定，与流量、日志级别、文件是否落盘/有没有被轮转全部无关。
// 按 upstream_id 查，因此停用的上游已经落库的历史行仍然可见；库里没有这个上游
// 的行时给空列表，界面照现有约定整块不渲染。
//
// 库读不出来是真的错误，交给调用方翻成 503（失败与降级一节），和上游表查不出来
// 时一样——不报错、不阻塞整屏管理界面。
func (s *requestSvc) RecentRequests(ctx context.Context, req *admin.RecentRequestsRequest) (
	*admin.RecentRequestsResponse, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = admin.RecentRequestsDefaultLimit
	}
	if limit > admin.RecentRequestsMaxLimit {
		// 接口层的 binding 已经挡过一次。这里再夹一道，是因为响应大小不能由
		// 调用方决定，而服务层也会被别的调用方直接用。
		limit = admin.RecentRequestsMaxLimit
	}
	repo := request_log_repo.RecentRequestLog()
	if repo == nil {
		// 没装配就当作库读不出来：走 storageAware 那条 503，而不是在这里
		// 空指针 panic 掉整个读取请求。
		return nil, errors.New("最近请求仓储未装配")
	}
	rows, err := repo.ListRecent(ctx, req.UpstreamID, limit)
	if err != nil {
		return nil, err
	}
	// 空时也要是非 nil 的切片：JSON 上要的是 []，不是 null——界面把两者当同
	// 一件事，但响应体不该让调用方多一种情况去处理。
	list := make([]*admin.RecentRequest, 0, len(rows))
	for _, row := range rows {
		list = append(list, &admin.RecentRequest{
			At:         row.At,
			Object:     row.Object,
			Result:     row.Result,
			Bytes:      row.Bytes,
			DurationMS: row.DurationMS,
		})
	}
	return &admin.RecentRequestsResponse{List: list}, nil
}
