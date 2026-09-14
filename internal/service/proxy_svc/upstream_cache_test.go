package proxy_svc

import (
	"context"
	"sync"
	"testing"

	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	mock_upstream_repo "github.com/CodFrm/katch/internal/repository/upstream_repo/mock"
)

func table() []*upstream_entity.Upstream {
	return []*upstream_entity.Upstream{
		{ID: 1, Host: "deb.debian.org", Kind: upstream_entity.KindStatic, Enabled: true,
			ImmutablePatterns: upstream_entity.PatternList{"pool/"}},
		{ID: 2, Host: "docker.io", Kind: upstream_entity.KindRegistry, Enabled: true},
	}
}

// TestCachedUpstreamRepo_ReadPathDoesNotQueryPerRequest
//
// 拉取是热路径：一次 docker pull 是几十上百个请求，每个都查一次库等于把镜像站的
// 吞吐绑在 sqlite 上。整张表一次性装进进程内，未命中的主机也在同一份快照里回答，
// 否则随便一个探测循环就能把库打满。
func TestCachedUpstreamRepo_ReadPathDoesNotQueryPerRequest(t *testing.T) {
	convey.Convey("读路径只装载一次", t, func() {
		inner := mock_upstream_repo.NewMockUpstreamRepo(gomock.NewController(t))
		inner.EXPECT().List(gomock.Any()).Return(table(), nil).Times(1)
		repo := NewCachedUpstreamRepo(inner)
		ctx := context.Background()

		for range 3 {
			got, err := repo.FindByHost(ctx, "deb.debian.org")
			convey.So(err, convey.ShouldBeNil)
			convey.So(got.ID, convey.ShouldEqual, 1)
		}
		// 不在表里的主机也由同一份快照回答，不落到库上。
		got, err := repo.FindByHost(ctx, "internal.corp.local")
		convey.So(err, convey.ShouldBeNil)
		convey.So(got, convey.ShouldBeNil)
	})
}

// TestCachedUpstreamRepo_InvalidatedByAdminWrite
//
// 「改完无需重启」是这一轮的目标。缓存不在管理写入后失效，界面上停用一个上游
// 就要等到进程重启才生效——而停用通常正是因为出了事，等不了。
func TestCachedUpstreamRepo_InvalidatedByAdminWrite(t *testing.T) {
	convey.Convey("管理接口写入后缓存失效", t, func() {
		ctx := context.Background()

		convey.Convey("Save 之后重新装载", func() {
			inner := mock_upstream_repo.NewMockUpstreamRepo(gomock.NewController(t))
			first := table()
			inner.EXPECT().List(gomock.Any()).Return(first, nil).Times(1)
			inner.EXPECT().Save(gomock.Any(), gomock.Any()).Return(nil).Times(1)
			disabled := table()
			disabled[0].Enabled = false
			inner.EXPECT().List(gomock.Any()).Return(disabled, nil).Times(1)
			repo := NewCachedUpstreamRepo(inner)

			got, err := repo.FindByHost(ctx, "deb.debian.org")
			convey.So(err, convey.ShouldBeNil)
			convey.So(got.Enabled, convey.ShouldBeTrue)

			convey.So(repo.Save(ctx, &upstream_entity.Upstream{ID: 1}), convey.ShouldBeNil)

			got, err = repo.FindByHost(ctx, "deb.debian.org")
			convey.So(err, convey.ShouldBeNil)
			convey.So(got.Enabled, convey.ShouldBeFalse)
		})

		convey.Convey("Delete 之后重新装载", func() {
			inner := mock_upstream_repo.NewMockUpstreamRepo(gomock.NewController(t))
			inner.EXPECT().List(gomock.Any()).Return(table(), nil).Times(1)
			inner.EXPECT().Delete(gomock.Any(), int64(1)).Return(nil).Times(1)
			inner.EXPECT().List(gomock.Any()).Return([]*upstream_entity.Upstream{}, nil).Times(1)
			repo := NewCachedUpstreamRepo(inner)

			_, err := repo.FindByHost(ctx, "deb.debian.org")
			convey.So(err, convey.ShouldBeNil)
			convey.So(repo.Delete(ctx, 1), convey.ShouldBeNil)

			got, err := repo.FindByHost(ctx, "deb.debian.org")
			convey.So(err, convey.ShouldBeNil)
			convey.So(got, convey.ShouldBeNil)
		})

		convey.Convey("写入失败时不动缓存", func() {
			inner := mock_upstream_repo.NewMockUpstreamRepo(gomock.NewController(t))
			inner.EXPECT().List(gomock.Any()).Return(table(), nil).Times(1)
			inner.EXPECT().Save(gomock.Any(), gomock.Any()).Return(context.DeadlineExceeded).Times(1)
			repo := NewCachedUpstreamRepo(inner)

			_, err := repo.FindByHost(ctx, "deb.debian.org")
			convey.So(err, convey.ShouldBeNil)
			// 库里没改成，缓存就还是对的，丢掉它只会白查一次库。
			convey.So(repo.Save(ctx, &upstream_entity.Upstream{ID: 1}), convey.ShouldNotBeNil)
			got, err := repo.FindByHost(ctx, "deb.debian.org")
			convey.So(err, convey.ShouldBeNil)
			convey.So(got.ID, convey.ShouldEqual, 1)
		})
	})
}

