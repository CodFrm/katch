package git_svc

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/go-git/go-git/v5/plumbing/transport/server"
	"github.com/smartystreets/goconvey/convey"

	"github.com/CodFrm/katch/internal/model/entity/git_entity"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/service/setting_svc"
)

// 这一组验的是两道时间闸：停滞闸（上游不送字节了）与总时限闸（整段墙钟太久）。
//
// 上游仍旧是进程内那台假上游，只是在它前面加了一层「送字节的节奏」——一次
// 建镜像会不会被掐断，取决于上游以什么速率吐包，而这正是公网证不了的事：
// 真仓库的体积与当天的网速都不在用例的掌控内。

// shapedProtocol 带节奏的那台假上游用的协议名。
const shapedProtocol = "katchslow"

// shapedOrigin 它的回源地址。
const shapedOrigin = shapedProtocol + "://upstream"

// packShaper 一个仓库的 pack 以什么节奏送到客户端手里。
type packShaper func(ctx context.Context, repo string, pack io.ReadCloser) io.ReadCloser

// serveShaped 把一个目录挂成进程内的 git 上游，包体按 shape 给的节奏吐。
func serveShaped(t *testing.T, base string, shape packShaper) {
	t.Helper()
	inner := server.NewClient(server.NewFilesystemLoader(osfs.New(base)))
	client.InstallProtocol(shapedProtocol, &shapedTransport{inner: inner, shape: shape})
	t.Cleanup(func() { client.InstallProtocol(shapedProtocol, nil) })
}

// shapedRemote 拼出一个仓库在这台假上游上的地址。
func shapedRemote(name string) string {
	return shapedOrigin + shapedRepo(name)
}

// shapedRepo 一个仓库在这台假上游上的路径。
func shapedRepo(name string) string {
	return "/" + name + "/.git"
}

type shapedTransport struct {
	inner transport.Transport
	shape packShaper
}

func (t *shapedTransport) NewUploadPackSession(ep *transport.Endpoint,
	auth transport.AuthMethod,
) (transport.UploadPackSession, error) {
	session, err := t.inner.NewUploadPackSession(ep, auth)
	if err != nil {
		return nil, err
	}
	return &shapedSession{UploadPackSession: session, repo: ep.Path, shape: t.shape}, nil
}

func (t *shapedTransport) NewReceivePackSession(ep *transport.Endpoint,
	auth transport.AuthMethod,
) (transport.ReceivePackSession, error) {
	return t.inner.NewReceivePackSession(ep, auth)
}

type shapedSession struct {
	transport.UploadPackSession
	repo  string
	shape packShaper
}

// UploadPack 把包体换成一个有节奏的 reader。
//
// 只包包体，不包 ref 广播：停滞闸计的是「拉包的时候上游不说话了」，
// 而广播那一段在 AdvertisedReferences 上，两者不是一回事。
func (s *shapedSession) UploadPack(ctx context.Context,
	req *packp.UploadPackRequest,
) (*packp.UploadPackResponse, error) {
	resp, err := s.UploadPackSession.UploadPack(ctx, req)
	if err != nil {
		return nil, err
	}
	return packp.NewUploadPackResponseWithPackfile(req, s.shape(ctx, s.repo, resp)), nil
}

// stallAfter 送够 n 字节就再也不送，像一条还连着、但不再有数据过来的连接。
//
// 挂起时认 ctx：真实回源也是这个样子（net/http 在请求的 ctx 被取消时让正在
// 进行的 body 读报错）。一个连取消都不认的假上游会让任何实现都挂死在那里，
// 那样的用例证不了任何事。
func stallAfter(n int64) packShaper {
	return func(ctx context.Context, _ string, pack io.ReadCloser) io.ReadCloser {
		return &stallReader{ctx: ctx, inner: pack, left: n}
	}
}

// stallOn 只对某一个仓库停滞，别的仓库照常拉。
func stallOn(name string, n int64) packShaper {
	stall := stallAfter(n)
	return func(ctx context.Context, repo string, pack io.ReadCloser) io.ReadCloser {
		if repo != shapedRepo(name) {
			return pack
		}
		return stall(ctx, repo, pack)
	}
}

type stallReader struct {
	ctx   context.Context
	inner io.ReadCloser
	left  int64
}

