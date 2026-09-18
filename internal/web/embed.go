// Package web 把前端 SPA 嵌入 katch 二进制。
//
// dist 目录在 build 时由 frontend/dist 拷贝填充；dev 模式下前端由 vite 自己提供，
// 这里挂的是 make prepare-web-dist 造的占位文件。
package web

import (
	"bytes"
	"compress/gzip"
	"context"
	"embed"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/cago-frame/cago/configs"
	"github.com/cago-frame/cago/pkg/logger"
	"github.com/cago-frame/cago/server/mux"
	"github.com/gin-gonic/gin"

	"github.com/CodFrm/katch/internal/metrics"
	"github.com/CodFrm/katch/internal/proxy/dispatch"
	"github.com/CodFrm/katch/internal/service/cache_svc"
	"github.com/CodFrm/katch/internal/service/git_svc"
	"github.com/CodFrm/katch/internal/service/proxy_svc"
)

//go:embed dist
var distFS embed.FS

// immutableCacheControl 给 /assets/ 下的产物。vite 把内容 hash 写进文件名，内容一变
// 文件名就跟着变，所以这些 URL 的内容永不改写——正是 immutable 的定义。
//
// 不能指望 net/http 的默认行为：embed.FS 的 ModTime 是零值，http.ServeContent 因此
// 连 Last-Modified 都不会发，net/http 也不会自动生成 ETag。不显式给缓存头，每次打开
// 页面都要把整份 bundle 重新下一遍，连一次 304 都省不下来。
const immutableCacheControl = "public, max-age=31536000, immutable"

// revalidateCacheControl 给 index.html。它是那张指向当前一组 hash 的名片，**绝不能**
// 跟着一起被永久缓存——否则滚动更新之后浏览器永远拿着旧名片，新版本再也上不去。
const revalidateCacheControl = "no-cache"

// git 的应答是谁答的：local（本地镜像）或 passthrough（穿透上游）。
//
// 不复用 X-Katch-Cache：那个头的语义是「这个对象有没有回源」，而 git 的一次应答
// 不是一个对象——协商结果因客户端而异。两个头各自只回答一件事，才不会有人拿着
// 一个 MISS 去猜镜像状态。它同时是端到端用例的判据——判据必须是客户端观察得到
// 的东西，而不是某个内部标志位。
//
// 常量借 metrics 那一份：计数中间件按这个头分本地应答与穿透两档，两边各写一遍
// 字符串，改一处就会悄悄分叉。
const (
	gitSourceHeader      = metrics.GitSourceHeader
	gitSourcePassthrough = metrics.GitSourcePassthrough
	gitSourceLocal       = metrics.GitSourceLocal
)

// gitNoCache 本地应答自己带的缓存头。
//
// 穿透那一侧的头是上游给的，上游的 git 服务器本来就会发这一句；本地应答没有
// 上游可抄，得自己发：一次协商的结果因客户端而异，被中间任何一层缓存下来，
// 就是把一个客户端的协商结果发给另一个客户端（决策 9）。
const gitNoCache = "no-cache, max-age=0, must-revalidate"

// maxGitRequestBytes 愿意为「能不能本地答」读进内存的协商请求体上限。
//
// 要判断一个请求答不答得了，就得先把它解开，而解开就得先读进来。超过这个大小
// 的请求直接走穿透：一次 fetch 的 have 段再长也就几十 KB，真超了它更可能是
// 一个想让 katch 吃内存的请求，而不是一次正经的拉取。
const maxGitRequestBytes = 8 << 20

// NewNoRouteHandler 构造生产的 NoRoute 处理器：SPA 与拉取路径共用的那一个。
//
// 导出是为了端到端那一侧（internal/proxy/extension）能挂上**生产的**这一个，
// 而不是再写一份替身——替身漂了，守卫就守了个空。MountSPA 走的也是它。
func NewNoRouteHandler() (gin.HandlerFunc, error) {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return nil, err
	}
	return newNoRouteHandlerFS(sub), nil
}

