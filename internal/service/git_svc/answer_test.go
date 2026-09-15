package git_svc

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smartystreets/goconvey/convey"

	"github.com/CodFrm/katch/internal/model/entity/git_entity"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/service/setting_svc"
)

// readyMirror 往镜像表里放一条建好的镜像，最后一次同步在 ago 秒之前。
func readyMirror(t *testing.T, repo *fakeMirrorRepo, ago int64) *git_entity.GitMirror {
	t.Helper()
	now := time.Now().Unix()
	row := &git_entity.GitMirror{
		Host: testHost, Repo: testRepo, State: git_entity.MirrorReady,
		LastSyncAt: now - ago, LastAccessAt: now - ago, SizeBytes: 1024,
		Createtime: now - ago, Updatetime: now - ago,
	}
	if err := repo.save(row); err != nil {
		t.Fatalf("放一条镜像记录失败：%v", err)
	}
	return row
}

// local 取镜像层上本地应答那一半的能力。
func local(svc MirrorSvc) LocalAnswerer {
	answerer, ok := svc.(LocalAnswerer)
	if !ok {
		panic("这个镜像层答不了本地请求")
	}
	return answerer
}

// advertise 发一次 ref 广播的本地应答请求。
func advertise() *AnswerRequest {
	return &AnswerRequest{Host: testHost, Repo: testRepo, Advertise: true}
}

// TestAnswer_ReadyMirrorAnswersLocally 目标 (b)：镜像就绪且在 TTL 内时由本地应答，
// 一个字节都不问上游。
func TestAnswer_ReadyMirrorAnswersLocally(t *testing.T) {
	convey.Convey("TTL 内的 ready 镜像由本地应答", t, func() {
		builder := newFakeBuilder()
		answerer := newFakeAnswerer(builder, "001e# service=git-upload-pack\n0000")
		svc, repo, _, _, dir := newTestMirror(t, builder, func(rt *setting_svc.RuntimeSettings) {
			rt.MutableTTLSeconds = 300
		})
		withAnswerer(svc, answerer)
		row := readyMirror(t, repo, 10)

		answer := local(svc).Answer(context.Background(), advertise())

		convey.So(answer, convey.ShouldNotBeNil)
		convey.So(answer.ContentType, convey.ShouldEqual,
			"application/x-git-upload-pack-advertisement")
		body, err := io.ReadAll(answer.Body)
		convey.So(err, convey.ShouldBeNil)
		convey.So(string(body), convey.ShouldEqual, "001e# service=git-upload-pack\n0000")
		convey.So(answer.Body.Close(), convey.ShouldBeNil)

		convey.Convey("TTL 内一次上游同步都没有发生", func() {
			convey.So(builder.syncCount(), convey.ShouldEqual, 0)
		})
		convey.Convey("答的是这个仓库在盘上那份镜像", func() {
			convey.So(answerer.lastRequest().Dir, convey.ShouldStartWith, dir)
			convey.So(answerer.lastRequest().Dir, convey.ShouldEndWith, ".git")
			convey.So(answerer.lastRequest().Advertise, convey.ShouldBeTrue)
		})
		convey.Convey("访问时间被推到现在：配额淘汰按它排序", func() {
			convey.So(repo.find(testHost, testRepo).LastAccessAt,
				convey.ShouldBeGreaterThan, row.LastAccessAt)
		})
	})
}

// TestAnswer_WithoutReadyMirrorFallsBack 没有一份就绪的镜像时一律穿透（决策 5）。
func TestAnswer_WithoutReadyMirrorFallsBack(t *testing.T) {
	convey.Convey("镜像不可用时给回 nil，让调用方穿透", t, func() {
		cases := map[string]string{
			"还没建完":  git_entity.MirrorPending,
			"建失败了":  git_entity.MirrorFailed,
			"超单仓上限": git_entity.MirrorRejected,
		}
		for name, state := range cases {
			convey.Convey(name, func() {
				builder := newFakeBuilder()
				answerer := newFakeAnswerer(builder, "x")
				svc, repo, _, _, _ := newTestMirror(t, builder, nil)
				withAnswerer(svc, answerer)
				row := readyMirror(t, repo, 10)
				row.State = state
				convey.So(repo.save(row), convey.ShouldBeNil)

				convey.So(local(svc).Answer(context.Background(), advertise()), convey.ShouldBeNil)
				convey.So(answerer.callCount(), convey.ShouldEqual, 0)
			})
		}
		convey.Convey("这个仓库从来没被镜像过", func() {
			builder := newFakeBuilder()
			answerer := newFakeAnswerer(builder, "x")
			svc, _, _, _, _ := newTestMirror(t, builder, nil)
			withAnswerer(svc, answerer)

			convey.So(local(svc).Answer(context.Background(), advertise()), convey.ShouldBeNil)
			convey.So(answerer.callCount(), convey.ShouldEqual, 0)
		})
	})
}

