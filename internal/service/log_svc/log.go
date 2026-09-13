// Package log_svc 从结构化日志里读出「最近请求」。
//
// 上游详情上的最近请求本来就是日志：每请求的时间、对象、结果、大小、耗时由拉取
// 路径上的计数中间件写成一行（internal/metrics），这里只是把那份文件的**尾部**
// 读回来。它不另建每请求的表——决策 16 已经否掉了每请求写库，那会把拉取热路径
// 拖进事务；分钟级 rollup 答的是「这段时间总共怎么样」，答不了「刚刚发生了什么」。
//
// 两条性质是这一层的全部难点：
//
//   - **有界**：只读文件末尾固定大小的一段，返回条数有硬上限。日志是会长到几个 G
//     的东西，把它读进内存等于给管理接口开一条内存放大路径。
//   - **读不到就当没有**：文件没开、被轮转走、目录权限变了，都返回空列表而不是
//     错误。这块面板是排障的辅助，它消失总好过让整屏管理界面挂在一句打不开文件上。
package log_svc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/cago-frame/cago/configs"
	"github.com/cago-frame/cago/pkg/logger"
	"go.uber.org/zap"

	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/metrics"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
)

// defaultTailBytes 从文件末尾往回读多少字节。
//
// 256 KiB 装得下几百条拉取行，而返回条数的上限是 100：窗口比上限宽出一截，
// 是因为窗口里混着别的上游和别的日志，不能按「一行一条」去估。
// 它同时是这一层的内存上限——文件有多大都只碰这么多。
const defaultTailBytes = 256 * 1024

// LogSvc 结构化日志的读取。
type LogSvc interface {
	// RecentRequests 某个上游最近的若干次拉取，最近的在最前。
	//
	// 日志读不到时返回空列表且不报错：调用方据此让那块面板消失。
	RecentRequests(ctx context.Context, req *admin.RecentRequestsRequest) (
		*admin.RecentRequestsResponse, error)
}

// Options 读取参数。
type Options struct {
	// Filename 日志文件路径。留空表示现读 configs 里的 logger.logFile.filename。
	//
	// 它不是一个可以由请求指定的东西：管理接口上没有对应的参数，这里留这个口子
	// 只为让用例指向自己造的日志文件。
	Filename string
	// TailBytes 尾部窗口大小，0 按 defaultTailBytes。
	TailBytes int64
}

type logSvc struct {
	opt Options
}

// New 构造日志读取层。
func New(opt Options) LogSvc {
	if opt.TailBytes <= 0 {
		opt.TailBytes = defaultTailBytes
	}
	return &logSvc{opt: opt}
}

var defaultLog = New(Options{})

// Log 返回日志读取层。
func Log() LogSvc {
	return defaultLog
}

// Register 注册实现。
func Register(svc LogSvc) {
	defaultLog = svc
}

// pullLine 拉取日志那一行里这一层认得的字段。
//
// 字段名是和 internal/metrics 约好的，两边一起改。msg 是唯一的判别依据：
// 同一个文件里还躺着回源失败、缓存淘汰这些别的行。
type pullLine struct {
	Message    string `json:"msg"`
	At         int64  `json:"at"`
	Upstream   string `json:"upstream"`
	Object     string `json:"object"`
	Result     string `json:"result"`
	Bytes      int64  `json:"bytes"`
	DurationMS int64  `json:"duration_ms"`
}

func (l *logSvc) RecentRequests(ctx context.Context, req *admin.RecentRequestsRequest) (
	*admin.RecentRequestsResponse, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = admin.RecentRequestsDefaultLimit
	}
	if limit > admin.RecentRequestsMaxLimit {
		// 接口层的 binding 已经挡过一次。这里再夹一道，是因为响应大小不能由
		// 调用方决定，而服务层也会被别的调用方直接用。
		limit = admin.RecentRequestsMaxLimit
	}
	// 请求给的是上游 id，主机名从上游表来：日志按主机名记，而「哪个 id 是哪台
	// 主机」只有表说了算，让调用方直接给主机名等于把过滤条件交出去。
	upstream, err := upstream_repo.Upstream().Find(ctx, req.UpstreamID)
	if err != nil {
		// 库出了问题是真的错误，走 storageAware 那条 503，和别的管理接口一致。
		return nil, err
	}
	empty := &admin.RecentRequestsResponse{List: []*admin.RecentRequest{}}
	if upstream == nil {
		return empty, nil
	}
	name := l.filename(ctx)
	if name == "" {
		return empty, nil
	}
	tail, ok := readTail(ctx, name, l.opt.TailBytes)
	if !ok {
		return empty, nil
	}
	return &admin.RecentRequestsResponse{List: pick(tail, upstream.Host, limit)}, nil
}