func (r *stallReader) Read(p []byte) (int, error) {
	if r.left <= 0 {
		<-r.ctx.Done()
		return 0, r.ctx.Err()
	}
	if int64(len(p)) > r.left {
		p = p[:r.left]
	}
	n, err := r.inner.Read(p)
	r.left -= int64(n)
	return n, err
}

func (r *stallReader) Close() error { return r.inner.Close() }

// trickleOn 只对某一个仓库以极慢但**不断**的速率送：每 every 送 chunk 字节。
//
// 「慢」和「停」要分得开：停滞闸看的是距上一次收到字节的时长，这种上游一直在
// 送，它就不该被停滞闸误伤（决策 2），到点掐断它的必须是总时限闸。
func trickleOn(name string, chunk int, every time.Duration) packShaper {
	return func(ctx context.Context, repo string, pack io.ReadCloser) io.ReadCloser {
		if repo != shapedRepo(name) {
			return pack
		}
		return &trickleReader{ctx: ctx, inner: pack, chunk: chunk, every: every}
	}
}

type trickleReader struct {
	ctx   context.Context
	inner io.ReadCloser
	chunk int
	every time.Duration
}

func (r *trickleReader) Read(p []byte) (int, error) {
	timer := time.NewTimer(r.every)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	}
	if len(p) > r.chunk {
		p = p[:r.chunk]
	}
	return r.inner.Read(p)
}

func (r *trickleReader) Close() error { return r.inner.Close() }

// TestBounds_TimeGatesCloseTheSameSeam 两道时间闸合的是和体积闸同一道闸：
// 到点之后盘上的**读**和**写**都报错，而且不必有人来取消一个 context。
//
// 这条守的是 Problem 4：go-git 的 delta 解析阶段整个不看 ctx（remote.go 的
// packfile.UpdateObjectStorage 一路到 plumbing/format/packfile/common.go 都没有
// ctx 参数），只认 ctx 的时间闸在那一段上形同虚设。把闸挂在读写两侧，任务 1
// 已经证过它掐得断一个正在跑的解析 goroutine。
func TestBounds_TimeGatesCloseTheSameSeam(t *testing.T) {
	convey.Convey("停滞时限到点时", t, func() {
		limit := newBounds(0, 20*time.Millisecond, 0)
		// 取消阀给一个什么都不做的：合闸这件事本身不依赖它。
		stop := limit.watch(func() {})
		defer stop()

		tripped := waitForTrip(limit)
		convey.So(errors.Is(tripped, ErrBuildStalled), convey.ShouldBeTrue)

		convey.Convey("读侧和写侧一起报错，解析 goroutine 的下一次读就撞在这里", func() {
			convey.So(errors.Is(limit.allowRead("objects/pack/tmp_pack_x"),
				ErrBuildStalled), convey.ShouldBeTrue)
			convey.So(errors.Is(limit.reserve(1), ErrBuildStalled), convey.ShouldBeTrue)
		})
	})

	convey.Convey("总时限到点时", t, func() {
		limit := newBounds(0, 0, 20*time.Millisecond)
		stop := limit.watch(func() {})
		defer stop()

		tripped := waitForTrip(limit)
		convey.So(errors.Is(tripped, ErrBuildTimedOut), convey.ShouldBeTrue)
		convey.So(errors.Is(limit.allowRead("objects/pack/tmp_pack_x"),
			ErrBuildTimedOut), convey.ShouldBeTrue)
	})

	convey.Convey("fetch 阶段收工之后停滞闸就不再计时", t, func() {
		// 解析阶段网络上本来就静默，CPU 却满载（Problem 2 实测的那 19 分钟）。
		// 把它当成一次停滞会误杀一次合法的解析（决策 2），那一段归总时限管。
		limit := newBounds(0, 20*time.Millisecond, 0)
		limit.fetchDone()
		stop := limit.watch(func() {})
		defer stop()

		time.Sleep(120 * time.Millisecond)
		convey.So(limit.err(), convey.ShouldBeNil)
	})
}

