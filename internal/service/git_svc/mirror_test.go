package git_svc

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/smartystreets/goconvey/convey"

	"github.com/CodFrm/katch/internal/model/entity/event_entity"
	"github.com/CodFrm/katch/internal/model/entity/git_entity"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/service/setting_svc"
)

const (
	testHost = "git.fake-upstream.test"
	testRepo = "/foo/bar.git"
)

// newTestMirror 起一个装好了假上游、假镜像表、假 builder 的镜像层。
func newTestMirror(t *testing.T, builder Builder, patch func(rt *setting_svc.RuntimeSettings)) (
	MirrorSvc, *fakeMirrorRepo, *fakeEvents, *fakeRuntime, string,
) {
	t.Helper()
	registerUpstream(t, testHost, true, upstream_entity.ProtocolGit)
	repo := newFakeMirrorRepo(t)
	events := captureEvents(t)
	rt := newFakeRuntime(t, patch)
	dir := t.TempDir()
	svc := New(Options{Dir: dir, Runtime: rt, Builder: builder})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = svc.Quiesce(ctx)
	})
	return svc, repo, events, rt, dir
}

// TestEnsure_PendingThenReady 一次穿透登记 pending，后台把它建成 ready。
func TestEnsure_PendingThenReady(t *testing.T) {
	convey.Convey("一次穿透拉取之后", t, func() {
		builder := newFakeBuilder()
		builder.gate = make(chan struct{})
		svc, repo, events, _, dir := newTestMirror(t, builder, nil)
		ctx := context.Background()

		svc.Ensure(ctx, testHost, testRepo)

		convey.Convey("记录当场就是 pending，请求这一侧不必等建镜像", func() {
			row := repo.find(testHost, testRepo)
			convey.So(row, convey.ShouldNotBeNil)
			convey.So(row.State, convey.ShouldEqual, git_entity.MirrorPending)
			convey.So(row.LastAccessAt, convey.ShouldBeGreaterThan, 0)

			convey.Convey("建完之后转 ready，体积与同步时刻都记下来", func() {
				close(builder.gate)
				convey.So(svc.Quiesce(ctx), convey.ShouldBeNil)

				done := repo.find(testHost, testRepo)
				convey.So(done.State, convey.ShouldEqual, git_entity.MirrorReady)
				convey.So(done.SizeBytes, convey.ShouldEqual, 1024)
				convey.So(done.LastSyncAt, convey.ShouldBeGreaterThan, 0)
				convey.So(done.LastError, convey.ShouldEqual, "")

				convey.Convey("镜像建在镜像目录下，远端是上游的回源地址加仓库路径", func() {
					req := builder.lastRequest()
					convey.So(req.Remote, convey.ShouldEqual, "https://"+testHost+testRepo)
					convey.So(req.Dir, convey.ShouldStartWith, dir)
					convey.So(req.Dir, convey.ShouldEndWith, ".git")
				})

				convey.Convey("两次状态变化都进了事件流", func() {
					convey.So(events.kinds(), convey.ShouldResemble, []string{
						event_entity.KindGitMirrorPending, event_entity.KindGitMirrorReady,
					})
					convey.So(events.upstreamIDOf(event_entity.KindGitMirrorReady),
						convey.ShouldEqual, 7)
					convey.So(events.actorOf(event_entity.KindGitMirrorReady),
						convey.ShouldEqual, event_entity.ActorSystem)
					convey.So(events.detailOf(event_entity.KindGitMirrorReady),
						convey.ShouldNotBeNil)
				})

				convey.Convey("建成之后再来一次不会重建", func() {
					svc.Ensure(ctx, testHost, testRepo)
					convey.So(svc.Quiesce(ctx), convey.ShouldBeNil)
					convey.So(builder.callCount(), convey.ShouldEqual, 1)
				})
			})
		})
	})
}

