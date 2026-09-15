package git_svc

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/smartystreets/goconvey/convey"

	"github.com/CodFrm/katch/internal/model/entity/event_entity"
	"github.com/CodFrm/katch/internal/model/entity/git_entity"
	"github.com/CodFrm/katch/internal/service/setting_svc"
)

// manage 断言这个镜像层具备管理能力（List/Delete/Sweep），同 answer_test.go 的
// local：production 的 New 出来的那个必然具备，只是接口分开之后测试得显式过一道。
func manage(svc MirrorSvc) Manageable {
	m, ok := svc.(Manageable)
	if !ok {
		panic("mirrorSvc 应当实现 Manageable")
	}
	return m
}

// seedReadyMirror 摆出一份「已经建好」的镜像：库里一条 ready 记录，盘上一份带内容
// 的目录。配额淘汰与手动删除都要能在这上面动手，所以两样都得有。
func seedReadyMirror(t *testing.T, repo *fakeMirrorRepo, dir, host, path string,
	size, lastAccess int64,
) *git_entity.GitMirror {
	t.Helper()
	target, err := mirrorDir(dir, host, path)
	if err != nil {
		t.Fatalf("算镜像目录失败：%v", err)
	}
	if err := os.MkdirAll(target, 0o750); err != nil {
		t.Fatalf("建镜像目录失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "HEAD"), []byte("ref: refs/heads/main\n"), 0o640); err != nil {
		t.Fatalf("写镜像内容失败：%v", err)
	}
	row := &git_entity.GitMirror{
		Host: host, Repo: path, State: git_entity.MirrorReady,
		SizeBytes: size, LastAccessAt: lastAccess, Createtime: lastAccess, Updatetime: lastAccess,
	}
	if err := repo.save(row); err != nil {
		t.Fatalf("登记镜像记录失败：%v", err)
	}
	return repo.find(host, path)
}

func mirrorExistsOnDisk(t *testing.T, dir, host, path string) bool {
	t.Helper()
	target, err := mirrorDir(dir, host, path)
	if err != nil {
		t.Fatalf("算镜像目录失败：%v", err)
	}
	_, err = os.Stat(target)
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	t.Fatalf("查镜像目录失败：%v", err)
	return false
}

// TestSweep_EvictsOldestUntilUnderQuota 超配额时按最后访问时间最旧的顺序整仓删除，
// 删到配额之下为止（镜像生命周期一节）。
func TestSweep_EvictsOldestUntilUnderQuota(t *testing.T) {
	convey.Convey("三个仓库的镜像总量超过配额", t, func() {
		svc, repo, _, _, dir := newTestMirror(t, newFakeBuilder(), func(rt *setting_svc.RuntimeSettings) {
			rt.GitMirrorQuotaBytes = 2500
		})
		ctx := context.Background()

		seedReadyMirror(t, repo, dir, testHost, "/oldest.git", 1000, 100)
		seedReadyMirror(t, repo, dir, testHost, "/middle.git", 1000, 200)
		seedReadyMirror(t, repo, dir, testHost, "/newest.git", 1000, 300)

		removed, err := manage(svc).Sweep(ctx)

		convey.Convey("删到总量回到配额之下，从最旧的开始删", func() {
			convey.So(err, convey.ShouldBeNil)
			convey.So(removed, convey.ShouldEqual, 1)
			convey.So(repo.find(testHost, "/oldest.git"), convey.ShouldBeNil)
			convey.So(repo.find(testHost, "/middle.git"), convey.ShouldNotBeNil)
			convey.So(repo.find(testHost, "/newest.git"), convey.ShouldNotBeNil)
		})

		convey.Convey("被删仓库的盘上目录跟着没了，没被删的还在", func() {
			convey.So(mirrorExistsOnDisk(t, dir, testHost, "/oldest.git"), convey.ShouldBeFalse)
			convey.So(mirrorExistsOnDisk(t, dir, testHost, "/middle.git"), convey.ShouldBeTrue)
		})
	})
}

// TestSweep_SkipsMirrorsInUse 正在被本地应答读着的镜像不参与淘汰，即便它是最旧的
// 那个；配额淘汰改从下一个最旧但空闲的仓库下手。
func TestSweep_SkipsMirrorsInUse(t *testing.T) {
	convey.Convey("最旧的那个镜像正在被读", t, func() {
		svc, repo, _, _, dir := newTestMirror(t, newFakeBuilder(), func(rt *setting_svc.RuntimeSettings) {
			rt.GitMirrorQuotaBytes = 1500
		})
		ctx := context.Background()

		seedReadyMirror(t, repo, dir, testHost, "/busy.git", 1000, 100)
		seedReadyMirror(t, repo, dir, testHost, "/free.git", 1000, 200)

		release := svc.(*mirrorSvc).readers.acquire(mirrorKey(testHost, "/busy.git"))
		defer release()

		removed, err := manage(svc).Sweep(ctx)

		convey.Convey("跳过正被读的那个，删掉空闲的那个", func() {
			convey.So(err, convey.ShouldBeNil)
			convey.So(removed, convey.ShouldEqual, 1)
			convey.So(repo.find(testHost, "/busy.git"), convey.ShouldNotBeNil)
			convey.So(repo.find(testHost, "/free.git"), convey.ShouldBeNil)
			convey.So(mirrorExistsOnDisk(t, dir, testHost, "/busy.git"), convey.ShouldBeTrue)
		})
	})
}

// TestSweep_AllCandidatesInUseMakesNoProgress 剩下的候选全在被读时，这一轮不删
// 任何东西，也不会因为反复拿到同一批候选而卡死。
func TestSweep_AllCandidatesInUseMakesNoProgress(t *testing.T) {
	convey.Convey("唯一的候选正在被读", t, func() {
		svc, repo, _, _, dir := newTestMirror(t, newFakeBuilder(), func(rt *setting_svc.RuntimeSettings) {
			rt.GitMirrorQuotaBytes = 100
		})
		ctx := context.Background()
		seedReadyMirror(t, repo, dir, testHost, "/busy.git", 1000, 100)
		release := svc.(*mirrorSvc).readers.acquire(mirrorKey(testHost, "/busy.git"))
		defer release()

		removed, err := manage(svc).Sweep(ctx)

		convey.So(err, convey.ShouldBeNil)
		convey.So(removed, convey.ShouldEqual, 0)
		convey.So(repo.find(testHost, "/busy.git"), convey.ShouldNotBeNil)
	})
}

// TestDelete_RemovesRecordAndDisk 手动删除一条镜像：库记录与盘上的目录都要清掉，
// 下一次拉取因此重新走穿透（镜像生命周期一节）。
func TestDelete_RemovesRecordAndDisk(t *testing.T) {
	convey.Convey("删除一条已经建好的镜像", t, func() {
		svc, repo, _, _, dir := newTestMirror(t, newFakeBuilder(), nil)
		ctx := context.Background()
		row := seedReadyMirror(t, repo, dir, testHost, testRepo, 1000, 100)

		err := manage(svc).Delete(ctx, row.ID)

		convey.Convey("库记录与盘上目录都没了", func() {
			convey.So(err, convey.ShouldBeNil)
			convey.So(repo.find(testHost, testRepo), convey.ShouldBeNil)
			convey.So(mirrorExistsOnDisk(t, dir, testHost, testRepo), convey.ShouldBeFalse)
		})

		convey.Convey("下一次拉取答不出来，落回穿透", func() {
			convey.So(local(svc).Answer(ctx, advertise()), convey.ShouldBeNil)
		})
	})
}

// TestDelete_RefusesWhenInUse 正在被读的镜像删不掉：半路抽掉目录会让正在进行的
// 那次 clone 散架。
func TestDelete_RefusesWhenInUse(t *testing.T) {
	convey.Convey("镜像正在被读时删除它", t, func() {
		svc, repo, _, _, dir := newTestMirror(t, newFakeBuilder(), nil)
		ctx := context.Background()
		row := seedReadyMirror(t, repo, dir, testHost, testRepo, 1000, 100)
		release := svc.(*mirrorSvc).readers.acquire(mirrorKey(testHost, testRepo))
		defer release()

		err := manage(svc).Delete(ctx, row.ID)

		convey.So(err, convey.ShouldEqual, ErrMirrorInUse)
		convey.So(repo.find(testHost, testRepo), convey.ShouldNotBeNil)
		convey.So(mirrorExistsOnDisk(t, dir, testHost, testRepo), convey.ShouldBeTrue)
	})
}

// TestDelete_NotFound 删一条已经不在的记录不是一次穿库的 error，界面重复点两下
// 删除不该在日志里留一条 error。
func TestDelete_NotFound(t *testing.T) {
	convey.Convey("删一条不存在的镜像", t, func() {
		svc, _, _, _, _ := newTestMirror(t, newFakeBuilder(), nil)
		err := manage(svc).Delete(context.Background(), 999)
		convey.So(err, convey.ShouldEqual, ErrMirrorNotFound)
	})
}

// TestList_ReturnsAllMirrors 管理界面的镜像列表页：全量给，不分页（同上游列表，
// 规模有配额顶着）。
func TestList_ReturnsAllMirrors(t *testing.T) {
	convey.Convey("列出全部镜像记录", t, func() {
		svc, repo, _, _, dir := newTestMirror(t, newFakeBuilder(), nil)
		ctx := context.Background()
		seedReadyMirror(t, repo, dir, testHost, "/a.git", 1000, 100)
		seedReadyMirror(t, repo, dir, testHost, "/b.git", 2000, 200)

		list, err := manage(svc).List(ctx)

		convey.So(err, convey.ShouldBeNil)
		convey.So(len(list), convey.ShouldEqual, 2)
	})
}

// TestSweep_RecordsEvictionEvent 被淘汰也是一次镜像状态变化，它和建成、失败、
// 超限拒绝一样要进事件流（可观测性一节）——站长看到盘上少了一份镜像时，时间线
// 上得有一条说明它是被配额收走的，而不是谁手动删了。
func TestSweep_RecordsEvictionEvent(t *testing.T) {
	convey.Convey("配额淘汰删掉一份镜像", t, func() {
		svc, repo, events, _, dir := newTestMirror(t, newFakeBuilder(), func(rt *setting_svc.RuntimeSettings) {
			rt.GitMirrorQuotaBytes = 1500
		})
		ctx := context.Background()

		seedReadyMirror(t, repo, dir, testHost, "/oldest.git", 1000, 100)
		seedReadyMirror(t, repo, dir, testHost, "/newest.git", 1000, 200)

		removed, err := manage(svc).Sweep(ctx)
		convey.So(err, convey.ShouldBeNil)
		convey.So(removed, convey.ShouldEqual, 1)

		convey.Convey("时间线上有一条淘汰事件，操作人是 system", func() {
			convey.So(events.kinds(), convey.ShouldContain, event_entity.KindGitMirrorEvicted)
			convey.So(events.actorOf(event_entity.KindGitMirrorEvicted),
				convey.ShouldEqual, event_entity.ActorSystem)
		})

		convey.Convey("细节里带着是哪个仓库、腾出多少字节", func() {
			detail, ok := events.detailOf(event_entity.KindGitMirrorEvicted).(*mirrorDetail)
			convey.So(ok, convey.ShouldBeTrue)
			convey.So(detail.Host, convey.ShouldEqual, testHost)
			convey.So(detail.Repo, convey.ShouldEqual, "/oldest.git")
			convey.So(detail.SizeBytes, convey.ShouldEqual, 1000)
		})

		convey.Convey("没被删的那个不在时间线上", func() {
			for _, in := range events.list {
				if in.Kind != event_entity.KindGitMirrorEvicted {
					continue
				}
				detail, ok := in.Detail.(*mirrorDetail)
				convey.So(ok, convey.ShouldBeTrue)
				convey.So(detail.Repo, convey.ShouldNotEqual, "/newest.git")
			}
		})
	})
}

// TestSweep_SkipsMirrorsBeingSynced 正在做增量同步的镜像不参与淘汰。
//
// 读占用挡不住这一种：同步发生在 readers.acquire **之前**（answer.go 的 Answer
// 先 freshen 再拿读占用），所以一份正在被 Sync 写的仓库在 InUse 眼里是空闲的。
// 淘汰它就是在 go-git 往里写的同时把目录整个删掉，而同步一旦抢在删除之前收尾，
// 紧接着那一句 Save 会把刚被删掉的那行按原主键重新插回来（gorm 的 Save 在更新
// 影响 0 行时回落到 Create）——库里于是留下一条 ready、占着配额、盘上却什么都
// 没有的记录，它此后每次请求都被 touch，再也轮不到被淘汰，也永远不会重建。
func TestSweep_SkipsMirrorsBeingSynced(t *testing.T) {
	convey.Convey("唯一的候选正在做增量同步", t, func() {
		builder := newFakeBuilder()
		builder.syncGate = make(chan struct{})
		svc, repo, _, _, dir := newTestMirror(t, builder, func(rt *setting_svc.RuntimeSettings) {
			rt.GitMirrorQuotaBytes = 100
			rt.MutableTTLSeconds = 60
		})
		withAnswerer(svc, newFakeAnswerer(builder, "refs"))
		ctx := context.Background()
		// seedReadyMirror 放下的记录 LastSyncAt 是 0，因此这一次应答必然先同步。
		seedReadyMirror(t, repo, dir, testHost, "/syncing.git", 1000, 100)

		done := make(chan struct{})
		go func() {
			defer close(done)
			answer := local(svc).Answer(ctx, &AnswerRequest{
				Host: testHost, Repo: "/syncing.git", Advertise: true,
			})
			if answer != nil {
				_ = answer.Body.Close()
			}
		}()
		<-builder.syncStarted

		removed, err := manage(svc).Sweep(ctx)

		convey.Convey("这一轮什么都不删，记录与盘上目录都还在", func() {
			convey.So(err, convey.ShouldBeNil)
			convey.So(removed, convey.ShouldEqual, 0)
			convey.So(repo.find(testHost, "/syncing.git"), convey.ShouldNotBeNil)
			convey.So(mirrorExistsOnDisk(t, dir, testHost, "/syncing.git"), convey.ShouldBeTrue)
		})

		close(builder.syncGate)
		<-done
	})
}

// TestDelete_RefusesWhileBuilding 后台正在建的镜像删不掉。
//
// 同 ErrMirrorInUse 的理由，只是被打断的那一方换成了建镜像：删掉库记录之后，
// 后台那趟活收尾时的 Save 会按原主键把它重新插回来，而盘上的目录已经被删了。
func TestDelete_RefusesWhileBuilding(t *testing.T) {
	convey.Convey("一个仓库正在后台建镜像", t, func() {
		builder := newFakeBuilder()
		builder.gate = make(chan struct{})
		svc, repo, _, _, _ := newTestMirror(t, builder, nil)
		ctx := context.Background()

		svc.Ensure(ctx, testHost, testRepo)
		<-builder.started
		row := repo.find(testHost, testRepo)
		convey.So(row, convey.ShouldNotBeNil)

		err := manage(svc).Delete(ctx, row.ID)

		convey.Convey("拒绝删除，记录还在", func() {
			convey.So(err, convey.ShouldEqual, ErrMirrorInUse)
			convey.So(repo.find(testHost, testRepo), convey.ShouldNotBeNil)
		})

		close(builder.gate)
	})
}