// filename 日志文件在哪。
//
// 现读配置而不是启动时注入：这个路径只在有人打开这块面板时才用得上，为它在
// 装配处多接一根线，等于让一个诊断用的读取影响启动顺序。
func (l *logSvc) filename(ctx context.Context) string {
	if l.opt.Filename != "" {
		return l.opt.Filename
	}
	cfg := configs.Default()
	if cfg == nil {
		return ""
	}
	loggerCfg := &logger.Config{}
	if err := cfg.Scan(ctx, "logger", loggerCfg); err != nil {
		logger.Ctx(ctx).Warn("读不出日志配置，最近请求这一块给空", zap.Error(err))
		return ""
	}
	if !loggerCfg.LogFile.Enable {
		// 没开落盘就没有可读的文件——日志全在 stdout 上，被容器运行时收走了。
		return ""
	}
	return loggerCfg.LogFile.Filename
}

// readTail 读文件末尾的一段，读不到时 ok 为 false。
//
// 只碰 window 个字节：日志会长到几个 G，size 是多少都不该影响这里的内存。
// 任何一步失败都不往上抛错误——文件可能刚被 lumberjack 轮转走，那不是一次
// 需要有人介入的故障，而是「这块面板这次没有内容」。
func readTail(ctx context.Context, name string, window int64) ([]byte, bool) {
	// 路径只来自配置文件：请求上没有任何字段能影响它，见 admin.RecentRequestsRequest。
	file, err := os.Open(name)
	if err != nil {
		logger.Ctx(ctx).Warn("打不开日志文件，最近请求这一块给空",
			zap.String("file", name), zap.Error(err))
		return nil, false
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	size := info.Size()
	offset := int64(0)
	if size > window {
		offset = size - window
	}
	buf := make([]byte, size-offset)
	n, err := file.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		logger.Ctx(ctx).Warn("读不了日志文件，最近请求这一块给空",
			zap.String("file", name), zap.Error(err))
		return nil, false
	}
	buf = buf[:n]
	if offset > 0 {
		// 窗口的左边界多半落在某一行中间，那半行不是一条记录。
		if cut := bytes.IndexByte(buf, '\n'); cut >= 0 {
			buf = buf[cut+1:]
		} else {
			buf = nil
		}
	}
	return buf, true
}

// pick 从尾部那段字节里挑出这个上游的拉取行，最近的在最前，最多 limit 条。
//
// 从后往前扫：要的就是最近的那几条，凑够就停，不必把窗口里的行全解一遍。
func pick(tail []byte, host string, limit int) []*admin.RecentRequest {
	list := make([]*admin.RecentRequest, 0, limit)
	for len(tail) > 0 && len(list) < limit {
		var line []byte
		if cut := bytes.LastIndexByte(tail, '\n'); cut >= 0 {
			line, tail = tail[cut+1:], tail[:cut]
		} else {
			line, tail = tail, nil
		}
		parsed := &pullLine{}
		// 认不出来的行直接跳过：同一个文件里有别的日志，也可能有被写了一半的行。
		if err := json.Unmarshal(line, parsed); err != nil {
			continue
		}
		if parsed.Message != metrics.PullLogMessage || parsed.Upstream != host {
			continue
		}
		list = append(list, &admin.RecentRequest{
			At:         parsed.At,
			Object:     parsed.Object,
			Result:     parsed.Result,
			Bytes:      parsed.Bytes,
			DurationMS: parsed.DurationMS,
		})
	}
	return list
}