// TestCloneBounded_StallGateStopsTimingWhenFetchEnds 一次真的 clone 走完之后，
// 停滞闸必须已经收工——再久的静默也不该被它当成一次停滞。
//
// 上面那条 fetchDone 的用例是自己动手合的闸，它证不了「go-git 真的会走到那个
// 收工点」。收工点挂在 boundedStorage.PackfileWriter 返回的那个 writer 的 Close
// 上：go-git 哪天不再按 storer.PackfileWriter 要 writer，或者这一层被摘掉，
// 停滞闸就会一路计到解析阶段里去——而解析阶段网络上本来就静默、CPU 却满载
// （Problem 2 实测的那 19 分钟），那正是决策 2 明确要避免的误杀。
func TestCloneBounded_StallGateStopsTimingWhenFetchEnds(t *testing.T) {
	convey.Convey("一次正常的 clone 走完之后", t, func() {
		base := t.TempDir()
		remote := serveRepos(t, base)
		createSourceRepo(t, filepath.Join(base, "source"))

		dir := filepath.Join(t.TempDir(), "mirror", "source.git")
		// 5 秒：进程内这台上游是毫秒级的，这一路上停滞闸不该响。
		limit := newBounds(0, 5*time.Second, 0)
		convey.So(cloneBounded(context.Background(), dir, remote("source"), limit),
			convey.ShouldBeNil)

		convey.Convey("停滞闸不再计时：之后静默一个小时也不算停滞", func() {
			convey.So(limit.overdue(time.Now().Add(time.Hour)), convey.ShouldBeNil)
		})
	})
}

// waitForTrip 等闸合上，返回合闸的原因；等太久就当它没合。
func waitForTrip(limit *bounds) error {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := limit.err(); err != nil {
			return err
		}
		time.Sleep(time.Millisecond)
	}
	return nil
}

// TestEnsure_StallGateStopsAnUpstreamThatWentQuiet 目标：上游送了一段字节之后
// 挂起不再送，建镜像在停滞时限内被中止、转 failed、last_error 指明是停滞闸；
// 现场清干净，并发名额立刻归还——下一个仓库马上就能开始建。
func TestEnsure_StallGateStopsAnUpstreamThatWentQuiet(t *testing.T) {
	convey.Convey("上游送了一段字节之后不再送时", t, func() {
		base := t.TempDir()
		serveShaped(t, base, stallOn("stalled", 64<<10))
		createBigSourceRepo(t, filepath.Join(base, "stalled"), 512<<10)
		createSourceRepo(t, filepath.Join(base, "quick"))

		registerUpstreamWithOrigin(t, testHost, shapedOrigin, upstream_entity.ProtocolGit)
		repo := newFakeMirrorRepo(t)
		rt := newFakeRuntime(t, func(rt *setting_svc.RuntimeSettings) {
			rt.GitBuildStallSeconds = 1
			// 总时限放得很宽：这一次要被掐断的理由只能是停滞。
			rt.GitBuildTimeoutSeconds = 600
			// 一个名额：下一个仓库能不能建起来，就是「名额还回来了没有」。
			rt.GitSyncConcurrency = 1
		})
		root := t.TempDir()
		svc := New(Options{Dir: root, Runtime: rt})
		ctx := context.Background()

		started := time.Now()
		svc.Ensure(ctx, testHost, shapedRepo("stalled"))
		svc.Ensure(ctx, testHost, shapedRepo("quick"))
		convey.So(svc.Quiesce(ctx), convey.ShouldBeNil)
		elapsed := time.Since(started)

		convey.Convey("停滞时限一到就中止，不是等上游自己断开", func() {
			convey.So(elapsed, convey.ShouldBeLessThan, 3*time.Second)
		})

		convey.Convey("记录转 failed，last_error 指明是停滞闸拦的", func() {
			row := repo.find(testHost, shapedRepo("stalled"))
			convey.So(row, convey.ShouldNotBeNil)
			convey.So(row.State, convey.ShouldEqual, git_entity.MirrorFailed)
			convey.So(row.LastError, convey.ShouldContainSubstring, ErrBuildStalled.Error())
		})

		convey.Convey("现场清干净：半个裸仓库不留在盘上", func() {
			dir, err := mirrorDir(root, testHost, shapedRepo("stalled"))
			convey.So(err, convey.ShouldBeNil)
			_, statErr := os.Stat(dir)
			convey.So(os.IsNotExist(statErr), convey.ShouldBeTrue)
		})

		convey.Convey("名额立刻归还：下一个仓库建起来了", func() {
			// 只有一个名额，被掐断的那一趟要是不还，这一条永远排不上队。
			row := repo.find(testHost, shapedRepo("quick"))
			convey.So(row, convey.ShouldNotBeNil)
			convey.So(row.State, convey.ShouldEqual, git_entity.MirrorReady)
			convey.So(row.SizeBytes, convey.ShouldBeGreaterThan, 0)
		})
	})
}

