package request_svc

import (
	"context"
	"errors"
	"testing"

	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"

	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/model/entity/request_log_entity"
)

// 这一层要验的是「面板怎么按 upstream_id 从库里取最近若干条」：排序交给仓储的
// SQL（sqlmock 用例守着），这里守的是条数的夹逼与字段的搬运，不连库。

// testUpstreamI 读取用例里那个上游的 id。
const testUpstreamI int64 = 7

func recentRow(id, at int64, object, result string, bytes, durationMS int64) *request_log_entity.RecentRequest {
	return &request_log_entity.RecentRequest{
		ID:         id,
		UpstreamID: testUpstreamI,
		At:         at,
		Object:     object,
		Result:     result,
		Bytes:      bytes,
		DurationMS: durationMS,
	}
}

func TestRequest_RecentRequests(t *testing.T) {
	convey.Convey("按 upstream_id 读某个上游的最近请求", t, func() {
		deps := setup(t)
		svc := deps.svc(Options{})
		ctx := context.Background()

		convey.Convey("字段逐个搬到响应上，顺序是仓储给的", func() {
			deps.repo.EXPECT().ListRecent(gomock.Any(), testUpstreamI, admin.RecentRequestsDefaultLimit).
				Return([]*request_log_entity.RecentRequest{
					recentRow(2, testNow, "/dists/InRelease", "hit", 512, 4),
					recentRow(1, testNow-1, "/pool/a.deb", "miss", 4096, 120),
				}, nil)

			resp, err := svc.RecentRequests(ctx, &admin.RecentRequestsRequest{UpstreamID: testUpstreamI})
			convey.So(err, convey.ShouldBeNil)
			convey.So(resp.List, convey.ShouldHaveLength, 2)
			convey.So(resp.List[0].At, convey.ShouldEqual, testNow)
			convey.So(resp.List[0].Object, convey.ShouldEqual, "/dists/InRelease")
			convey.So(resp.List[0].Result, convey.ShouldEqual, "hit")
			convey.So(resp.List[0].Bytes, convey.ShouldEqual, 512)
			convey.So(resp.List[0].DurationMS, convey.ShouldEqual, 4)
			convey.So(resp.List[1].Object, convey.ShouldEqual, "/pool/a.deb")
			convey.So(resp.List[1].Result, convey.ShouldEqual, "miss")
		})

		convey.Convey("limit 非正时按默认条数取", func() {
			deps.repo.EXPECT().ListRecent(gomock.Any(), testUpstreamI, admin.RecentRequestsDefaultLimit).
				Return(nil, nil)

			_, err := svc.RecentRequests(ctx, &admin.RecentRequestsRequest{UpstreamID: testUpstreamI})
			convey.So(err, convey.ShouldBeNil)
		})

		convey.Convey("limit 超过硬上限时夹到 100", func() {
			// 接口层的 binding 已经挡过一次，服务层被别的调用方直接用时也要夹住：
			// 响应大小不能由调用方决定。
			deps.repo.EXPECT().ListRecent(gomock.Any(), testUpstreamI, admin.RecentRequestsMaxLimit).
				Return(nil, nil)

			_, err := svc.RecentRequests(ctx, &admin.RecentRequestsRequest{
				UpstreamID: testUpstreamI,
				Limit:      100000,
			})
			convey.So(err, convey.ShouldBeNil)
		})

		convey.Convey("库里没有行时给空列表而不是 nil", func() {
			deps.repo.EXPECT().ListRecent(gomock.Any(), testUpstreamI, admin.RecentRequestsDefaultLimit).
				Return([]*request_log_entity.RecentRequest{}, nil)

			resp, err := svc.RecentRequests(ctx, &admin.RecentRequestsRequest{UpstreamID: testUpstreamI})
			convey.So(err, convey.ShouldBeNil)
			convey.So(resp.List, convey.ShouldNotBeNil)
			convey.So(resp.List, convey.ShouldBeEmpty)
		})

		convey.Convey("库读不出来时把错误交给调用方", func() {
			deps.repo.EXPECT().ListRecent(gomock.Any(), testUpstreamI, admin.RecentRequestsDefaultLimit).
				Return(nil, errors.New("库不可用"))

			_, err := svc.RecentRequests(ctx, &admin.RecentRequestsRequest{UpstreamID: testUpstreamI})
			convey.So(err, convey.ShouldNotBeNil)
		})
	})
}
