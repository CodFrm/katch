package extension_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/smartystreets/goconvey/convey"

	"github.com/CodFrm/katch/internal/model/entity/git_entity"
	"github.com/CodFrm/katch/internal/repository/git_repo"
	"github.com/CodFrm/katch/internal/service/git_svc"
	"github.com/CodFrm/katch/internal/web"
)

// fakeGitHost 一个此前不存在的 git 上游主机名。
//
// 和 fakeUpstreamHost 受同一条约束：TestFakeUpstreamIsNotAFixture 会去核实它
// 不出现在任何生产 Go 文件里。一旦有人为了让这条用例通过而在某个 switch 里
// 认识它，这条用例测的就不再是扩展点。
const fakeGitHost = "git.brand-new-mirror.invalid"

// fakeGitRepo 上游那个仓库在路径上的样子。
const fakeGitRepo = "/demo.git"

// gitBinary 找到 git，没有就跳过——这一组用例的判据是「真 git 认不认这些字节」，
// 没有 git 就没有判据可言，假装跑过去比跳过更糟。
func gitBinary(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skip("环境里没有 git 客户端，真客户端对拉这条判据降级为运行时验证")
	}
	return path
}

// runGit 跑一条 git 命令，失败时把它的输出一起报出来。
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(gitBinary(t), args...) // #nosec G204 -- 参数全是用例自己拼的
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		// 不许弹任何交互：跑在 CI 上时一个凭据提示就是一次挂死。
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "gitconfig"),
		"GIT_CONFIG_SYSTEM="+filepath.Join(t.TempDir(), "gitconfig-system"),
		"GIT_AUTHOR_NAME=katch", "GIT_AUTHOR_EMAIL=katch@example.invalid",
		"GIT_COMMITTER_NAME=katch", "GIT_COMMITTER_EMAIL=katch@example.invalid",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s 失败：%v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitUpstream 一台真的 git 服务器：git http-backend 跑在 CGI 上。
//
// 不能用 go-git 自己搭一台来当上游：那样两头都是同一份实现，协议理解错了也
// 照样对得上。这里上游是真 git，客户端也是真 git，中间夹着 katch。
type gitUpstream struct {
	srv  *httptest.Server
	head string
	hits func() int64
}

func newGitUpstream(t *testing.T) *gitUpstream {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o750); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "init", "--quiet", "--initial-branch=main", work)
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("katch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "README.md")
	runGit(t, work, "commit", "--quiet", "-m", "初始提交")
	// 第二条提交让 --depth 1 成为一个真的约束：只有一条提交的仓库，浅 clone
	// 和全量 clone 长得一模一样，那条用例就什么也证不了。
	if err := os.WriteFile(filepath.Join(work, "NEWS.md"), []byte("第二条\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "NEWS.md")
	runGit(t, work, "commit", "--quiet", "-m", "第二条提交")
	head := runGit(t, work, "rev-parse", "HEAD")

	served := filepath.Join(root, "served")
	if err := os.MkdirAll(served, 0o750); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "clone", "--quiet", "--bare", work, filepath.Join(served, "demo.git"))

	// 原子计数：后台建镜像和客户端的请求会同时打到这台上游。
	hits := &atomic.Int64{}
	handler := &cgi.Handler{
		Path: gitBinary(t),
		Args: []string{"http-backend"},
		Env: []string{
			"GIT_PROJECT_ROOT=" + served,
			// 不必往每个仓库里塞 git-daemon-export-ok。
			"GIT_HTTP_EXPORT_ALL=1",
		},
	}
	upstream := &gitUpstream{head: head}
	upstream.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.srv.Close)
	upstream.hits = hits.Load
	return upstream
}