// TestEnsure_TotalGateStopsAnUpstreamThatCrawls 目标：上游以极慢但持续的速率送，
// 停滞闸不误伤，总时限到点中止、转 failed、last_error 指明是总时限闸；现场同样
// 清干净、名额同样立刻归还。
func TestEnsure_TotalGateStopsAnUpstreamThatCrawls(t *testing.T) {
	convey.Convey("上游一直在送、但慢到建不完时", t, func() {
		base := t.TempDir()
		// 每 20 毫秒 256 字节：一个几 MB 的仓库这样送要几分钟，而总时限只有一秒。
		serveShaped(t, base, trickleOn("slow", 256, 20*time.Millisecond))
		createBigSourceRepo(t, filepath.Join(base, "slow"), 512<<10)
		createSourceRepo(t, filepath.Join(base, "quick"))

		registerUpstreamWithOrigin(t, testHost, shapedOrigin, upstream_entity.ProtocolGit)
		repo := newFakeMirrorRepo(t)
		rt := newFakeRuntime(t, func(rt *setting_svc.RuntimeSettings) {
			// 停滞时限放得很宽：上游一直在送字节，它不该被这一道拦下。
			rt.GitBuildStallSeconds = 600
			rt.GitBuildTimeoutSeconds = 1
			rt.GitSyncConcurrency = 1
		})
		root := t.TempDir()
		svc := New(Options{Dir: root, Runtime: rt})
		ctx := context.Background()

		svc.Ensure(ctx, testHost, shapedRepo("slow"))
		convey.So(svc.Quiesce(ctx), convey.ShouldBeNil)

		convey.Convey("记录转 failed，last_error 指明是总时限闸而不是停滞闸", func() {
			row := repo.find(testHost, shapedRepo("slow"))
			convey.So(row, convey.ShouldNotBeNil)
			convey.So(row.State, convey.ShouldEqual, git_entity.MirrorFailed)
			convey.So(row.LastError, convey.ShouldContainSubstring, ErrBuildTimedOut.Error())
			convey.So(row.LastError, convey.ShouldNotContainSubstring, ErrBuildStalled.Error())
		})

		convey.Convey("现场清干净：半个裸仓库不留在盘上", func() {
			dir, err := mirrorDir(root, testHost, shapedRepo("slow"))
			convey.So(err, convey.ShouldBeNil)
			_, statErr := os.Stat(dir)
			convey.So(os.IsNotExist(statErr), convey.ShouldBeTrue)
		})

		convey.Convey("名额立刻归还：下一个仓库马上就能开始建", func() {
			// 这一个排在中止之后才登记，而不是和上面那个同时排队：总时限盖的是
			// 从开始建镜像到镜像可用的整段，排队那一段也算在里面（决策 3），
			// 同时登记的话它自己那一秒预算会先被队列吃光。
			//
			// 只有一个名额：被掐断的那一趟要是不还，这一条连一秒都等不到就得
			// 因为排队超时转 failed。
			started := time.Now()
			svc.Ensure(ctx, testHost, shapedRepo("quick"))
			convey.So(svc.Quiesce(ctx), convey.ShouldBeNil)
			row := repo.find(testHost, shapedRepo("quick"))
			convey.So(row, convey.ShouldNotBeNil)
			convey.So(row.State, convey.ShouldEqual, git_entity.MirrorReady)
			convey.So(time.Since(started), convey.ShouldBeLessThan, time.Second)
		})
	})
}