// newNoRouteHandlerFS 是 newNoRouteHandler 的可注入形态：dist 从外面给，用例因此能
// 拿真实形状的产物（带 hash 的 js/css）来验缓存头，而不必依赖 make prepare-web-dist
// 造的那份占位 index.html。
func newNoRouteHandlerFS(sub fs.FS) gin.HandlerFunc {
	fileSrv := http.FileServer(http.FS(sub))

	return func(c *gin.Context) {
		path := c.Request.URL.Path
		kind, host, rest := dispatch.Classify(c.Request.URL.EscapedPath())
		// katch 自身端点未命中就是 404，不能回落 index.html：否则调用方拿到
		// 200 + HTML，而 200 不会被任何监控计成失败，问题被彻底藏住。
		if kind == dispatch.KindSelf {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		// dist 里真有这个文件就先给文件。这一步必须排在上游分发之前：
		// /favicon.ico 这类根目录下的产物名含点，按分段规则会被当成上游主机名，
		// 而 dist 是编译期固定的一小撮文件，让它优先才不会因为谁加了一条上游
		// 就把界面打坏。
		if f, ferr := sub.Open(strings.TrimPrefix(path, "/")); ferr == nil {
			_ = f.Close()
			setCacheHeaders(c, path)
			fileSrv.ServeHTTP(c.Writer, c.Request)
			return
		}
		if serveUpstream(c, kind, host, rest) {
			return
		}
		// /assets/ 下未命中，只可能是滚动更新期间浏览器拿着新副本的 index.html
		// 向旧副本要新文件。回落 index.html 会让浏览器把 HTML 当 JS 解析，
		// 直接 404 才让这次缺失可见、刷新即可自愈。
		if strings.HasPrefix(path, "/assets/") {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		// 其余未命中路径是 SPA 的前端路由，回落 index.html
		idx, err := fs.ReadFile(sub, "index.html")
		if err != nil {
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
		setCacheHeaders(c, path)
		c.Writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.ServeContent(c.Writer, c.Request, "index.html", time.Time{}, bytes.NewReader(idx))
	}
}

// serveUpstream 处理拉取路径，返回这次请求是否已由它接管。
//
// 它和 SPA 共用 NoRoute 而不是新开一条 gin 通配路由：通配路由会和 /api/v1
// 以及将来任何一条真实路由抢同一段前缀，而 NoRoute 天然是「所有已注册路由都没
// 命中之后」，顺序问题不存在。
func serveUpstream(c *gin.Context, kind dispatch.Kind, host, rest string) bool {
	switch kind {
	case dispatch.KindRegistryPing:
		// registry 客户端拿这个探测「对面是不是一个 v2 registry」，它固定发在
		// /v2/ 上、不带上游主机名，所以不查白名单也无从查起。
		c.Header("Docker-Distribution-Api-Version", "registry/2.0")
		c.Data(http.StatusOK, "application/json; charset=utf-8", []byte("{}"))
		return true
	case dispatch.KindInvalid:
		// 拿不出主机名或带着回溯段的请求，和主机不在白名单里一样 404。
		c.AbortWithStatus(http.StatusNotFound)
		return true
	case dispatch.KindRegistry, dispatch.KindStatic:
		serveProxy(c, kind, host, rest)
		return true
	case dispatch.KindSPA, dispatch.KindSelf:
		return false
	}
	return false
}

// serveProxy 取一个对象并把响应流式转发给客户端。
//
// 入口是 cache_svc 而不是 proxy_svc：缓存必须坐在拉取路径上，否则「第二次拉取由
// 磁盘服务」只在服务层成立，经 HTTP 拉两次仍然是回源两次。白名单那道闸没有被
// 绕开——cache_svc 未命中时仍旧落到 proxy_svc.Fetch，ErrUpstreamNotAllowed
// 依然从这里出来（判定只有一个出处）。
func serveProxy(c *gin.Context, kind dispatch.Kind, host, rest string) {
	// git 的端点寄生在 static 的路径空间里，由路径形态认出来（决策 3）。
	// registry 有自己的路径空间，/v2/ 之下不存在 git 端点。
	git := dispatch.GitEndpoint{}
	if kind == dispatch.KindStatic {
		git = dispatch.ClassifyGit(rest, c.Request.URL.RawQuery)
	}
	if git.IsGit() && !git.IsUploadPack() {
		// push 一律拒绝（决策 4）：katch 是镜像不是代码托管，而且不转发客户端
		// 凭据意味着写操作无从鉴权。403 而不是 404，且**不问上游表**：拒绝的是
		// 这个动作，与这台主机在不在白名单里无关；按主机分别给 403 与 404，
		// 那两个状态码的差别本身就成了一个主机名探针。响应体为空，同上。
		c.AbortWithStatus(http.StatusForbidden)
		return
	}
	if !methodAllowed(c.Request.Method, git) {
		c.AbortWithStatus(http.StatusMethodNotAllowed)
		return
	}
	ctx := c.Request.Context()
	request := gitRequestBody(c.Request, git)
	// 请求体已经在入口解开了的话，转发的头里不能再带着那个编码声明。
	forwardHeader := c.Request.Header
	if request.decoded {
		forwardHeader = c.Request.Header.Clone()
		forwardHeader.Del("Content-Encoding")
	}
	// 本地镜像答得了就由它答，答不了才穿透（决策 5）。判断必须在这里做完：
	// 响应一旦开了头就没法再改主意。
	if request.answerable && serveGitLocal(c, host, git, request.local) {
		return
	}
	body, meta, err := cache_svc.Cache().Get(ctx, &proxy_svc.Target{
		Kind:          kind,
		Host:          host,
		Path:          rest,
		RawQuery:      c.Request.URL.RawQuery,
		Method:        c.Request.Method,
		Header:        forwardHeader,
		Git:           git,
		Body:          request.upstream,
		ContentLength: request.length,
	})
	if err != nil {
		if errors.Is(err, proxy_svc.ErrUpstreamNotAllowed) {
			// 空响应体、不加任何头。主机名一旦出现在响应里，katch 就成了探测
			// 内网主机是否存在的工具——能回显的就是表里有的（决策 6）。
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		// 回源失败是 502 而不是 404：404 会让客户端把「这个对象不存在」当成
		// 结论记下来，而这只是上游此刻不可达。主机名只进日志，不进响应。
		logger.Ctx(ctx).Sugar().Errorw("回源失败", "host", host, "path", rest, "err", err)
		c.AbortWithStatus(http.StatusBadGateway)
		return
	}
	defer func() { _ = body.Close() }()
	// 上游的状态码原样透传，包括 4xx——那是上游对这个对象的判断，改写它只会
	// 让客户端拿到一个和真实源站不一致的结果。
	header := c.Writer.Header()
	for k, values := range meta.Header {
		for _, v := range values {
			header.Add(k, v)
		}
	}
	if git.IsGit() {
		// Set 而不是 Add：上游也可能是另一台 katch，它那一份归因说的是它自己
		// 那一跳，留着会让客户端看到两个值。
		header.Set(gitSourceHeader, gitSourcePassthrough)
	}
	c.Writer.WriteHeader(meta.StatusCode)
	// io.Copy 而不是先读进内存：一个几百 MB 的镜像层读完再发，既把首字节推迟到
	// 整体下载完成，又让并发拉取直接把内存吃光。
	if _, err := io.Copy(c.Writer, body); err != nil {
		// 客户端断开也会走到这里，所以是 warn 不是 error。
		logger.Ctx(ctx).Sugar().Warnw("转发响应体中断", "host", host, "path", rest, "err", err)
	}
}

// methodAllowed 方法闸。
//
// 镜像站只读：GET/HEAD 之外的方法要么是写操作，要么是探测，转发一个没有请求体的
// POST 给上游拿到的结果没有任何意义。唯一的例外是 git 的协商端点——它的请求体
// 就是客户端要哪些对象这件事本身，协议规定用 POST 发（决策 3）。
//
// 放行的判据是**路径形态**，不是「这台主机开了 git」：闸在白名单之前，按主机放行
// 会让 405 与 404 的差别变成一个主机名探针。没开 git 的主机照样走到白名单那一步，
// 在那里拿到和「表里没有」一模一样的空 404。
func methodAllowed(method string, git dispatch.GitEndpoint) bool {
	if method == http.MethodGet || method == http.MethodHead {
		return true
	}
	return method == http.MethodPost && gitNegotiation(git)
}

// gitNegotiation 这次请求是不是 upload-pack 的协商——git 端点里唯一带请求体、
// 也唯一用 POST 发的那一个。ref 广播是 GET，它没有请求体。
func gitNegotiation(git dispatch.GitEndpoint) bool {
	return git.IsUploadPack() && !git.Advertise
}

// gitBody 一次 git 拉取的请求体的两份形态。
type gitBody struct {
	// local 交给本地应答去看的那一份，ref 广播时为空——广播不看请求体。
	local []byte
	// answerable 这一次能不能问本地镜像。请求体大到读不进来时是假：那时
	// 手上没有一份完整的请求可看，问了也只能得到「答不了」。
	answerable bool
	// upstream 穿透时转给上游的那一份，nil 表示这次请求没有请求体。
	upstream io.Reader
	// length 上面那一份有多长，-1 表示分块。
	length int64
	// decoded 表示 upstream 那份的 Content-Encoding 已经在入口解开了：
	// 转发时得把这个头摘掉，否则就是反过来的同一个错——声明 gzip、送的是明文。
	decoded bool
}

// gitRequestBody 把请求体读成本地应答与穿透各自要的形态。
//
// 只有 git 的协商请求带请求体，别的拉取一律不带——把客户端的请求体转给上游是
// 一件要显式开的事，默认不转。
//
// 协商请求要读两遍（先给本地应答判断，答不了再给上游），所以它先落进内存再
// 分发：读完了不还回去，上游收到的就是一个空请求。
func gitRequestBody(r *http.Request, git dispatch.GitEndpoint) gitBody {
	if !git.IsUploadPack() {
		return gitBody{}
	}
	if r.Body == nil || r.Method != http.MethodPost || !gitNegotiation(git) {
		// ref 广播：没有请求体，但照样可以由本地答。
		return gitBody{answerable: true}
	}
	buffered, err := io.ReadAll(io.LimitReader(r.Body, maxGitRequestBytes+1))
	if err != nil {
		// 请求体读不完（客户端半路走了）。把已经拿到的那一段原样往上游送，
		// 由上游给出它的判断——这里不替客户端编一个完整的请求出来。
		return gitBody{upstream: bytes.NewReader(buffered), length: int64(len(buffered))}
	}
	if len(buffered) > maxGitRequestBytes {
		// 太大：不留在内存里，把已读的那一段和剩下的接起来继续流给上游。
		// ContentLength 原样抄客户端那一侧，-1 表示分块。
		//
		// 这一份**不解压**：手上从来没有完整的一份，流到一半去解反而会把字节弄丢。
		// 它带着什么编码就原样转给上游，Content-Encoding 在回源头的转发白名单里
		// （origin.forwardedRequestHeaders），那个头会跟着走。
		return gitBody{
			upstream: io.MultiReader(bytes.NewReader(buffered), r.Body),
			length:   r.ContentLength,
		}
	}
	plain, decoded := decodeGitBody(buffered, r.Header.Get("Content-Encoding"))
	return gitBody{
		local: plain, answerable: true,
		// 长度用实际读到的字节数，不用客户端声明的那个：两者不一致时，
		// 按声明的发会让上游一直等一段永远不会来的字节。
		upstream: bytes.NewReader(plain), length: int64(len(plain)),
		decoded: decoded,
	}
}

// decodeGitBody 按 Content-Encoding 解开协商请求体。
//
// git 对超过 1KB 的协商请求体会压成 gzip 再发，而 ref 多的仓库正好落在这条线上。
// 不解开的话两头都坏：回源头的白名单里虽然有 Content-Encoding，但本地镜像读不懂
// 压缩字节，于是一次都答不上来——而「ref 多到要压缩」正是最该命中镜像的那一类。
// 穿透那一侧也不剩好处：多一个上游去解压，就多一处可以解错的地方。
//
// 解不开就把原字节原样交出去并如实报告没解开。上游的 git 比这里的解压器认得多，
// 由它给出判断比在这里替它下一个结论可靠。
func decodeGitBody(body []byte, contentEncoding string) ([]byte, bool) {
	encoding := strings.TrimSpace(contentEncoding)
	// x-gzip 是 gzip 的别名（RFC 9110 的 8.4.1.2），历史上有人只发这个。
	if !strings.EqualFold(encoding, "gzip") && !strings.EqualFold(encoding, "x-gzip") {
		return body, false
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return body, false
	}
	defer func() { _ = zr.Close() }()
	plain, err := io.ReadAll(zr)
	if err != nil {
		return body, false
	}
	return plain, true
}

// serveGitLocal 尝试由本地镜像应答，返回这次请求是否已经答完。
//
// 答不了不是错误：那是设计里的默认路径（决策 5）——镜像还没建成、请求带着
// shallow 或 --filter、上游的 TTL 到了而同步没成，一律回到穿透那一条。
func serveGitLocal(c *gin.Context, host string, git dispatch.GitEndpoint, body []byte) bool {
	ctx := c.Request.Context()
	answer := git_svc.LocalAnswer(ctx, &git_svc.AnswerRequest{
		Host: host, Repo: git.Repo, Advertise: git.Advertise, Body: body,
	})
	if answer == nil {
		return false
	}
	// 关掉它同时放掉镜像上的读占用，所以这一句不能漏。
	defer func() { _ = answer.Body.Close() }()
	header := c.Writer.Header()
	header.Set("Content-Type", answer.ContentType)
	header.Set("Cache-Control", gitNoCache)
	header.Set(gitSourceHeader, gitSourceLocal)
	c.Writer.WriteHeader(http.StatusOK)
	// 边打边发：pack 是现打的，等它打完再发会把首字节推迟到整个仓库遍历完。
	if _, err := io.Copy(c.Writer, answer.Body); err != nil {
		// 客户端断开也会走到这里，所以是 warn 不是 error。
		logger.Ctx(ctx).Sugar().Warnw("本地应答中断", "host", host, "repo", git.Repo, "err", err)
	}
	return true
}

func setCacheHeaders(c *gin.Context, path string) {
	// 声明 Vary，否则共享缓存会把按某个 Accept-Encoding 协商出的副本发给别的客户端。
	c.Writer.Header().Set("Vary", "Accept-Encoding")
	if strings.HasPrefix(path, "/assets/") {
		c.Writer.Header().Set("Cache-Control", immutableCacheControl)
		return
	}
	c.Writer.Header().Set("Cache-Control", revalidateCacheControl)
}

// MountSPA 把 SPA 处理器挂到 gin 的 NoRoute 上。
//
// 这里没有做 gzip 预压缩：katch 的管理界面体积小，且部署形态默认在反代之后，
// 压缩交给反代更合适。若将来 bundle 变大又要直接对外，再在 newNoRouteHandlerFS
// 里加一层构造期预压缩（dist 编译期固定，压一次即可，不必为每个请求烧 CPU）。
func MountSPA(_ context.Context, _ *configs.Config) error {
	mux.RegisterMiddleware(func(_ *configs.Config, engine *gin.Engine) error {
		handler, err := NewNoRouteHandler()
		if err != nil {
			return err
		}
		engine.NoRoute(handler)
		return nil
	})
	return nil
}