// startGitKatch 在 startKatch 那套装配之上补齐 git 这一侧：镜像仓储、镜像层，
// 以及**生产的**那个 NoRoute 处理器——本地应答就住在它里面。
func startGitKatch(t *testing.T) (*httptest.Server, *gitUpstream) {
	t.Helper()
	engine, _ := startKatch(t)
	git_repo.RegisterGitMirror(git_repo.NewGitMirror())
	mirrors := t.TempDir()
	before := git_svc.Mirror()
	git_svc.Register(git_svc.New(git_svc.Options{Dir: mirrors}))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := git_svc.Mirror().Quiesce(ctx); err != nil {
			t.Errorf("后台建镜像没能在用例结束前收尾：%v", err)
		}
		git_svc.Register(before)
	})
	handler, err := web.NewNoRouteHandler()
	if err != nil {
		t.Fatal(err)
	}
	engine.NoRoute(handler)

	// 真 git 客户端要的是一个真的监听端口，不能再用 ServeHTTP 直接喂。
	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)
	return srv, newGitUpstream(t)
}

// waitForMirror 等后台把镜像建成 ready。
func waitForMirror(t *testing.T) *git_entity.GitMirror {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := git_svc.Mirror().Quiesce(ctx); err != nil {
		t.Fatalf("等后台建镜像超时：%v", err)
	}
	list, err := git_svc.List(ctx)
	if err != nil {
		t.Fatalf("查镜像失败：%v", err)
	}
	for _, mirror := range list {
		if mirror.Host == fakeGitHost && mirror.Repo == fakeGitRepo &&
			mirror.State == git_entity.MirrorReady {
			return mirror
		}
	}
	t.Fatal("一次穿透 clone 之后镜像没有建成 ready")
	return nil
}

// TestGitClone_PassthroughThenLocal 任务目标 (a)(b)(e)。
//
// 一条真的 git 客户端 → katch → 真的 git 服务器：先穿透 clone 成功，后台把镜像
// 建起来，第二次 clone 由本地镜像应答，两次拿到同一条提交。判据全是客户端这一侧
// 观察得到的东西——上游被打了几次、响应头上写着谁答的、以及 rev-parse 出来的哈希。
func TestGitClone_PassthroughThenLocal(t *testing.T) {
	convey.Convey("经 katch 走完「穿透 clone → 后台建镜像 → 本地应答 clone」", t, func() {
		srv, upstream := startGitKatch(t)
		registerGitUpstream(t, srv, upstream)
		cloneURL := srv.URL + "/" + fakeGitHost + fakeGitRepo

		// ① 还没有镜像，只能穿透。
		first := t.TempDir()
		runGit(t, first, "clone", "--quiet", cloneURL, filepath.Join(first, "one"))
		convey.So(runGit(t, filepath.Join(first, "one"), "rev-parse", "HEAD"),
			convey.ShouldEqual, upstream.head)
		convey.So(upstream.hits(), convey.ShouldBeGreaterThan, 0)

		// ② 后台把镜像建成 ready，盘上真有一份裸仓库。
		mirror := waitForMirror(t)
		convey.So(mirror.State, convey.ShouldEqual, git_entity.MirrorReady)
		convey.So(mirror.SizeBytes, convey.ShouldBeGreaterThan, 0)
		convey.So(mirror.LastSyncAt, convey.ShouldBeGreaterThan, 0)

		// ③ 第二次 clone：一个字节都不该再问上游。
		hitsBefore := upstream.hits()
		second := t.TempDir()
		runGit(t, second, "clone", "--quiet", cloneURL, filepath.Join(second, "two"))

		convey.Convey("两次 clone 拿到的是同一条提交", func() {
			convey.So(runGit(t, filepath.Join(second, "two"), "rev-parse", "HEAD"),
				convey.ShouldEqual, upstream.head)
		})
		convey.Convey("第二次 clone 没有碰上游", func() {
			convey.So(upstream.hits(), convey.ShouldEqual, hitsBefore)
		})
		convey.Convey("本地应答的响应带 X-Katch-Git: local", func() {
			w := callGit(t, srv, http.MethodGet,
				fakeGitRepo+"/info/refs?service=git-upload-pack", "")
			convey.So(w.StatusCode, convey.ShouldEqual, http.StatusOK)
			convey.So(w.Header.Get("X-Katch-Git"), convey.ShouldEqual, "local")
			convey.So(w.Header.Get("Content-Type"), convey.ShouldEqual,
				"application/x-git-upload-pack-advertisement")
		})
		convey.Convey("指标上本地应答与穿透各自成一档", func() {
			local := gitRequestCount(t, "local")
			passthrough := gitRequestCount(t, "passthrough")
			convey.So(local, convey.ShouldBeGreaterThan, 0)
			convey.So(passthrough, convey.ShouldBeGreaterThan, 0)
		})
	})
}

