package git_svc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// gitBuilder 用纯 Go 的 go-git 建镜像，不依赖 git 二进制（决策 6）。
//
// 装一个 git 进镜像能白拿到协议 v2、shallow 与 partial clone，但产物就不再是
// 「单个静态二进制」，非 Docker 部署要自备 git。代价写在「本地应答的能力边界」。
type gitBuilder struct{}

// NewGitBuilder 构造基于 go-git 的实现。
func NewGitBuilder() Builder {
	return &gitBuilder{}
}

// mirrorDirPerm 镜像目录的权限。
//
// 0750 而不是 0755：镜像里可能有私有仓库的内容（上游给 katch 的那一份），
// 同机上的别的用户没有理由读得到。
const mirrorDirPerm fs.FileMode = 0o750

func (g *gitBuilder) Build(ctx context.Context, req *BuildRequest) (int64, error) {
	// 先清掉残留：PlainClone 要求目录是空的，而上一趟活可能是被超时掐断的，
	// 留下半个仓库。带着半个仓库重试只会拿到一句「目录非空」，而真正的原因
	// （上一次超时了）已经没人记得。
	if err := os.RemoveAll(req.Dir); err != nil {
		return 0, fmt.Errorf("清理镜像目录失败：%w", err)
	}
	if err := os.MkdirAll(filepath.Dir(req.Dir), mirrorDirPerm); err != nil {
		return 0, fmt.Errorf("建镜像目录失败：%w", err)
	}
	limit := newBounds(req.MaxBytes, req.StallTimeout, req.TotalTimeout)
	if err := cloneBounded(ctx, req.Dir, req.Remote, limit); err != nil {
		g.discard(req.Dir)
		return 0, err
	}
	// 量一遍盘是给配额用的，不是给上限用的：上限已经在收字节的时候管住了，
	// 走到这里的镜像按定义就在上限之内。
	size, err := dirSize(req.Dir)
	if err != nil {
		g.discard(req.Dir)
		return 0, err
	}
	return size, nil
}

// cloneBounded 把一个远端仓库镜像到 dir，收到的每一个字节都过一遍闸。
//
// 不用 git.PlainCloneContext 而是自己拼 storer：CloneOptions 上没有任何能挂
// 计量的地方，传输层又是由一个进程级全局注册表解析的（plumbing/transport/
// client），按次注入只能从 storer 这一侧进来。除了文件系统被套了一层，
// 拼法和 PlainCloneContext 对 isBare=true 做的事完全一样（repository.go 的
// PlainInitWithOptions）：worktree 传 nil 就是一个裸仓库。
func cloneBounded(ctx context.Context, dir, remote string, limit *bounds) error {
	// 自己的取消阀，只用来叫醒一次阻塞在上游连接上的读——闸合上之后盘上的读写
	// 立刻报错，但停在网络 Read 上的那一次谁也看不见。判据仍旧是闸：下面优先
	// 报 limit.err()，解析阶段的中止也全靠它。
	ctx, wake := context.WithCancel(ctx)
	defer wake()
	defer limit.watch(wake)()

	storage := &boundedStorage{
		Storage: filesystem.NewStorage(
			newBoundedFS(osfs.New(dir), limit), cache.NewObjectLRUDefault()),
		bounds: limit,
	}
	// Mirror 而不是普通 clone：要的是上游全部的 ref，不是一个默认分支的工作区。
	// 不带任何凭据——katch 不转发客户端的 Authorization，也没有自己的账号。
	_, err := git.CloneContext(ctx, storage, nil, &git.CloneOptions{
		URL:    remote,
		Mirror: true,
	})
	if err == nil {
		return nil
	}
	if tripped := limit.err(); tripped != nil {
		// 闸合上之后 go-git 会把这个错误在好几层里转手，最后露出来的可能是
		// 「pack 文件坏了」之类的下游症状。报闸自己的那一个，调用方才分得清
		// 这是一次拒绝而不是一次失败。
		return tripped
	}
	return err
}

// boundedStorage 在对象存储上再包一层，只为知道 fetch 阶段什么时候结束。
//
// 包在 storer 而不是文件系统上：收 pack 的那个 writer 是 go-git 按
// storer.PackfileWriter 要去的（plumbing/format/packfile/common.go 的
// UpdateObjectStorage），只有这一层拦得住它的 Close——而那个 Close 正好就是
// 「上游的字节收完了」这件事的发生时刻。
type boundedStorage struct {
	*filesystem.Storage
	bounds *bounds
}

func (s *boundedStorage) PackfileWriter() (io.WriteCloser, error) {
	writer, err := s.Storage.PackfileWriter()
	if err != nil {
		return nil, err
	}
	return &fetchWriter{WriteCloser: writer, bounds: s.bounds}, nil
}

// mirrorRefSpec 镜像的 refspec：上游有什么 ref，本地就有什么 ref。
//
// 和 PlainClone 的 Mirror 选项写下的那条是同一个，同步因此不会把镜像变成一份
// 只有默认分支的普通 clone。
const mirrorRefSpec = "+refs/*:refs/*"