// TestEnsure_TooLargeBecomesRejected 超过单仓上限的仓库转 rejected，此后永久穿透。
func TestEnsure_TooLargeBecomesRejected(t *testing.T) {
	convey.Convey("仓库超过单仓体积上限时", t, func() {
		builder := newFakeBuilder()
		builder.result = func(*BuildRequest) (int64, error) {
			return 0, ErrRepoTooLarge
		}
		svc, repo, events, _, _ := newTestMirror(t, builder,
			func(rt *setting_svc.RuntimeSettings) { rt.GitRepoMaxBytes = 4096 })
		ctx := context.Background()

		svc.Ensure(ctx, testHost, testRepo)
		convey.So(svc.Quiesce(ctx), convey.ShouldBeNil)

		convey.Convey("登记为 rejected 而不是 failed", func() {
			row := repo.find(testHost, testRepo)
			convey.So(row.State, convey.ShouldEqual, git_entity.MirrorRejected)
			convey.So(row.LastError, convey.ShouldNotBeBlank)
			convey.So(row.SizeBytes, convey.ShouldEqual, 0)
		})

		convey.Convey("上限是每次现读的，不是构造时抄下来的", func() {
			convey.So(builder.lastRequest().MaxBytes, convey.ShouldEqual, 4096)
		})

		convey.Convey("此后不再反复尝试建镜像", func() {
			svc.Ensure(ctx, testHost, testRepo)
			svc.Ensure(ctx, testHost, testRepo)
			convey.So(svc.Quiesce(ctx), convey.ShouldBeNil)
			convey.So(builder.callCount(), convey.ShouldEqual, 1)
		})

		convey.Convey("状态变化进了事件流", func() {
			convey.So(events.kinds(), convey.ShouldResemble, []string{
				event_entity.KindGitMirrorPending, event_entity.KindGitMirrorRejected,
			})
		})
	})
}

// TestEnsure_FailureRecordsReason 建镜像失败转 failed 并记下原因，也不再重试。
func TestEnsure_FailureRecordsReason(t *testing.T) {
	convey.Convey("建镜像失败时", t, func() {
		builder := newFakeBuilder()
		builder.result = func(*BuildRequest) (int64, error) {
			return 0, errors.New("上游拨不通")
		}
		svc, repo, events, _, _ := newTestMirror(t, builder, nil)
		ctx := context.Background()

		svc.Ensure(ctx, testHost, testRepo)
		convey.So(svc.Quiesce(ctx), convey.ShouldBeNil)

		convey.Convey("转 failed 并把原因记下来", func() {
			row := repo.find(testHost, testRepo)
			convey.So(row.State, convey.ShouldEqual, git_entity.MirrorFailed)
			convey.So(row.LastError, convey.ShouldContainSubstring, "上游拨不通")
			convey.So(row.LastSyncAt, convey.ShouldEqual, 0)
		})

		convey.Convey("此后不再反复尝试", func() {
			svc.Ensure(ctx, testHost, testRepo)
			convey.So(svc.Quiesce(ctx), convey.ShouldBeNil)
			convey.So(builder.callCount(), convey.ShouldEqual, 1)
		})

		convey.Convey("状态变化进了事件流", func() {
			convey.So(events.kinds(), convey.ShouldResemble, []string{
				event_entity.KindGitMirrorPending, event_entity.KindGitMirrorFailed,
			})
		})
	})
}

// TestEnsure_ConcurrentBuildsCollapse 同一个仓库的并发建镜像合并成一次。
func TestEnsure_ConcurrentBuildsCollapse(t *testing.T) {
	convey.Convey("同一个仓库被几个客户端同时拉到时", t, func() {
		builder := newFakeBuilder()
		builder.gate = make(chan struct{})
		svc, repo, _, _, _ := newTestMirror(t, builder, nil)
		ctx := context.Background()

		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				svc.Ensure(ctx, testHost, testRepo)
			}()
		}
		wg.Wait()
		// 第一趟活已经进门，后面那七个请求都已经返回：此刻还没建完，
		// 合并没做到的话，另外几趟已经各自进门了。
		<-builder.started
		close(builder.gate)
		convey.So(svc.Quiesce(ctx), convey.ShouldBeNil)

		convey.Convey("只建了一次镜像，库里也只有一条记录", func() {
			convey.So(builder.callCount(), convey.ShouldEqual, 1)
			convey.So(repo.count(), convey.ShouldEqual, 1)
			convey.So(repo.find(testHost, testRepo).State, convey.ShouldEqual, git_entity.MirrorReady)
		})
	})
}