// TestAnswer_StaleMirrorSyncsFirst 目标 (c)：超出 TTL 时先做一次增量同步再广播（决策 8）。
//
// 顺序是判据本身：先广播后同步等于把 push 完立刻 clone 的人送去拿旧提交，
// 而他无从判断手上这份是不是旧的。
func TestAnswer_StaleMirrorSyncsFirst(t *testing.T) {
	convey.Convey("超出 TTL 的镜像先同步再应答", t, func() {
		builder := newFakeBuilder()
		answerer := newFakeAnswerer(builder, "refs")
		svc, repo, _, _, _ := newTestMirror(t, builder, func(rt *setting_svc.RuntimeSettings) {
			rt.MutableTTLSeconds = 60
		})
		withAnswerer(svc, answerer)
		before := readyMirror(t, repo, 3600)

		answer := local(svc).Answer(context.Background(), advertise())
		convey.So(answer, convey.ShouldNotBeNil)
		_ = answer.Body.Close()

		convey.So(builder.syncCount(), convey.ShouldEqual, 1)
		convey.So(builder.lastSync().Remote, convey.ShouldEqual, "https://"+testHost+testRepo)
		convey.Convey("同步在应答之前：广播出去的正是同步之后那份", func() {
			convey.So(answerer.syncsBefore(), convey.ShouldEqual, 1)
		})
		convey.Convey("同步时刻被推到现在，状态仍然是 ready", func() {
			row := repo.find(testHost, testRepo)
			convey.So(row.LastSyncAt, convey.ShouldBeGreaterThan, before.LastSyncAt)
			convey.So(row.State, convey.ShouldEqual, git_entity.MirrorReady)
		})
	})
}

// TestAnswer_FailedSyncDegradesThisRequestOnly 目标 (c)：同步失败或超时时这一次降级
// 为穿透，镜像状态不变——一次上游抖动不该把一个可用的镜像作废（决策 8）。
func TestAnswer_FailedSyncDegradesThisRequestOnly(t *testing.T) {
	convey.Convey("同步没成时这一次穿透，镜像还在", t, func() {
		convey.Convey("同步报错", func() {
			builder := newFakeBuilder()
			builder.syncResult = func(*SyncRequest) (int64, error) {
				return 0, io.ErrUnexpectedEOF
			}
			answerer := newFakeAnswerer(builder, "refs")
			svc, repo, events, _, _ := newTestMirror(t, builder, func(rt *setting_svc.RuntimeSettings) {
				rt.MutableTTLSeconds = 60
			})
			withAnswerer(svc, answerer)
			before := readyMirror(t, repo, 3600)

			convey.So(local(svc).Answer(context.Background(), advertise()), convey.ShouldBeNil)
			convey.So(answerer.callCount(), convey.ShouldEqual, 0)

			row := repo.find(testHost, testRepo)
			convey.So(row.State, convey.ShouldEqual, git_entity.MirrorReady)
			convey.So(row.LastSyncAt, convey.ShouldEqual, before.LastSyncAt)
			convey.So(row.LastError, convey.ShouldBeEmpty)
			convey.Convey("一次失败的同步不是状态变化，不该进事件流", func() {
				convey.So(events.kinds(), convey.ShouldBeEmpty)
			})
		})

		convey.Convey("同步超时", func() {
			builder := newFakeBuilder()
			builder.syncResult = func(*SyncRequest) (int64, error) {
				// 永远不返回，靠同步超时把它掐掉。
				select {}
			}
			answerer := newFakeAnswerer(builder, "refs")
			svc, repo, _, _, _ := newTestMirror(t, builder, func(rt *setting_svc.RuntimeSettings) {
				rt.MutableTTLSeconds = 60
				rt.GitSyncTimeoutSeconds = 1
			})
			withAnswerer(svc, answerer)
			before := readyMirror(t, repo, 3600)

			convey.So(local(svc).Answer(context.Background(), advertise()), convey.ShouldBeNil)
			row := repo.find(testHost, testRepo)
			convey.So(row.State, convey.ShouldEqual, git_entity.MirrorReady)
			convey.So(row.LastSyncAt, convey.ShouldEqual, before.LastSyncAt)
		})
	})
}

// TestAnswer_ConcurrentSyncsAreMergedIntoOne 并发的同步合并成一次（镜像生命周期一节）。
//
// 冷启动时几十个客户端同时 clone 同一个仓库，不合并就是把并发原样放大到上游。
func TestAnswer_ConcurrentSyncsAreMergedIntoOne(t *testing.T) {
	convey.Convey("同一个仓库的并发同步只打上游一次", t, func() {
		builder := newFakeBuilder()
		builder.syncGate = make(chan struct{})
		answerer := newFakeAnswerer(builder, "refs")
		svc, repo, _, _, _ := newTestMirror(t, builder, func(rt *setting_svc.RuntimeSettings) {
			rt.MutableTTLSeconds = 60
		})
		withAnswerer(svc, answerer)
		readyMirror(t, repo, 3600)

		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if answer := local(svc).Answer(context.Background(), advertise()); answer != nil {
					_ = answer.Body.Close()
				}
			}()
		}
		// 等第一趟同步进门，其余七个这时只能各自走穿透。
		<-builder.syncStarted
		close(builder.syncGate)
		wg.Wait()

		convey.So(builder.syncCount(), convey.ShouldEqual, 1)
	})
}