// TestCachedUpstreamRepo_ReturnsCopy
//
// 缓存里那份是全进程共用的。直接把指针递出去，任何一个调用方顺手改一个字段，
// 别的请求就会看到一条谁也没在库里写过的上游记录。
func TestCachedUpstreamRepo_ReturnsCopy(t *testing.T) {
	convey.Convey("返回的是副本", t, func() {
		inner := mock_upstream_repo.NewMockUpstreamRepo(gomock.NewController(t))
		inner.EXPECT().List(gomock.Any()).Return(table(), nil).Times(1)
		repo := NewCachedUpstreamRepo(inner)
		ctx := context.Background()

		got, err := repo.FindByHost(ctx, "deb.debian.org")
		convey.So(err, convey.ShouldBeNil)
		got.Origin = "http://evil.example.com"
		got.ImmutablePatterns[0] = "/"

		again, err := repo.FindByHost(ctx, "deb.debian.org")
		convey.So(err, convey.ShouldBeNil)
		convey.So(again.Origin, convey.ShouldBeEmpty)
		convey.So(again.ImmutablePatterns[0], convey.ShouldEqual, "pool/")
	})
}

// TestCachedUpstreamRepo_ConcurrentColdStart 冷启动时一堆请求同时到，装载只做一次。
func TestCachedUpstreamRepo_ConcurrentColdStart(t *testing.T) {
	convey.Convey("并发冷启动只装载一次", t, func() {
		inner := mock_upstream_repo.NewMockUpstreamRepo(gomock.NewController(t))
		inner.EXPECT().List(gomock.Any()).Return(table(), nil).Times(1)
		repo := NewCachedUpstreamRepo(inner)

		wg := sync.WaitGroup{}
		for range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = repo.FindByHost(context.Background(), "docker.io")
			}()
		}
		wg.Wait()
	})
}

// TestCachedUpstreamRepo_AdminReadsGoToDB 管理接口读的是库，不是缓存快照——
// 它要看到刚写进去的那条，而不是一份可能还没失效的副本。
func TestCachedUpstreamRepo_AdminReadsGoToDB(t *testing.T) {
	convey.Convey("管理接口的读直穿到库", t, func() {
		inner := mock_upstream_repo.NewMockUpstreamRepo(gomock.NewController(t))
		inner.EXPECT().List(gomock.Any()).Return(table(), nil).Times(2)
		inner.EXPECT().Find(gomock.Any(), int64(1)).Return(table()[0], nil).Times(1)
		repo := NewCachedUpstreamRepo(inner)
		ctx := context.Background()

		for range 2 {
			list, err := repo.List(ctx)
			convey.So(err, convey.ShouldBeNil)
			convey.So(len(list), convey.ShouldEqual, 2)
		}
		got, err := repo.Find(ctx, 1)
		convey.So(err, convey.ShouldBeNil)
		convey.So(got.ID, convey.ShouldEqual, 1)
	})
}

// TestCachedUpstreamRepo_WriteDuringColdLoadIsNotLost 装载途中发生的写入不能被盖掉。
//
// 冷装载是「查库 → 发布快照」两步，中间没有持锁。一次写入如果正好落在这两步之间，
// 它的失效会先于发布执行，于是**写入之前**那份读数被发布在失效之后，然后再也不会
// 过期（这层缓存没有 TTL，只有下一次写入才会丢快照）。
//
// 具体到会坏成什么样：站长在后台停用 deb.debian.org，而此刻正好有一次冷装载在飞
// （冷装载在每次写入之后都会发生，连着改两条上游就能撞上），停用就此丢失——界面上
// 显示已停用，回源却照常进行，一直到下一次有人写上游表为止。
//
// 兄弟实现 setting_svc.cachedSettingRepo 用 generation 挡住了同一件事，两边形状本来
// 就是照着抄的；这里少的正是那一道判定。
func TestCachedUpstreamRepo_WriteDuringColdLoadIsNotLost(t *testing.T) {
	convey.Convey("冷装载途中发生的写入不会被过期读数盖掉", t, func() {
		inner := mock_upstream_repo.NewMockUpstreamRepo(gomock.NewController(t))
		release := make(chan struct{})
		var calls int
		var mu sync.Mutex
		inner.EXPECT().List(gomock.Any()).AnyTimes().DoAndReturn(
			func(context.Context) ([]*upstream_entity.Upstream, error) {
				mu.Lock()
				calls++
				first := calls == 1
				mu.Unlock()
				if first {
					// 第一次装载停在这里，让写入插到「查完库」和「发布快照」之间。
					<-release
					return table(), nil
				}
				// 写入之后库里那条已经停用了。
				return []*upstream_entity.Upstream{
					{ID: 2, Host: "docker.io", Kind: upstream_entity.KindRegistry, Enabled: true},
				}, nil
			})
		inner.EXPECT().Save(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

		repo := NewCachedUpstreamRepo(inner)
		ctx := context.Background()

		loaded := make(chan struct{})
		go func() {
			defer close(loaded)
			_, _ = repo.FindByHost(ctx, "docker.io") // 停在 List 里
		}()
		// 等第一次装载真的进到 List 之后再写，否则这条用例测不到那个缝。
		for {
			mu.Lock()
			in := calls >= 1
			mu.Unlock()
			if in {
				break
			}
		}
		// 停用 deb.debian.org：写库成功 → 丢快照。
		convey.So(repo.Save(ctx, &upstream_entity.Upstream{ID: 1, Host: "deb.debian.org"}),
			convey.ShouldBeNil)
		close(release)
		<-loaded

		// 写入已经发生过了，这次读必须看到写入之后的库，而不是写入之前那份读数。
		got, err := repo.FindByHost(ctx, "deb.debian.org")
		convey.So(err, convey.ShouldBeNil)
		convey.So(got, convey.ShouldBeNil)
	})
}