// TestEnsure_HonoursSyncConcurrency 同步并发上限是每次现读的运行时项。
func TestEnsure_HonoursSyncConcurrency(t *testing.T) {
	convey.Convey("同步并发上限压着同时在跑的建镜像数", t, func() {
		builder := newFakeBuilder()
		builder.gate = make(chan struct{})
		svc, _, _, _, _ := newTestMirror(t, builder, func(rt *setting_svc.RuntimeSettings) {
			rt.GitSyncConcurrency = 1
		})
		ctx := context.Background()

		svc.Ensure(ctx, testHost, "/a.git")
		svc.Ensure(ctx, testHost, "/b.git")
		<-builder.started

		convey.Convey("第二个仓库要等第一个让出名额", func() {
			// 上限是 1，所以此刻只该有一趟活在跑；第二趟必须还在排队。
			convey.So(builder.callCount(), convey.ShouldEqual, 1)
			close(builder.gate)
			convey.So(svc.Quiesce(ctx), convey.ShouldBeNil)
			convey.So(builder.callCount(), convey.ShouldEqual, 2)
		})
	})
}

// TestEnsure_IgnoresUpstreamsWithoutGit 没开 git 协议、已停用、不在表里的主机都不镜像。
func TestEnsure_IgnoresUpstreamsWithoutGit(t *testing.T) {
	convey.Convey("不该被镜像的主机", t, func() {
		convey.Convey("上游已停用时不登记、也不建", func() {
			builder := newFakeBuilder()
			registerUpstream(t, testHost, false, upstream_entity.ProtocolGit)
			repo := newFakeMirrorRepo(t)
			captureEvents(t)
			svc := New(Options{Dir: t.TempDir(), Runtime: newFakeRuntime(t, nil), Builder: builder})
			ctx := context.Background()

			svc.Ensure(ctx, testHost, testRepo)
			convey.So(svc.Quiesce(ctx), convey.ShouldBeNil)
			convey.So(repo.count(), convey.ShouldEqual, 0)
			convey.So(builder.callCount(), convey.ShouldEqual, 0)
		})

		convey.Convey("上游没开 git 协议时不登记、也不建", func() {
			builder := newFakeBuilder()
			registerUpstream(t, testHost, true, upstream_entity.ProtocolStatic)
			repo := newFakeMirrorRepo(t)
			captureEvents(t)
			svc := New(Options{Dir: t.TempDir(), Runtime: newFakeRuntime(t, nil), Builder: builder})
			ctx := context.Background()

			svc.Ensure(ctx, testHost, testRepo)
			convey.So(svc.Quiesce(ctx), convey.ShouldBeNil)
			convey.So(repo.count(), convey.ShouldEqual, 0)
			convey.So(builder.callCount(), convey.ShouldEqual, 0)
		})

		convey.Convey("没配镜像目录时整个功能是关的，拉取照常穿透", func() {
			builder := newFakeBuilder()
			registerUpstream(t, testHost, true, upstream_entity.ProtocolGit)
			repo := newFakeMirrorRepo(t)
			svc := New(Options{Runtime: newFakeRuntime(t, nil), Builder: builder})
			ctx := context.Background()

			svc.Ensure(ctx, testHost, testRepo)
			convey.So(svc.Quiesce(ctx), convey.ShouldBeNil)
			convey.So(repo.count(), convey.ShouldEqual, 0)
			convey.So(builder.callCount(), convey.ShouldEqual, 0)
		})
	})
}

// TestEnsure_ReadsSettingsFreshEachTime 设置改完，下一趟活就按新值走，不必重启。
//
// 这一条钉的是「运行时项每次现读」：把上限在构造时抄进字段里，第一趟活照样是对的，
// 只有第二趟才露馅——而那时站长看到的是「设置页改了，但它不生效」。
func TestEnsure_ReadsSettingsFreshEachTime(t *testing.T) {
	convey.Convey("单仓上限改完之后", t, func() {
		builder := newFakeBuilder()
		svc, _, _, rt, _ := newTestMirror(t, builder,
			func(r *setting_svc.RuntimeSettings) { r.GitRepoMaxBytes = 4096 })
		ctx := context.Background()

		svc.Ensure(ctx, testHost, "/a.git")
		convey.So(svc.Quiesce(ctx), convey.ShouldBeNil)
		convey.So(builder.lastRequest().MaxBytes, convey.ShouldEqual, 4096)

		rt.set(func(r *setting_svc.RuntimeSettings) { r.GitRepoMaxBytes = 9999 })
		svc.Ensure(ctx, testHost, "/b.git")
		convey.So(svc.Quiesce(ctx), convey.ShouldBeNil)

		convey.Convey("下一个仓库按新的上限建", func() {
			convey.So(builder.lastRequest().MaxBytes, convey.ShouldEqual, 9999)
		})
	})
}