func (g *gitBuilder) Sync(ctx context.Context, req *SyncRequest) (int64, error) {
	repo, err := git.PlainOpen(req.Dir)
	if err != nil {
		// 盘上没有那份镜像（被人删了、或者上一趟只写了一半）。这是一次失败，
		// 不在这里顺手重建：重建是 Build 的事，而调用方这一次要的是降级穿透。
		return 0, fmt.Errorf("打开镜像失败：%w", err)
	}
	// 每次现造一个匿名 remote，而不是用仓库里记着的那个 origin：站长在管理
	// 界面上改掉回源地址之后，下一次同步就该打到新的地方去，而仓库里那一份
	// 是建镜像当天的快照。它不落盘（NewRemote 不写 config）。
	remote := git.NewRemote(repo.Storer, &config.RemoteConfig{
		Name:  "katch-sync",
		URLs:  []string{req.Remote},
		Fetch: []config.RefSpec{mirrorRefSpec},
	})
	err = remote.FetchContext(ctx, &git.FetchOptions{
		RefSpecs: []config.RefSpec{mirrorRefSpec},
		// Force：上游 force push 之后，镜像要跟着走，而不是停在一条已经不
		// 存在的历史上并从此每次同步都报「非快进」。
		Force: true,
		// Prune：上游删掉的分支这边也要没有。镜像广播出去的是「上游此刻的
		// 样子」，留着一条上游已经不存在的分支，客户端会一直看到它。
		Prune: true,
		Tags:  git.NoTags,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		// 上游没有新东西是最常见的那一次同步，不是失败。
		return 0, fmt.Errorf("增量同步失败：%w", err)
	}
	return dirSize(req.Dir)
}

// discard 丢掉一份没建成的镜像。
//
// 半个仓库留在盘上比没有更糟：它既占着配额，又会让下一次重试撞上「目录非空」。
func (g *gitBuilder) discard(dir string) {
	_ = os.RemoveAll(dir)
}

// dirSize 量一份镜像在盘上占了多少字节。
//
// 走一遍目录树而不是记 clone 下来多少字节：落盘的是解包之后的对象和索引，
// 和线上传输的那份 pack 不是一回事，而配额管的是盘。
func dirSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("统计镜像体积失败：%w", err)
	}
	return total, nil
}

// remoteURL 拼出上游仓库的完整地址。
//
// 仓库路径保持转义形态原样拼上去（dispatch 给的就是那一个）：解一次再拼回去，
// 路径里的 %2F 之类会变成一个真的分隔符，指向另一个仓库。
func remoteURL(origin, repo string) string {
	if repo == "/" {
		// 仓库落在回源地址的基路径上：地址本身就是仓库。
		return strings.TrimSuffix(origin, "/")
	}
	return strings.TrimSuffix(origin, "/") + repo
}

// mirrorDir 一个仓库的镜像落在盘上的哪里。
//
// 形态是 <根目录>/<主机名>/<仓库路径>.git：目录名直接照着上游的路径走，出了事
// 人能在盘上按主机和仓库名找到它，而不是对着一堆哈希目录猜。
//
// 末尾那个 .git **无条件**缀上去，不去掉仓库路径本身已有的 .git：/foo/bar 和
// /foo/bar.git 在库里是两条记录（上游侧确实是两个路径），归一成同一个目录会让
// 两趟建镜像同时往一处写。多一层 .git.git 不好看，但它不会毁掉一份镜像。
//
// 每一段都要过 safeSegment：仓库路径来自 URL，虽然分发那一层已经挡掉了回溯段，
// 但「镜像目录不会跑到根目录外面」这件事必须由写目录的这一层自己保证——它是
// 最后一道，也是唯一一道知道根目录在哪的。
func mirrorDir(root, host, repo string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("没有配置镜像目录")
	}
	if err := safeSegment(host); err != nil {
		return "", fmt.Errorf("主机名不能作为目录名：%w", err)
	}
	segments := make([]string, 0, 4)
	for _, segment := range strings.Split(strings.Trim(repo, "/"), "/") {
		if segment == "" {
			continue
		}
		if err := safeSegment(segment); err != nil {
			return "", fmt.Errorf("仓库路径不能作为目录名：%w", err)
		}
		segments = append(segments, segment)
	}
	if len(segments) == 0 {
		// 仓库就落在回源地址的基路径上。它挂在主机目录的**旁边**而不是里面：
		// 主机目录里装的是这台主机下的各个仓库，让其中一个仓库同时是那个目录
		// 本身，两者会互相埋掉。
		return filepath.Join(root, host+".git"), nil
	}
	segments[len(segments)-1] += ".git"
	dir := filepath.Join(append([]string{root, host}, segments...)...)
	// 兜底再验一次：上面每一段都查过了，但「结果仍在根目录之下」这句话本身
	// 值得被直接断言一次，而不是从几条局部规则里推出来。
	if !strings.HasPrefix(dir, filepath.Clean(root)+string(filepath.Separator)) {
		return "", fmt.Errorf("镜像目录跑出了根目录：%s", dir)
	}
	return dir, nil
}

// safeSegment 一段路径能不能直接当目录名。
func safeSegment(segment string) error {
	switch segment {
	case "", ".", "..":
		return fmt.Errorf("非法的路径段 %q", segment)
	}
	if strings.ContainsAny(segment, "/\\\x00") {
		// 反斜杠与 NUL 在这里都不该出现：前者在别的系统上是分隔符，
		// 后者会把路径在系统调用那一层截断。
		return fmt.Errorf("路径段里有分隔符或空字节：%q", segment)
	}
	return nil
}
