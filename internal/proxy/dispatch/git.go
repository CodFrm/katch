package dispatch

import (
	"net/url"
	"strings"
)

// git smart HTTP 的两个服务名，按协议原样写死。
const (
	uploadPackService  = "git-upload-pack"
	receivePackService = "git-receive-pack"
)

// GitService 一次 git 请求要的是哪一个服务。
type GitService int

const (
	// GitNone 不是 git 端点。零值即此项：Target 上带的就是这个结构，
	// 认不出来时按原来的静态路径处理。
	GitNone GitService = iota
	// GitUploadPack 取（clone/fetch），katch 服务的只有这一半。
	GitUploadPack
	// GitReceivePack 推（push），一律拒绝：katch 是镜像不是代码托管（决策 4）。
	GitReceivePack
)

// String 让表驱动用例的失败信息可读。
func (s GitService) String() string {
	switch s {
	case GitNone:
		return "None"
	case GitUploadPack:
		return uploadPackService
	case GitReceivePack:
		return receivePackService
	}
	return "Unknown"
}

// GitEndpoint git 端点的识别结果。
type GitEndpoint struct {
	// Service 这次请求要的服务，GitNone 表示这不是 git 端点。
	Service GitService
	// Advertise true 是 ref 广播（GET <仓库>/info/refs?service=...），
	// false 是随后的协商（POST <仓库>/<service>）。两者的请求形态与响应
	// 类型都不同，本地应答时也是两条不同的路，所以分开而不是各算一种服务。
	Advertise bool
	// Repo 仓库在上游侧的路径，转义形态、以 / 开头，端点后缀已经去掉。
	// 端点就是整条路径时是 "/"——仓库落在回源地址的基路径上。
	Repo string
}

// IsGit 这是不是一个 git 端点。
func (e GitEndpoint) IsGit() bool { return e.Service != GitNone }

// IsUploadPack 这是不是只读的那一半。
//
// 和 IsGit 分开：拒绝 push 靠的正是这一句，合成一句会让「是 git 请求」
// 和「可以服务」变成同一个判断，那时 receive-pack 就跟着被服务了。
func (e GitEndpoint) IsUploadPack() bool { return e.Service == GitUploadPack }

// ClassifyGit 从上游侧路径与原始查询串识别 git 端点。
//
// 入参是 Classify 给出的 rest 与 http.Request.URL.RawQuery，两者都保持转义形态。
// 它是 Classify 之后的第二步而不是 Classify 的一部分：分段规则只看路径，而
// info/refs 的判据在查询串上，把查询串塞进 Classify 会让每一个只关心「这段是不是
// 主机名」的调用方都被迫多传一个参数。
//
// 只认 smart HTTP 的两个端点（决策 3）：
//
//  1. <仓库>/info/refs 且查询串里 service=git-upload-pack 或 git-receive-pack；
//  2. <仓库>/git-upload-pack 与 <仓库>/git-receive-pack。
//
// 其余一律不是 git——包括不带 service 的 info/refs：那是哑协议（dumb HTTP）的
// 文件请求，它本来就是普通静态资源，按 static 走即可。方法不在入参里：这里只
// 回答「这个 URL 是不是那两个端点」，谁能用什么方法访问是拉取路径那一层的闸。
func ClassifyGit(path, rawQuery string) GitEndpoint {
	if repo, ok := cutEndpoint(path, "/info/refs"); ok {
		switch advertisedService(rawQuery) {
		case uploadPackService:
			return GitEndpoint{Service: GitUploadPack, Advertise: true, Repo: repo}
		case receivePackService:
			return GitEndpoint{Service: GitReceivePack, Advertise: true, Repo: repo}
		}
		return GitEndpoint{}
	}
	if repo, ok := cutEndpoint(path, "/"+uploadPackService); ok {
		return GitEndpoint{Service: GitUploadPack, Repo: repo}
	}
	if repo, ok := cutEndpoint(path, "/"+receivePackService); ok {
		return GitEndpoint{Service: GitReceivePack, Repo: repo}
	}
	return GitEndpoint{}
}

// cutEndpoint 把端点后缀切下来，返回仓库路径。
//
// 必须整段匹配：/a/b/git-upload-pack.txt 是一个普通文件，而 /a/bgit-upload-pack
// 连分隔符都没有。后缀本身带着前导斜杠，两者都因此落不进来。
func cutEndpoint(path, suffix string) (string, bool) {
	repo, ok := strings.CutSuffix(path, suffix)
	if !ok {
		return "", false
	}
	if repo == "" {
		// 端点就是整条路径：仓库落在回源地址的基路径上。上游侧路径一律
		// 以 / 开头，这里也给回一个 /，免得调用方拿到一个空字符串当仓库名。
		return "/", true
	}
	return repo, true
}

// advertisedService 取查询串里的 service，取不到就是空串。
//
// 查询串坏掉时当作没有 service：那样的请求落回 static，至多是一次 404 或一份
// 普通文件，而猜一个服务出来会让一个畸形请求走进 git 这条路。
func advertisedService(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return ""
	}
	return values.Get("service")
}