// TestEnsure_SyncTimeoutNoLongerCoversTheBuild 目标：git_sync_timeout_seconds
// 的生效范围收窄——它不再覆盖初次建镜像，那一段改由总时限闸管（决策 3）。
//
// 两者叠加的话，出厂的 600 秒总是先到，新设置永远不会生效，站长必须同时调大
// 两个才能建一个大仓库。
func TestEnsure_SyncTimeoutNoLongerCoversTheBuild(t *testing.T) {
	convey.Convey("把同步超时调到再小也管不到建镜像", t, func() {
		builder := newFakeBuilder()
		builder.gate = make(chan struct{})
		svc, repo, _, _, _ := newTestMirror(t, builder, func(rt *setting_svc.RuntimeSettings) {
			rt.GitSyncTimeoutSeconds = 0
		})
		ctx := context.Background()

		svc.Ensure(ctx, testHost, testRepo)
		<-builder.started
		close(builder.gate)
		convey.So(svc.Quiesce(ctx), convey.ShouldBeNil)

		convey.Convey("建镜像照常跑完，没有被同步超时掐断", func() {
			row := repo.find(testHost, testRepo)
			convey.So(row, convey.ShouldNotBeNil)
			convey.So(row.State, convey.ShouldEqual, git_entity.MirrorReady)
		})

		convey.Convey("时限改由两道闸传下去，而且不再挂在 ctx 上", func() {
			// 挂在 ctx 上的时限在 go-git 的解析阶段是失效的（Problem 4），
			// 实测卡死的那 19 分钟正发生在那里。
			req := builder.lastRequest()
			convey.So(req.StallTimeout, convey.ShouldEqual,
				defaultBuildStall(t)*time.Second)
			convey.So(req.TotalTimeout, convey.ShouldBeGreaterThan,
				(defaultBuildTimeout(t)-10)*time.Second)
			convey.So(builder.lastBuildHadDeadline(), convey.ShouldBeFalse)
		})
	})
}

// defaultBuildStall / defaultBuildTimeout 出厂的两道时间闸是多少秒。
//
// 从设置那一侧现读而不是抄一个字面量：抄下来的数一旦和出厂值分叉，用例会在
// 「设置改了但没传下去」这件事上绿着过去。
func defaultBuildStall(t *testing.T) time.Duration {
	t.Helper()
	return time.Duration(newFakeRuntime(t, nil).rt.GitBuildStallSeconds)
}

func defaultBuildTimeout(t *testing.T) time.Duration {
	t.Helper()
	return time.Duration(newFakeRuntime(t, nil).rt.GitBuildTimeoutSeconds)
}

// TestGitBuilder_TimeGatesLeaveNothingBehind 建镜像这一层自己也要把话说清楚：
// 报的是哪一道闸的错，盘上不留残余。
func TestGitBuilder_TimeGatesLeaveNothingBehind(t *testing.T) {
	convey.Convey("上游挂起时", t, func() {
		base := t.TempDir()
		serveShaped(t, base, stallOn("stalled", 64<<10))
		createBigSourceRepo(t, filepath.Join(base, "stalled"), 512<<10)

		dir := filepath.Join(t.TempDir(), "mirror", "stalled.git")
		_, err := NewGitBuilder().Build(context.Background(), &BuildRequest{
			Dir: dir, Remote: shapedRemote("stalled"), MaxBytes: 1 << 30,
			StallTimeout: 150 * time.Millisecond, TotalTimeout: 30 * time.Second,
		})
		convey.So(errors.Is(err, ErrBuildStalled), convey.ShouldBeTrue)
		// 体积闸不该被牵连：这次中止和仓库多大没有关系。
		convey.So(errors.Is(err, ErrRepoTooLarge), convey.ShouldBeFalse)
		_, statErr := os.Stat(dir)
		convey.So(os.IsNotExist(statErr), convey.ShouldBeTrue)
	})

	convey.Convey("上游慢到建不完时", t, func() {
		base := t.TempDir()
		serveShaped(t, base, trickleOn("slow", 256, 20*time.Millisecond))
		createBigSourceRepo(t, filepath.Join(base, "slow"), 512<<10)

		dir := filepath.Join(t.TempDir(), "mirror", "slow.git")
		_, err := NewGitBuilder().Build(context.Background(), &BuildRequest{
			Dir: dir, Remote: shapedRemote("slow"), MaxBytes: 1 << 30,
			StallTimeout: 30 * time.Second, TotalTimeout: 200 * time.Millisecond,
		})
		convey.So(errors.Is(err, ErrBuildTimedOut), convey.ShouldBeTrue)
		convey.So(errors.Is(err, ErrBuildStalled), convey.ShouldBeFalse)
		_, statErr := os.Stat(dir)
		convey.So(os.IsNotExist(statErr), convey.ShouldBeTrue)
	})

	convey.Convey("上游正常时两道闸都不误伤", t, func() {
		base := t.TempDir()
		serveShaped(t, base, stallOn("nobody", 1))
		createSourceRepo(t, filepath.Join(base, "source"))

		dir := filepath.Join(t.TempDir(), "mirror", "source.git")
		size, err := NewGitBuilder().Build(context.Background(), &BuildRequest{
			Dir: dir, Remote: shapedRemote("source"), MaxBytes: 1 << 30,
			StallTimeout: 5 * time.Second, TotalTimeout: 30 * time.Second,
		})
		convey.So(err, convey.ShouldBeNil)
		convey.So(size, convey.ShouldBeGreaterThan, 0)
	})
}