// TestGitClone_ShallowFallsBackToPassthrough 任务目标 (d)。
//
// 本地那份 go-git 答不了 shallow，`--depth 1` 却必须照样可用：带 shallow 的请求
// 默认降级为穿透（能力边界一节）。判据有两个——响应头说这次是穿透，以及一次真的
// `git clone --depth 1` 确实拿到了仓库。
func TestGitClone_ShallowFallsBackToPassthrough(t *testing.T) {
	convey.Convey("带 shallow 的请求走穿透，--depth 1 照样 clone 得下来", t, func() {
		srv, upstream := startGitKatch(t)
		registerGitUpstream(t, srv, upstream)
		cloneURL := srv.URL + "/" + fakeGitHost + fakeGitRepo

		// 先让镜像建起来：本地答得了的时候还能不能 --depth，才是这条要验的。
		warm := t.TempDir()
		runGit(t, warm, "clone", "--quiet", cloneURL, filepath.Join(warm, "warm"))
		waitForMirror(t)

		convey.Convey("带 deepen 行的协商请求穿透上游", func() {
			hitsBefore := upstream.hits()
			body := pktLine("want "+upstream.head+" ofs-delta\n") +
				pktLine("deepen 1\n") + "0000" + pktLine("done\n")
			w := callGit(t, srv, http.MethodPost, fakeGitRepo+"/git-upload-pack", body)
			convey.So(w.Header.Get("X-Katch-Git"), convey.ShouldEqual, "passthrough")
			convey.So(upstream.hits(), convey.ShouldBeGreaterThan, hitsBefore)
		})

		convey.Convey("git clone --depth 1 成功，拿到的还是最新那条提交，且真的只有一条", func() {
			shallow := t.TempDir()
			runGit(t, shallow, "clone", "--quiet", "--depth", "1", cloneURL,
				filepath.Join(shallow, "shallow"))
			convey.So(runGit(t, filepath.Join(shallow, "shallow"), "rev-parse", "HEAD"),
				convey.ShouldEqual, upstream.head)
			convey.So(runGit(t, filepath.Join(shallow, "shallow"), "rev-list", "--count", "HEAD"),
				convey.ShouldEqual, "1")
		})
	})
}

// pktLine 把一行内容包成 pkt-line。
//
// 长度前缀自己算，不写死：一个算错的长度会让 katch 在解请求的第一步就放弃，
// 于是这条用例照样看到「穿透」两个字，而它想证的其实是「deepen 让它穿透」。
func pktLine(payload string) string {
	return fmt.Sprintf("%04x%s", len(payload)+4, payload)
}

// registerGitUpstream 只经管理接口把这台 git 上游写进去，同时开 static 与 git。
func registerGitUpstream(t *testing.T, srv *httptest.Server, upstream *gitUpstream) {
	t.Helper()
	body := `{"host":"` + fakeGitHost + `","protocols":["static","git"],` +
		`"origin":"` + upstream.srv.URL + `","enabled":true}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/admin/upstreams",
		strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = adminJSON()
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(got), `"code":0`) {
		t.Fatalf("注册 git 上游失败：%s", got)
	}
}

// callGit 直接发一次 git 端点的 HTTP 请求，用来看客户端看不到的那几个响应头。
func callGit(t *testing.T, srv *httptest.Server, method, path, body string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+"/"+fakeGitHost+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	return resp
}

// gitRequestCount 从进程级的 prometheus registry 上读一个 git 结果的计数。
//
// 读导出的那一份而不是内部字段：指标是对外契约，标签拼错了只有从这一头看得出来。
func gitRequestCount(t *testing.T, result string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var total float64
	for _, family := range families {
		if family.GetName() != "katch_requests_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, pair := range metric.GetLabel() {
				labels[pair.GetName()] = pair.GetValue()
			}
			if labels["upstream"] == fakeGitHost && labels["kind"] == "git" &&
				labels["result"] == result {
				total += metric.GetCounter().GetValue()
			}
		}
	}
	return total
}

// 让 gin 的测试模式在这一组用例里也成立。
var _ = gin.TestMode