// TestAnswer_MirrorIsInUseWhileBodyIsOpen 本地应答期间这份镜像算「正在被读」。
//
// 配额淘汰整仓删目录，删掉一份正在被打包的镜像会让那一次 clone 半路散架。
func TestAnswer_MirrorIsInUseWhileBodyIsOpen(t *testing.T) {
	convey.Convey("应答体没关掉之前，这份镜像不可删", t, func() {
		builder := newFakeBuilder()
		answerer := newFakeAnswerer(builder, "refs")
		svc, repo, _, _, _ := newTestMirror(t, builder, nil)
		withAnswerer(svc, answerer)
		readyMirror(t, repo, 10)

		convey.So(local(svc).InUse(testHost, testRepo), convey.ShouldBeFalse)
		answer := local(svc).Answer(context.Background(), advertise())
		convey.So(answer, convey.ShouldNotBeNil)
		convey.So(local(svc).InUse(testHost, testRepo), convey.ShouldBeTrue)

		convey.So(answer.Body.Close(), convey.ShouldBeNil)
		convey.So(local(svc).InUse(testHost, testRepo), convey.ShouldBeFalse)
		convey.Convey("重复关不会把计数减穿", func() {
			convey.So(answer.Body.Close(), convey.ShouldBeNil)
			convey.So(local(svc).InUse(testHost, testRepo), convey.ShouldBeFalse)
		})
	})
}

// TestAnswer_UnanswerableRequestFallsBack 本地答不了的形态（shallow / filter）降级
// 为穿透，而不是把一个错误发给客户端——目标 (d) 在服务层这一半。
func TestAnswer_UnanswerableRequestFallsBack(t *testing.T) {
	convey.Convey("答不了的请求给回 nil", t, func() {
		builder := newFakeBuilder()
		answerer := newFakeAnswerer(builder, "")
		answerer.err = gitsmartNotAnswerable()
		svc, repo, _, _, _ := newTestMirror(t, builder, nil)
		withAnswerer(svc, answerer)
		readyMirror(t, repo, 10)

		answer := local(svc).Answer(context.Background(), &AnswerRequest{
			Host: testHost, Repo: testRepo,
			Body: []byte(strings.Repeat("0", 4)),
		})
		convey.So(answer, convey.ShouldBeNil)
		convey.So(local(svc).InUse(testHost, testRepo), convey.ShouldBeFalse)
	})
}

// TestAnswer_UnusableWhenUpstreamGone 上游停用、删除或关掉 git 协议之后，盘上
// 那份镜像不再发出去。
//
// 判据挂在 Answer 上而不是别的什么查询方法上：白名单这道闸只有落在**请求真正
// 走的那条路**上才算数，而 clone 走的正是这里。停用等同于不在表里，删除与关掉
// git 协议同理——三者在这一层折叠成同一个结果（决策 6），盘上有没有副本不影响
// 这个判断。answerer 一次都不该被调用：连目录都不该去碰。
func TestAnswer_UnusableWhenUpstreamGone(t *testing.T) {
	convey.Convey("一份已经建好、还在 TTL 内的镜像", t, func() {
		builder := newFakeBuilder()
		answerer := newFakeAnswerer(builder, "refs")
		svc, repo, _, _, _ := newTestMirror(t, builder, nil)
		withAnswerer(svc, answerer)
		readyMirror(t, repo, 10)
		ctx := context.Background()

		convey.Convey("上游还在、还开着 git 时由本地应答", func() {
			answer := local(svc).Answer(ctx, advertise())
			convey.So(answer, convey.ShouldNotBeNil)
			_ = answer.Body.Close()
			convey.So(answerer.callCount(), convey.ShouldEqual, 1)
		})

		cases := map[string]func(){
			"上游被停用之后不发出去": func() {
				registerUpstream(t, testHost, false, upstream_entity.ProtocolGit)
			},
			"上游被删掉之后不发出去": func() {
				registerUpstream(t, "someone.else.test", true, upstream_entity.ProtocolGit)
			},
			"上游关掉 git 协议之后不发出去": func() {
				registerUpstream(t, testHost, true, upstream_entity.ProtocolStatic)
			},
		}
		for name, arrange := range cases {
			convey.Convey(name, func() {
				arrange()
				convey.So(local(svc).Answer(ctx, advertise()), convey.ShouldBeNil)
				convey.So(answerer.callCount(), convey.ShouldEqual, 0)
			})
		}
	})
}