// TestBounds_TotalGateOutranksStallWhenBothAreDue 两道时间闸在同一刻都到点时，
// 报的必须是总时限那一个。
//
// 这条守的是决策 6 的用处：站长看 last_error 是为了知道该调哪一项。一次「整段
// 跑得太久」被报成「上游停止发送数据」，他会去换上游，而真正该调大的是总时限。
// 上面那几条用例里两道闸从来不同时到点（要么停滞放得很宽，要么总时限放得很宽），
// 把 overdue 里这两段判断对调过来，它们会一条不落地照旧绿着。
func TestBounds_TotalGateOutranksStallWhenBothAreDue(t *testing.T) {
	convey.Convey("停滞与总时限都已经过了点时", t, func() {
		limit := newBounds(0, 10*time.Millisecond, 20*time.Millisecond)

		cause := limit.overdue(limit.startedAt.Add(time.Second))
		convey.So(errors.Is(cause, ErrBuildTimedOut), convey.ShouldBeTrue)
		convey.So(errors.Is(cause, ErrBuildStalled), convey.ShouldBeFalse)
	})

	convey.Convey("只有停滞到点时，报的仍旧是停滞", t, func() {
		// 反面这一半不能省：没有它，上面那条靠「总时限永远赢」也能过。
		limit := newBounds(0, 10*time.Millisecond, time.Hour)

		cause := limit.overdue(limit.startedAt.Add(time.Second))
		convey.So(errors.Is(cause, ErrBuildStalled), convey.ShouldBeTrue)
		convey.So(errors.Is(cause, ErrBuildTimedOut), convey.ShouldBeFalse)
	})
}

// waitForMirrorState 等一条镜像记录走到某个状态，等太久就当它没走到。
//
// 后台那趟活是自己在跑的，用例只能从库这一侧看着它。
func waitForMirrorState(repo *fakeMirrorRepo, host, name, state string) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if row := repo.find(host, name); row != nil && row.State == state {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

// TestEnsure_QueueTimeoutNamesTheTotalGate 整段预算全花在排队等名额上时，
// last_error 要指明是总时限闸，而不是原样抛一句 context 的话（决策 6）。
//
// 总时限盖的是从开始建镜像到镜像可用的整段，排队那一段也算在里面（决策 3），
// 所以一趟活完全可能连 builder 的门都没进就到点了。「context deadline exceeded」
// 说不出该调哪一项设置，而这条路此前一次都没被走到过。
func TestEnsure_QueueTimeoutNamesTheTotalGate(t *testing.T) {
	convey.Convey("唯一的名额被占着、后来者的总时限先到时", t, func() {
		builder := newFakeBuilder()
		builder.gate = make(chan struct{})
		svc, repo, _, _, _ := newTestMirror(t, builder, func(rt *setting_svc.RuntimeSettings) {
			rt.GitSyncConcurrency = 1
			rt.GitBuildTimeoutSeconds = 1
		})
		ctx := context.Background()

		svc.Ensure(ctx, testHost, "/held.git")
		// 先确认名额确实被占住了，再放第二个进来排队。
		<-builder.started
		svc.Ensure(ctx, testHost, "/queued.git")

		// 等排队的那一趟自己到点。放行要在这之后：名额一还它就排上队了，
		// 那样这条用例证的就不是排队超时。
		convey.So(waitForMirrorState(repo, testHost, "/queued.git", git_entity.MirrorFailed),
			convey.ShouldBeTrue)
		close(builder.gate)
		convey.So(svc.Quiesce(ctx), convey.ShouldBeNil)

		convey.Convey("转 failed，last_error 指明是总时限闸、且花在排队上", func() {
			row := repo.find(testHost, "/queued.git")
			convey.So(row, convey.ShouldNotBeNil)
			convey.So(row.State, convey.ShouldEqual, git_entity.MirrorFailed)
			convey.So(row.LastError, convey.ShouldContainSubstring, ErrBuildTimedOut.Error())
			convey.So(row.LastError, convey.ShouldContainSubstring, "排队")
		})

		convey.Convey("排队超时的那一趟根本没进 builder 的门", func() {
			convey.So(builder.callCount(), convey.ShouldEqual, 1)
		})
	})
}
