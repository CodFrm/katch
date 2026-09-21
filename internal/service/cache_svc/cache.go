// Package cache_svc 是缓存层：拉取路径在回源之前先问它一次。
//
// 它站在 proxy_svc 之前——命中就由磁盘服务，未命中才回源，并且一边向客户端流式
// 转发一边写入缓存（不得先下载完整对象再开始响应）。缓存坏掉、磁盘不可用、数据库
// 不可用时，它一律退回成纯透传：缓存是优化，它坏掉不该让拉取整体失败。
package cache_svc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cago-frame/cago/pkg/i18n"
	"github.com/cago-frame/cago/pkg/logger"
	"go.uber.org/zap"

	"github.com/CodFrm/katch/internal/cache"
	"github.com/CodFrm/katch/internal/metrics"
	"github.com/CodFrm/katch/internal/model/entity/cache_entity"
	"github.com/CodFrm/katch/internal/model/entity/event_entity"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/pkg/code"
	"github.com/CodFrm/katch/internal/proxy/destination"
	"github.com/CodFrm/katch/internal/proxy/dispatch"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
	"github.com/CodFrm/katch/internal/repository/cache_repo"
	"github.com/CodFrm/katch/internal/service/event_svc"
	"github.com/CodFrm/katch/internal/service/proxy_svc"
	"github.com/CodFrm/katch/internal/service/setting_svc"
	"github.com/CodFrm/katch/internal/service/upstream_svc"
)

// ErrCacheUnavailable 缓存目录不可用。只有显式写缓存的接口会返回它，
// 拉取路径遇到同样的情况是降级透传，而不是报错。
var ErrCacheUnavailable = errors.New("缓存不可用")

// ErrPutIncomplete 写入缓存至少要有上游、键和内容。
var ErrPutIncomplete = errors.New("缓存写入缺少上游、键或内容")

const (
	// cacheStatusHeader 让调用方（以及运维）看得见这次响应是不是缓存命中。
	cacheStatusHeader = "X-Katch-Cache"
	cacheStatusHit    = "HIT"
	cacheStatusMiss   = "MISS"
)

const (
	// evictBatch 一次取多少条淘汰候选。取太小会把一次超配额拆成很多轮查询，
	// 取太大则在缓存很满时一次拉回一大片记录。
	evictBatch = 64
	// copyBufferSize 回源转发的缓冲区。镜像层动辄几百 MB，缓冲太小会把一次拷贝
	// 变成几百万次系统调用。
	copyBufferSize    = 64 << 10
	maxTransformBytes = 16 << 20
)

// Options 缓存层的构造参数。
//
// 配额、回收水位、可变对象 TTL **不在这里**：它们是 setting 表里的运行时项，
// 每次用到时现读（决策 3/4——改完立刻生效，不必重启进程）。抄进构造参数里，
// 设置页上改的数就要等下次重启才算数，那正是这三项落库的理由被抵消掉的样子。
// 缓存目录同样不在这里：目录是启动时就要定下来的东西，由 main 从配置文件读出来
// 交给 cache.NewStore。
type Options struct {
	// Runtime 运行时设置的来源，nil 表示进程级的那一个（setting_svc）。
	// 用例注入一个假的，就能在不写库的情况下把配额压到几十字节。
	Runtime setting_svc.RuntimeSource
	// Profiles selects independently registered package adapters. nil uses the process registry.
	Profiles *packageprofile.Registry
	// RewriteConfig and DestinationResolver provide one rewrite generation and safe URL validation.
	RewriteConfig       proxy_svc.RewriteConfigSource
	DestinationResolver destination.DestinationResolver
	// Now is injectable for freshness and Age tests.
	Now func() time.Time
}

// PutRequest 直接写入一个缓存对象。
type PutRequest struct {
	UpstreamID  int64
	Key         string
	Content     io.Reader
	ContentType string
	// Immutable 内容寻址的对象长期缓存，只由 LRU 淘汰；否则按 TTL 过期。
	Immutable  bool
	TTLSeconds int64
}

// SearchRequest 管理界面的对象搜索条件。
type SearchRequest struct {
	UpstreamID int64
	Keyword    string
	Page       int
	Size       int
}

// SearchResponse 搜索结果。
//
// 带上归一化之后的 Page 与 Size：页号和页大小会被下面的 Search 兜底改写，
// 不回声一份的话，调用方按自己传的参数画分页器会画错。
type SearchResponse struct {
	List  []*cache_entity.CacheObject `json:"list"`
	Total int64                       `json:"total"`
	Page  int                         `json:"page"`
	Size  int                         `json:"size"`
}

// PurgeRequest 清缓存：给 ID 清一条，给 UpstreamID 清整个上游。
type PurgeRequest struct {
	ID         int64
	UpstreamID int64
}

// PurgeResponse 清掉了几条，以及因为被 pin 而跳过了几条。
type PurgeResponse struct {
	Removed int64 `json:"removed"`
	// Skipped 批量清除时被跳过的 pin 对象数。不报出来的话，界面只能说
	// 「清了 N 条」，看不出还有几条留在那里，看起来就像清缓存没生效。
	Skipped int64 `json:"skipped"`
}

// PinRequest 把一个对象钉住/放开。钉住的对象不参与淘汰。
type PinRequest struct {
	ID     int64
	Pinned bool
}

// CacheSvc 缓存层的业务操作。
type CacheSvc interface {
	// Get 取一个对象：命中由磁盘服务，未命中回源并边转发边写入缓存。
	// 与 proxy_svc.Fetch 的返回约定一致，上游的 4xx/5xx 是正常返回值。
	//
	// HEAD、条件与范围请求也走这里：新鲜完整副本可在本地回答 200/206/304/412/416。
	// 普通 registry manifest HEAD 在冷缓存时以同变体的 canonical GET 填充后返回；其余
	// 答不了的请求完整透传，绝不把 partial 或无实体响应写成完整对象。返回的响应体始终
	// 可以安全关闭——无实体时是 http.NoBody。
	Get(ctx context.Context, target *proxy_svc.Target) (io.ReadCloser, *proxy_svc.Meta, error)
	Put(ctx context.Context, req *PutRequest) error
	Search(ctx context.Context, req *SearchRequest) (*SearchResponse, error)
	Purge(ctx context.Context, req *PurgeRequest) (*PurgeResponse, error)
	Pin(ctx context.Context, req *PinRequest) error
	// Sweep 收走已经过期的可变对象，返回收走了几条，然后按当下的配额回收一次。
	//
	// 「可变对象由 TTL 自行过期」（缓存一节）在读路径上只做到了「过期的不再命中」；
	// 一个再也没人来取的过期对象，记录和盘上的字节会一直留着，还一直算进配额，
	// 而它又进不了 LRU 的候选（那条只挑不可变的）。没有这一趟，配额就会被一批
	// 死对象慢慢顶穿，表现成「缓存超配额但没有可淘汰的对象」那条日志。
	//
	// 顺带在这里再跑一次配额回收：原先只有「写进一个新对象」才会触发，于是站长
	// 在设置页把配额改小之后，要等到下一次回源才开始削——决策 3/4 说的是改完立刻生效。
	Sweep(ctx context.Context) (int64, error)
	// Quiesce 等到此刻还在跑的后台下载全部收尾为止，ctx 结束时带着 ctx 的错误返回。
	//
	// 回源下载是脱离客户端跑的（决策 9），所以「响应体被收完」不代表这个对象已经
	// 落进缓存：提交文件、写记录、按配额回收都排在那之后。要在拆掉这一层脚下的
	// 库与缓存目录之前确保不会再有人碰它们，就得有一个等得到的入口。
	//
	// 它不阻止新的下载开始，只等已经起来的那些：关停时的顺序是先停止收新请求
	// （mux 自己负责），再在这里等存量收尾。
	Quiesce(ctx context.Context) error
}

type cacheSvc struct {
	store *cache.Store
	// runtime 配额、回收水位与可变对象 TTL 的来源，每次用到时现读。
	runtime       setting_svc.RuntimeSource
	profiles      *packageprofile.Registry
	rewriteConfig proxy_svc.RewriteConfigSource
	resolver      destination.DestinationResolver
	now           func() time.Time
	// mu 只护 inflight 这张表。
	mu           sync.Mutex
	inflight     map[string]*flight
	transforming map[string]chan struct{}
	// evictMu 把淘汰串起来：几个下载同时收尾时各淘汰各的，会把缓存削过头。
	evictMu sync.Mutex
	// forgot 被我们自己收走的键，供下一次未命中归因，见 miss.go。
	forgot *forgotten
	// pending 还没跑完的后台下载，供 Quiesce 等。
	//
	// 下载脱离客户端跑（决策 9），于是「响应体被收完」和「这趟活干完了」是两件事：
	// 提交文件、写记录、回收配额都排在客户端拿到最后一个字节之后。没有这个计数，
	// 这批活就是不可观测也不可等待的——进程退出会把它们当场抛下，而任何一个
	// 重新装配全局的调用方都会在它们脚下换掉库、缓存目录与日志。
	pending sync.WaitGroup
}

// New 构造缓存层。store 为 nil 表示磁盘不可用——此时它是一个纯透传的实现，
// 拉取照常，只是不命中也不写入。
func New(store *cache.Store, opt Options) CacheSvc {
	if opt.Runtime == nil {
		opt.Runtime = setting_svc.Setting()
	}
	if opt.RewriteConfig == nil {
		opt.RewriteConfig = proxy_svc.NewRewriteConfigSource()
	}
	if opt.DestinationResolver == nil {
		opt.DestinationResolver = destination.New(destination.Options{})
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &cacheSvc{store: store, runtime: opt.Runtime, profiles: opt.Profiles,
		rewriteConfig: opt.RewriteConfig, resolver: opt.DestinationResolver, now: opt.Now,
		inflight: map[string]*flight{}, transforming: map[string]chan struct{}{},
		forgot: newForgotten()}
}

// limits 读一次运行时设置。
//
// 读不出来不让这次拉取失败：返回的快照在出错时是出厂值，缓存是优化，
// 它的参数读不到不该让拉取整体失败（失败与降级）。
func (c *cacheSvc) limits(ctx context.Context) *setting_svc.RuntimeSettings {
	rt, err := c.runtime.Runtime(ctx)
	if err != nil {
		logger.Ctx(ctx).Error("读取运行时设置失败，本次按默认值处理", zap.Error(err))
	}
	return rt
}

// defaultCache 在 main 装配之前就是纯透传：拉取路径不该依赖装配顺序。
var defaultCache = New(nil, Options{})

// Cache 返回缓存层。
func Cache() CacheSvc {
	return defaultCache
}

// Register 注册实现，由 main 装配。
func Register(svc CacheSvc) {
	defaultCache = svc
}

func (c *cacheSvc) Get(ctx context.Context, target *proxy_svc.Target) (io.ReadCloser, *proxy_svc.Meta, error) {
	if target.Git.IsGit() || (target.Method != http.MethodGet && target.Method != http.MethodHead) {
		return proxy_svc.Proxy().Fetch(ctx, target)
	}
	upstream, err := upstream_svc.Upstream().FindByHost(ctx, target.Host)
	if target.Kind == dispatch.KindSumDB {
		if err != nil || !sumDBServable(upstream) {
			return statusOnly(http.StatusServiceUnavailable)
		}
		// /supported 是合成应答，答案只取决于当前配置：不进对象缓存，撤销才能在
		// 下一次请求就看得见，也不必为它造一个带代数的键。
		if target.Path == "/supported" {
			return proxy_svc.Proxy().Fetch(ctx, target)
		}
	}
	if err != nil || upstream == nil {
		return proxy_svc.Proxy().Fetch(ctx, target)
	}
	if !proxy_svc.SupportsTarget(target, upstream) {
		return proxy_svc.Proxy().Fetch(ctx, target)
	}
	representation, profile := c.classify(upstream, target)
	if representation.Recognized() && representation.Transform {
		if !identityAccepted(target.Header) {
			return statusOnly(http.StatusNotAcceptable)
		}
		return c.getTransformed(ctx, target, upstream, profile, representation)
	}
	if !identityAccepted(target.Header) {
		return fetchOriginMiss(ctx, target)
	}
	if !c.usable() {
		return fetchOriginMiss(ctx, target)
	}
	key := cacheKeyForRepresentation(target, representation, 0)
	immutable, protocolDefined := registryRequestImmutability(target)
	if representation.Recognized() {
		immutable = representation.Class == packageprofile.ClassImmutable
	} else if !protocolDefined {
		immutable = cache.IsImmutable(upstream.ImmutablePatterns, key)
	}
	body, meta, m := c.serveFromDisk(ctx, target, upstream, key, immutable,
		!immutable, false)
	if m == nil {
		return body, meta, nil
	}
	variants := declaredVariants(target, representation)
	if promotableManifestHead(target) {
		body, meta, err = c.fetchAndCache(ctx, canonicalMetadataTarget(target), upstream, key, immutable, variants)
		if err != nil {
			return nil, nil, err
		}
		return drainPromotedHead(body, stampPassthroughMiss(meta, m))
	}
	if !writableRequest(target) {
		body, meta, err = fetchOriginMiss(ctx, target)
		if err != nil {
			return nil, nil, err
		}
		return body, m.stamp(meta), nil
	}
	body, meta, err = c.fetchAndCache(ctx, target, upstream, key, immutable, variants)
	if err != nil {
		return nil, nil, err
	}
	return body, m.stamp(meta), nil
}

// sumDBServable 复核 checksum database 这一条别名路由当前是否仍被授权。
//
// 放在读缓存之前：撤销 go profile 之后，这条路径不再被 profile 认领，会落回上游
// 自己的 immutable_patterns，暖对象就能绕开 proxy_svc 那侧的配置检查被长期重放。
// 判据只看上游记录本身——它就是 sumdb 桥那侧配置快照的同一份真相，不必在拉取路径
// 上再开一个来源。
func sumDBServable(upstream *upstream_entity.Upstream) bool {
	return upstream != nil && upstream.Enabled &&
		upstream.Protocols.Has(upstream_entity.ProtocolStatic) &&
		upstream_entity.NormalizePackageProfile(upstream.PackageProfile) == upstream_entity.PackageProfileGoProxy
}

func (c *cacheSvc) classify(upstream *upstream_entity.Upstream,
	target *proxy_svc.Target,
) (packageprofile.Representation, packageprofile.Profile) {
	name := upstream_entity.NormalizePackageProfile(upstream.PackageProfile)
	if target.Kind != dispatch.KindStatic && target.Kind != dispatch.KindSumDB ||
		name == upstream_entity.PackageProfileNone {
		return packageprofile.Representation{}, nil
	}
	var profile packageprofile.Profile
	var ok bool
	if c.profiles != nil {
		profile, ok = c.profiles.Lookup(name)
	} else {
		profile, ok = packageprofile.Lookup(name)
	}
	if !ok {
		return packageprofile.Representation{}, nil
	}
	representation := profile.Classify(packageprofile.Request{
		Host: target.Host, Path: target.Path, RawQuery: target.RawQuery, Header: target.Header.Clone(),
	})
	return representation, profile
}

func statusOnly(status int) (io.ReadCloser, *proxy_svc.Meta, error) {
	return http.NoBody, &proxy_svc.Meta{StatusCode: status, Header: make(http.Header)}, nil
}

// Quiesce 等存量的后台下载收尾，见接口上的说明。
func (c *cacheSvc) Quiesce(ctx context.Context) error {
	done := make(chan struct{})
	// WaitGroup.Wait 自己不认识 context，套一层才能让调用方按自己的时限抽身。
	// 这个协程在存量收尾之后一定会退出，不会留住任何东西。
	go func() {
		c.pending.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *cacheSvc) usable() bool {
	return c.store != nil && cache_repo.CacheObject() != nil
}

// fetchOriginMiss 是缓存层唯一的直接回源入口。响应头一到手就覆盖成当前这一跳的
// MISS/first，避免上游 katch 的归因在不可缓存、磁盘降级或 waiter 回退分支冒充本地
// 命中。调用方随后可以用实际的缓存状态把 first 细化成 TTL、淘汰或内容变更。
func fetchOriginMiss(ctx context.Context, target *proxy_svc.Target) (io.ReadCloser, *proxy_svc.Meta, error) {
	body, meta, err := proxy_svc.Proxy().Fetch(ctx, target)
	if err != nil {
		return nil, nil, err
	}
	return body, stampPassthroughMiss(meta, &miss{reason: metrics.MissFirst}), nil
}

// stampPassthroughMiss 给本跳直接回源的响应写入 MISS 与未命中原因。
// 只覆盖归因头：其余上游响应头仍按原透传规则交给客户端。
func stampPassthroughMiss(meta *proxy_svc.Meta, m *miss) *proxy_svc.Meta {
	if meta == nil {
		return nil
	}
	if meta.Header == nil {
		meta.Header = make(http.Header, 2)
	}
	meta.Header.Set(cacheStatusHeader, cacheStatusMiss)
	return m.stamp(meta)
}

// promotableManifestHead 判断一次 HEAD 是否应该以 canonical GET 填充缓存。
//
// 只认 registry 协议定义的 tag/digest manifest。blob 即使按摘要寻址也不能走这里：
// 对一个 layer 的 HEAD 悄悄改成 GET 会把本来只取元数据的请求放大成整层下载。
// 条件与范围头同样排除，它们要求上游按原请求求值，不能被无条件 GET 替代。
func promotableManifestHead(target *proxy_svc.Target) bool {
	if target.Method != http.MethodHead || !isManifestRequest(target.Path) {
		return false
	}
	_, recognized := registryRequestImmutability(target)
	if !recognized {
		return false
	}
	for _, name := range []string{
		"Range", "If-Range", "If-Match", "If-Unmodified-Since", "If-None-Match", "If-Modified-Since",
	} {
		if target.Header.Get(name) != "" {
			return false
		}
	}
	return true
}

// drainPromotedHead 等 canonical GET 完整读完并落盘，再把同一份元数据投影成 HEAD。
// 读取或关闭失败时不返回一个看似成功的 HEAD：pump 会放弃记录，调用方收到回源错误。
func drainPromotedHead(body io.ReadCloser, meta *proxy_svc.Meta) (io.ReadCloser, *proxy_svc.Meta, error) {
	_, readErr := io.Copy(io.Discard, body)
	closeErr := body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, nil, err
	}
	header := meta.Header.Clone()
	if meta.ContentLength >= 0 {
		header.Set("Content-Length", strconv.FormatInt(meta.ContentLength, 10))
	}
	return http.NoBody, &proxy_svc.Meta{
		StatusCode: meta.StatusCode, Header: header, ContentLength: meta.ContentLength,
	}, nil
}

// writableRequest 未命中时这次原请求能不能直接写缓存。
//
// 只有不带条件或范围的 GET 才直接留下一份完整对象：HEAD 没有响应体，条件请求拿到的
// 可能是 304/412，范围请求可能拿到 206。把这种结果当成对象存下来，下一次普通 GET
// 就会拿到无实体或半截内容。registry manifest HEAD 的 canonical GET 提升在调用本函数
// 之前单独处理；这里仍然不允许任何 HEAD 响应本身入库。
func writableRequest(target *proxy_svc.Target) bool {
	if target.Method != http.MethodGet {
		return false
	}
	for _, h := range []string{
		"Range", "If-Range", "If-Match", "If-Unmodified-Since", "If-None-Match", "If-Modified-Since",
	} {
		if target.Header.Get(h) != "" {
			return false
		}
	}
	return true
}

// cacheableResponse 哪些响应可以留下来。
func cacheableResponse(meta *proxy_svc.Meta, variants []string) bool {
	// 只缓存 200：4xx/5xx 原样透传但不缓存，否则上游的一次抖动会被固化下来；
	// 206 是半截内容；304 没有响应体。
	if meta.StatusCode != http.StatusOK {
		return false
	}
	if meta.Header.Get("Content-Range") != "" {
		return false
	}
	// 传输编码过的响应体不能存：命中时我们只按 Content-Type 回放，缺了
	// Content-Encoding，客户端会把一份 gzip 字节当成原文解析。
	if enc := meta.Header.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
		return false
	}
	cc := parseCacheControl(meta.Header.Get("Cache-Control"))
	if _, denied := cc["no-store"]; denied {
		return false
	}
	if _, denied := cc["private"]; denied {
		return false
	}
	return canonicalizeVary(meta.Header, variants)
}

func declaredVariants(target *proxy_svc.Target, representation packageprofile.Representation) []string {
	variants := append([]string(nil), representation.Variants...)
	if acceptVaries(target) {
		variants = append(variants, "Accept")
	}
	return variants
}

func canonicalTarget(target *proxy_svc.Target) *proxy_svc.Target {
	cloned := *target
	cloned.Header = target.Header.Clone()
	cloned.Header.Set("Accept-Encoding", "identity")
	return &cloned
}

func canonicalMetadataTarget(target *proxy_svc.Target) *proxy_svc.Target {
	cloned := canonicalTarget(target)
	cloned.Method = http.MethodGet
	for _, name := range []string{"Range", "If-Range", "If-Match", "If-Unmodified-Since", "If-None-Match", "If-Modified-Since"} {
		cloned.Header.Del(name)
	}
	return cloned
}

func identityAccepted(header http.Header) bool {
	lines := header.Values("Accept-Encoding")
	if len(lines) == 0 {
		return true
	}
	identity, wildcard := -1.0, -1.0
	for _, line := range lines {
		for _, item := range strings.Split(line, ",") {
			fields := strings.Split(strings.TrimSpace(item), ";")
			name := strings.ToLower(strings.TrimSpace(fields[0]))
			q := 1.0
			for _, field := range fields[1:] {
				key, value, ok := strings.Cut(strings.TrimSpace(field), "=")
				if ok && strings.EqualFold(key, "q") {
					parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
					if err == nil {
						q = parsed
					}
				}
			}
			switch name {
			case "identity":
				identity = q
			case "*":
				wildcard = q
			}
		}
	}
	if identity >= 0 {
		return identity > 0
	}
	return wildcard != 0
}

func canonicalizeVary(header http.Header, variants []string) bool {
	allowed := make(map[string]struct{}, len(variants))
	for _, variant := range variants {
		name := http.CanonicalHeaderKey(strings.TrimSpace(variant))
		if name != "" && !strings.EqualFold(name, "Accept-Encoding") {
			allowed[strings.ToLower(name)] = struct{}{}
		}
	}
	kept := make([]string, 0, len(allowed))
	seen := map[string]struct{}{}
	for _, line := range header.Values("Vary") {
		for _, item := range strings.Split(line, ",") {
			name := http.CanonicalHeaderKey(strings.TrimSpace(item))
			if name == "" || strings.EqualFold(name, "Accept-Encoding") {
				continue
			}
			if name == "*" {
				return false
			}
			lower := strings.ToLower(name)
			if _, ok := allowed[lower]; !ok {
				return false
			}
			if _, ok := seen[lower]; !ok {
				seen[lower] = struct{}{}
				kept = append(kept, name)
			}
		}
	}
	slices.Sort(kept)
	if len(kept) == 0 {
		header.Del("Vary")
	} else {
		header.Set("Vary", strings.Join(kept, ", "))
	}
	return true
}

func parseCacheControl(value string) map[string]string {
	out := make(map[string]string)
	for _, part := range strings.Split(value, ",") {
		name, argument, _ := strings.Cut(strings.TrimSpace(part), "=")
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "" {
			out[name] = strings.Trim(strings.TrimSpace(argument), `"`)
		}
	}
	return out
}

func (c *cacheSvc) getTransformed(ctx context.Context, target *proxy_svc.Target,
	upstream *upstream_entity.Upstream, profile packageprofile.Profile,
	representation packageprofile.Representation,
) (io.ReadCloser, *proxy_svc.Meta, error) {
	snapshot, err := c.rewriteConfig.Snapshot(ctx)
	if err != nil || snapshot == nil || snapshot.SiteBaseURL == "" {
		return statusOnly(http.StatusServiceUnavailable)
	}
	key := cacheKeyForRepresentation(target, representation, snapshot.Generation)
	immutable := representation.Class == packageprofile.ClassImmutable
	attribution := &miss{reason: metrics.MissFirst}
	if c.usable() {
		var firstMiss *miss
		for {
			body, meta, currentMiss := c.serveFromDisk(ctx, target, upstream, key, immutable,
				representation.Class == packageprofile.ClassMutable, true)
			if currentMiss == nil {
				return body, meta, nil
			}
			if firstMiss == nil {
				firstMiss = currentMiss
			}
			wait, leader := c.beginTransform(key)
			if leader {
				attribution = firstMiss
				defer c.finishTransform(key)
				break
			}
			select {
			case <-wait:
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}
	}

	fillCtx := context.WithoutCancel(ctx)
	body, meta, err := fetchOriginMiss(fillCtx, canonicalMetadataTarget(target))
	if err != nil {
		return nil, nil, err
	}
	if meta.StatusCode >= 400 {
		return body, stampPassthroughMiss(meta, attribution), nil
	}
	if meta.StatusCode != http.StatusOK {
		_ = body.Close()
		return statusOnly(http.StatusBadGateway)
	}
	if encoding := strings.TrimSpace(meta.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		_ = body.Close()
		return statusOnly(http.StatusBadGateway)
	}
	if meta.ContentLength > maxTransformBytes {
		_ = body.Close()
		return statusOnly(http.StatusBadGateway)
	}
	mediaType, _, mediaErr := mime.ParseMediaType(meta.Header.Get("Content-Type"))
	if mediaErr != nil || !acceptedMediaType(mediaType, representation.MediaTypes) {
		_ = body.Close()
		return statusOnly(http.StatusBadGateway)
	}
	limited := io.LimitReader(body, maxTransformBytes+1)
	originBody, readErr := io.ReadAll(limited)
	closeErr := body.Close()
	if readErr != nil || closeErr != nil || len(originBody) > maxTransformBytes {
		return statusOnly(http.StatusBadGateway)
	}
	if meta.SourceURL == nil {
		return statusOnly(http.StatusBadGateway)
	}
	source := *meta.SourceURL
	result, err := profile.Transform(fillCtx, packageprofile.TransformRequest{
		Body: originBody, ContentType: mediaType, Source: &source, SiteBaseURL: snapshot.SiteBaseURL,
		RewriteURL: c.urlRewriter(snapshot),
	})
	if err != nil || result == nil || len(result.Body) > maxTransformBytes {
		if errors.Is(err, packageprofile.ErrUnavailable) {
			return statusOnly(http.StatusServiceUnavailable)
		}
		return statusOnly(http.StatusBadGateway)
	}

	header := safeResponseHeaders(meta.Header)
	header.Del("Content-Encoding")
	header.Del("Last-Modified")
	header.Del("Etag")
	header.Del("Docker-Content-Digest")
	cacheable := canonicalizeVary(header, representation.Variants)
	sum := sha256.Sum256(result.Body)
	header.Set("Etag", `"sha256:`+hex.EncodeToString(sum[:])+`"`)
	header.Set("Content-Length", strconv.Itoa(len(result.Body)))
	if result.ContentType != "" {
		header.Set("Content-Type", result.ContentType)
	}
	outMeta := &proxy_svc.Meta{StatusCode: http.StatusOK, Header: header, ContentLength: int64(len(result.Body))}
	if cacheable && cacheableResponse(outMeta, representation.Variants) && c.usable() {
		if err := c.storeBuffered(fillCtx, upstream, key, result.Body, outMeta, immutable); err != nil {
			logger.Ctx(ctx).Warn("写转换后缓存失败", zap.String("key", key), zap.Error(err))
		}
	}
	body, responseMeta, err := transformedResponse(target, result.Body, outMeta, cacheStatusMiss)
	if err != nil {
		return nil, nil, err
	}
	return body, attribution.stamp(responseMeta), nil
}

func (c *cacheSvc) beginTransform(key string) (<-chan struct{}, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if running, ok := c.transforming[key]; ok {
		return running, false
	}
	done := make(chan struct{})
	c.transforming[key] = done
	return done, true
}

func (c *cacheSvc) finishTransform(key string) {
	c.mu.Lock()
	done := c.transforming[key]
	delete(c.transforming, key)
	close(done)
	c.mu.Unlock()
}

func acceptedMediaType(got string, accepted []string) bool {
	if len(accepted) == 0 {
		return got != ""
	}
	for _, candidate := range accepted {
		if strings.EqualFold(got, candidate) {
			return true
		}
	}
	return false
}

func transformedResponse(target *proxy_svc.Target, payload []byte, meta *proxy_svc.Meta,
	cacheStatus string,
) (io.ReadCloser, *proxy_svc.Meta, error) {
	header := meta.Header.Clone()
	header.Set(cacheStatusHeader, cacheStatus)
	conditions := target.Header.Clone()
	conditions.Del("If-Modified-Since")
	conditions.Del("If-Unmodified-Since")
	outcome := evaluateConditional(conditions, header.Get("Etag"), "")
	reader := bytes.NewReader(payload)
	body, out := representationResponse(target, io.NopCloser(reader), reader, header, int64(len(payload)), outcome)
	return body, out, nil
}

func (c *cacheSvc) urlRewriter(snapshot *proxy_svc.RewriteSnapshot) packageprofile.RewriteURL {
	// 一份元数据里的构件 URL 几乎都指向同一个伙伴主机，一次链接解析一次就把一趟 DNS
	// 查询变成成千上万趟（PyPI 的 numpy 索引有 4232 个链接，够让一个索引页几十分钟才
	// 回得来）。所以在这一份正文的范围内按 scheme/主机记住结论。
	//
	// 键里刻意不含端口：改写出来的地址本来就不带端口（下面拼的是 SiteBaseURL + "/" +
	// 主机 + 路径），客户端回头来取时用的是这个上游注册的 origin 上的端口，元数据里
	// 写的那个从头到尾不参与任何判断。把它放进键里只会让一份写了几万个不同端口的正文
	// 把省下来的 DNS 查询原样要回去。
	//
	// 刻意不跨请求：客户端真去拉那个 URL 时回源会重新解析、重新按地址策略校验，
	// 那才是 DNS 重绑定防护生效的地方，把结论留到请求之外等于把它放掉。
	requirement := destination.DestinationRequirement{AddressPolicy: destination.PublicAddressesOnly}
	var mu sync.Mutex
	resolved := map[string]error{}
	resolveOnce := func(ctx context.Context, target *url.URL, host string) error {
		// 带用户信息的 URL 一律现算：它在解析器里是无条件拒绝，不该由同主机的
		// 别的 URL 的结论代表，也不该把自己的拒绝结论传染给它们。
		if target.User != nil {
			_, err := c.resolver.Resolve(ctx, target, requirement)
			return err
		}
		key := target.Scheme + "|" + host
		mu.Lock()
		err, seen := resolved[key]
		mu.Unlock()
		if seen {
			return err
		}
		_, err = c.resolver.Resolve(ctx, target, requirement)
		mu.Lock()
		resolved[key] = err
		mu.Unlock()
		return err
	}
	return func(ctx context.Context, target *url.URL, companion packageprofile.Companion) (*url.URL, error) {
		host := strings.ToLower(strings.TrimSuffix(target.Hostname(), "."))
		if companion.Host != "" && host != strings.ToLower(strings.TrimSuffix(companion.Host, ".")) {
			return nil, packageprofile.ErrInvalidMetadata
		}
		configured, ok := snapshot.Upstreams[host]
		if !ok {
			logger.Ctx(ctx).Warn("metadata rewrite companion unavailable",
				zap.String("host", host), zap.String("reason", "not_configured_or_disabled"))
			return nil, packageprofile.ErrUnavailable
		}
		if companion.Transport != "" && !configured.Transports.Has(companion.Transport) {
			logger.Ctx(ctx).Warn("metadata rewrite companion unavailable",
				zap.String("host", host), zap.String("reason", "transport_incompatible"))
			return nil, packageprofile.ErrUnavailable
		}
		if companion.Profile != "" &&
			upstream_entity.NormalizePackageProfile(configured.Profile) != companion.Profile {
			logger.Ctx(ctx).Warn("metadata rewrite companion unavailable",
				zap.String("host", host), zap.String("reason", "profile_incompatible"))
			return nil, packageprofile.ErrUnavailable
		}
		// 端口不进记忆化的键，可它合不合法是解析器会判的一项：不单独判的话，一条
		// https://h:0/ 排在同主机的干净链接后面就会命中「已放行」，排在前面则被拒，
		// 同一份元数据的结论取决于链接的先后。
		if _, err := destination.TargetPort(target); err != nil {
			return nil, packageprofile.ErrInvalidMetadata
		}
		if err := resolveOnce(ctx, target, host); err != nil {
			return nil, packageprofile.ErrInvalidMetadata
		}
		mapped, err := url.Parse(strings.TrimSuffix(snapshot.SiteBaseURL, "/") + "/" + host + target.EscapedPath())
		if err != nil {
			return nil, packageprofile.ErrUnavailable
		}
		mapped.RawQuery = target.RawQuery
		mapped.Fragment = target.Fragment
		return mapped, nil
	}
}

func (c *cacheSvc) storeBuffered(ctx context.Context, upstream *upstream_entity.Upstream,
	key string, payload []byte, meta *proxy_svc.Meta, immutable bool,
) error {
	writer, err := c.store.Create()
	if err != nil {
		return err
	}
	defer func() { _ = writer.Close() }()
	if _, err := writer.Write(payload); err != nil {
		return err
	}
	digest, size, err := writer.Commit()
	if err != nil {
		return err
	}
	return c.saveRecord(ctx, responseRecordInput(upstream.ID, key, digest, size, meta,
		immutable, int64(upstream.MutableTTLSeconds), c.now()))
}

// cacheKey 缓存键：上游内路径加查询串，末尾按需缀上这次请求的变体。
//
// 带上查询串是因为它会改变返回的内容；上游 ID 不进键里，它是表上的另一列。
//
// 变体那一段管的是「请求头也会改变返回的内容」这件事：registry 靠 Accept 决定
// 给哪个版本的 manifest，只按路径存的话，先到的 docker 客户端会把自己那份留下，
// 后到的 OCI 客户端拿到一次命中——收到的是别人那一份，而且它不会报错。哪些请求
// 算有变体、怎么归一，见 variant.go；没有变体可言的请求落的还是**逐字不变**的
// 老键，库里改动之前写下的行因此照旧命中得到。
func cacheKey(target *proxy_svc.Target) string {
	key := target.Path
	if target.RawQuery != "" {
		key += "?" + target.RawQuery
	}
	if variant := cacheVariant(target); variant != "" {
		key += variantMarker + variant
	}
	return key
}

// serveFromDisk 命中则由磁盘服务。
//
// 第三个返回值为 nil 表示这次是命中；否则它带着这次未命中的归因——判断只能在
// 这里做，往上一层就只剩「查不到记录」这一个事实了。
func (c *cacheSvc) serveFromDisk(ctx context.Context, target *proxy_svc.Target,
	upstream *upstream_entity.Upstream, key string, immutable, currentlyMutable, transformed bool,
) (io.ReadCloser, *proxy_svc.Meta, *miss) {
	repo := cache_repo.CacheObject()
	object, err := repo.FindByKey(ctx, upstream.ID, key)
	if err != nil {
		// 库不可用不该让拉取停摆：当成未命中，回源照常。库都读不到的时候，
		// 「这个对象以前有没有缓存过」同样无从谈起，归因只能退回首次拉取。
		logger.Ctx(ctx).Error("读缓存记录失败", zap.String("key", key), zap.Error(err))
		return nil, nil, &miss{reason: metrics.MissFirst}
	}
	if object == nil {
		// 表里什么都没有：可能从没缓存过，也可能是被我们自己收走的。
		return nil, nil, &miss{reason: c.forgot.recall(upstream.ID, key)}
	}
	if currentlyMutable && object.Immutable {
		// 旧配置可能用宽泛模式把 tag manifest 或 referrers 写成了永久对象。
		// 不继续发这份没有新鲜度边界的旧快照；回源成功后 saveRecord 会按 TTL
		// 覆盖元数据，失败则保留旧记录供下次重试。
		return nil, nil, &miss{reason: metrics.MissTTL, heldDigest: object.Digest}
	}
	if immutable && !object.Immutable {
		// 旧版本只看 immutable_patterns，把 registry 的 digest 对象写成了带 TTL
		// 的可变记录。先提升再判过期：即使旧 TTL 已到但 Sweep 还没来，这份由
		// digest 寻址且校验完整的副本仍然有效，不必白白回源一次。
		object.Immutable = true
		object.ExpiresAt = 0
		if err := repo.PromoteImmutable(ctx, object.ID); err != nil {
			// 当前请求仍可按协议语义使用这份副本；下一次命中会继续尝试修正元数据。
			logger.Ctx(ctx).Warn("提升缓存记录为不可变对象失败",
				zap.Int64("id", object.ID), zap.String("key", key), zap.Error(err))
		}
	}
	if object.RequiresRevalidation || object.Expired(c.now().Unix()) {
		// 记录还在，只是过期了——可变对象由 TTL 自行过期（缓存一节）。
		// 带上手上这份的摘要：回源的响应头会说清上游那份是不是同一个。
		return nil, nil, &miss{reason: metrics.MissTTL, heldDigest: object.Digest}
	}
	file, size, err := c.store.Open(object.Digest)
	if err != nil {
		// 记录还在、文件没了：这是磁盘或写入路径出了问题，不能静默自愈了事。
		logger.Ctx(ctx).Error("缓存副本丢失", zap.String("key", key),
			zap.String("digest", object.Digest), zap.Error(err))
		c.dropRecord(ctx, object)
		return nil, nil, brokenCopyMiss()
	}
	if size != object.Size {
		_ = file.Close()
		logger.Ctx(ctx).Error("缓存副本大小与记录不符", zap.String("key", key),
			zap.Int64("want", object.Size), zap.Int64("got", size))
		metrics.RecordIntegrityFailure(upstream.Host)
		c.dropRecord(ctx, object)
		return nil, nil, brokenCopyMiss()
	}
	// 校验在发字节之前做完：失败时这一次请求还回得了源（缓存一节）。
	if err := verifyCopy(file, object); err != nil {
		_ = file.Close()
		// 记 error 而不是 warn：大小一样、内容却变了，说明磁盘或写入路径出了
		// 问题，不能静默自愈了事。
		logger.Ctx(ctx).Error("缓存副本校验失败", zap.String("key", key),
			zap.String("digest", object.Digest), zap.Error(err))
		metrics.RecordIntegrityFailure(upstream.Host)
		c.dropRecord(ctx, object)
		return nil, nil, brokenCopyMiss()
	}
	header := replayStoredHeaders(object, c.now())
	if object.ContentType != "" {
		header.Set("Content-Type", object.ContentType)
	}
	header.Set("Content-Length", strconv.FormatInt(object.Size, 10))
	header.Set(cacheStatusHeader, cacheStatusHit)
	// 上游的 validator 原样回放：未命中时客户端拿到的就是这两串，命中时若缺席，
	// 同一个 URL 的响应头就随「这次有没有命中」而变，靠它做条件请求的客户端会
	// 退回整份重传。空值不回放——那是「上游没给」，不是「上游给了空」。（决策 3/5）
	if object.ETag != "" {
		header.Set("Etag", object.ETag)
	}
	if object.LastModified != "" {
		header.Set("Last-Modified", object.LastModified)
	}
	// registry 的摘要头补在这里：未命中时它们来自上游，命中时若没有人补，同一个
	// URL 的响应头就随「这次有没有命中」而变，而摘要是 registry 协议里客户端可以
	// 依赖的字段。
	//
	// Docker-Content-Digest 的值是推导出来的，不是从上游那份拷贝存下来的：上面几行
	// 刚刚校验过盘上的字节与 Digest 相符，推导出来的头因此不可能和发出去的字节对
	// 不上。Etag 则是上游给了就用上游那一串（上面已经原样回放），只有它没给时才退回
	// 这串推导值——否则未命中发上游、命中换推导，同一份内容就有了两个互不相认的强
	// 校验符，客户端拿着命中的那个去回源做条件请求，只会换回一次整份重传。
	if upstream.Protocols.Has(upstream_entity.ProtocolRegistry) && object.Digest != "" {
		if object.DockerContentDigest == "" {
			header.Set("Docker-Content-Digest", object.Digest)
		}
		if object.ETag == "" {
			header.Set("Etag", `"`+object.Digest+`"`)
		}
	}
	// 条件求值要用**发得出去的那一套头**，而不是记录里的原始字段：registry 的
	// ETag 是可以由摘要推导的（上面刚补过），拿它比较才和客户端手里的一致。
	//
	// 求值排在 Touch 之前：只有真的答出一份副本（200/304/HEAD）才算一次命中，
	// 缺 validator 而透传的那一次不该被记成命中。
	conditionHeader := target.Header
	if transformed {
		conditionHeader = target.Header.Clone()
		conditionHeader.Del("If-Modified-Since")
		conditionHeader.Del("If-Unmodified-Since")
	}
	outcome := evaluateConditional(conditionHeader, header.Get("Etag"), header.Get("Last-Modified"))
	if outcome == conditionUnresolved {
		// 有效条件存在，副本却没有对应 validator：不猜，把判断交回上游。
		_ = file.Close()
		return nil, nil, &miss{reason: metrics.MissFirst}
	}
	if err := repo.Touch(ctx, object.ID, c.now().Unix()); err != nil {
		// 命中已经成立了，访问时间没更新上只影响淘汰顺序，不该让这次拉取失败。
		logger.Ctx(ctx).Warn("更新缓存访问时间失败", zap.Int64("id", object.ID), zap.Error(err))
	}
	body, meta := representationResponse(target, file, file, header, object.Size, outcome)
	return body, meta, nil
}

// notModifiedMeta 一次本地 304 的元数据。
//
// 复用 200 那一套头（validator、摘要、命中归因都在里面），但把实体相关的两项摘掉：
// 304 不带响应体，也就不该声明它有多长、是什么类型（决策 3）。
func notModifiedMeta(header http.Header) *proxy_svc.Meta {
	h := header.Clone()
	h.Del("Content-Length")
	h.Del("Content-Type")
	return &proxy_svc.Meta{StatusCode: http.StatusNotModified, Header: h}
}

// brokenCopyMiss 副本已经坏了、被丢掉了，这次只能回源。
//
// 算首次拉取而不是「内容变更」：变的是我们自己的盘，不是上游。记成内容变更会
// 把站长支去查上游，而真正该看的是那条校验失败的 error 与
// katch_cache_integrity_failures_total。不带摘要——手上那份既然已经不可信，
// 拿它去和上游比也没有意义。
func brokenCopyMiss() *miss {
	return &miss{reason: metrics.MissFirst}
}

// dropRecord 丢掉一条坏记录，连同它独占的那份内容。
func (c *cacheSvc) dropRecord(ctx context.Context, object *cache_entity.CacheObject) {
	if err := cache_repo.CacheObject().Delete(ctx, object.ID); err != nil {
		logger.Ctx(ctx).Error("删除缓存记录失败", zap.Int64("id", object.ID), zap.Error(err))
		return
	}
	c.removeIfUnreferenced(ctx, object.Digest)
}

// removeIfUnreferenced 没有别的记录引用这份内容时才删文件。
//
// 内容寻址意味着一份字节可能被多条路径共用，不问一声就删，会把别人的缓存
// 变成一条指向空文件的坏记录。
func (c *cacheSvc) removeIfUnreferenced(ctx context.Context, digest string) {
	if digest == "" {
		return
	}
	count, err := cache_repo.CacheObject().CountByDigest(ctx, digest)
	if err != nil {
		logger.Ctx(ctx).Error("统计内容引用失败", zap.String("digest", digest), zap.Error(err))
		return
	}
	if count > 0 {
		return
	}
	if err := c.store.Remove(digest); err != nil {
		logger.Ctx(ctx).Error("删除缓存文件失败", zap.String("digest", digest), zap.Error(err))
	}
}

// fetchAndCache 未命中：回源，同一对象的并发请求合并成一次（决策 9）。
func (c *cacheSvc) fetchAndCache(ctx context.Context, target *proxy_svc.Target,
	upstream *upstream_entity.Upstream, key string, immutable bool,
	variants []string,
) (io.ReadCloser, *proxy_svc.Meta, error) {
	flightKey := objectKey(upstream.ID, key)

	c.mu.Lock()
	if running, ok := c.inflight[flightKey]; ok {
		c.mu.Unlock()
		return c.attachOrFetch(ctx, running, target)
	}
	current := newFlight(c.store)
	c.inflight[flightKey] = current
	c.mu.Unlock()

	// 回源用脱离客户端取消的 context：客户端断开时下载要继续跑完，已下载的部分
	// 仍要写完缓存——下一个请求就能命中，否则一次断线就白白浪费整趟回源。
	fetchCtx := context.WithoutCancel(ctx)
	fetchTarget := canonicalTarget(target)
	body, meta, err := fetchOriginMiss(fetchCtx, fetchTarget)
	if err != nil {
		c.forget(flightKey)
		current.startFailed(err)
		return nil, nil, err
	}
	if !cacheableResponse(meta, variants) {
		c.forget(flightKey)
		current.startUncacheable()
		return body, meta, nil
	}
	writer, err := c.store.Create()
	if err != nil {
		// 盘写不了就降级为纯透传：这次拉取照常完成，只是不留缓存。
		logger.Ctx(ctx).Error("缓存写入不可用，本次降级为纯透传",
			zap.String("key", key), zap.Error(err))
		c.forget(flightKey)
		current.startUncacheable()
		return body, meta, nil
	}
	current.start(meta, writer.Name())
	// 计数要在起协程**之前**加：加在 pump 里面的话，Quiesce 可能刚好在协程还没
	// 被调度到的时候看到一个空计数，于是「等干完」等了个寂寞。
	c.pending.Add(1)
	go c.pump(fetchCtx, flightKey, current, body, writer, upstream, key, meta, immutable)
	return c.attachOrFetch(ctx, current, target)
}

// attachOrFetch 搭上一次正在进行的下载；搭不上就自己回源。
func (c *cacheSvc) attachOrFetch(ctx context.Context, current *flight,
	target *proxy_svc.Target) (io.ReadCloser, *proxy_svc.Meta, error) {
	body, meta, err := current.attach(ctx)
	if err == nil {
		return body, meta, nil
	}
	if errors.Is(err, errNotCoalescable) {
		return fetchOriginMiss(ctx, target)
	}
	return nil, nil, err
}

func (c *cacheSvc) forget(flightKey string) {
	c.mu.Lock()
	delete(c.inflight, flightKey)
	c.mu.Unlock()
}

// pump 把回源的响应体搬进临时文件，读者跟在后面读。
//
// 它不由任何一个客户端驱动：谁断开都不影响这趟下载跑完（失败与降级一节），
// 而写进去的字节一出现就能被读者看见（首字节不必等整份下载完成）。
func (c *cacheSvc) pump(ctx context.Context, flightKey string, current *flight,
	body io.ReadCloser, writer *cache.Writer, upstream *upstream_entity.Upstream,
	key string, meta *proxy_svc.Meta, immutable bool) {
	defer func() {
		_ = body.Close()
		_ = writer.Close()
		// 放在最后：这趟活到这里才算真的干完，Quiesce 等的就是这一刻。
		c.pending.Done()
	}()

	failure := c.copyToCache(current, body, writer)
	digest := ""
	if failure == nil {
		var size int64
		var err error
		// 提交与落库都在 finish 之前完成：读者读到 EOF 时，这个对象已经在缓存里，
		// 紧接着的下一次拉取才不会看见一个写了一半的缓存。
		digest, size, err = writer.Commit()
		if err != nil {
			logger.Ctx(ctx).Error("提交缓存文件失败", zap.String("key", key), zap.Error(err))
			digest = ""
		} else if err := c.saveRecord(ctx, responseRecordInput(upstream.ID, key, digest, size,
			meta, immutable, int64(upstream.MutableTTLSeconds), c.now())); err != nil {
			logger.Ctx(ctx).Error("写缓存记录失败", zap.String("key", key), zap.Error(err))
		}
	} else {
		// 回源中断或盘写不下去：这次不留缓存，下一次重新来过。
		logger.Ctx(ctx).Error("缓存写入中断", zap.String("key", key), zap.Error(failure))
	}
	current.finish(digest, failure)
	c.forget(flightKey)
	if failure == nil && digest != "" {
		c.enforceQuota(ctx)
	}
}

// copyToCache 搬字节，每写进去一段就让读者可见。
func (c *cacheSvc) copyToCache(current *flight, body io.Reader, writer *cache.Writer) error {
	buf := make([]byte, copyBufferSize)
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			if _, err := writer.Write(buf[:n]); err != nil {
				return err
			}
			current.publish(int64(n))
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

// recordInput 一条缓存记录要写进去的内容。
type recordInput struct {
	UpstreamID  int64
	Key         string
	Digest      string
	Size        int64
	ContentType string
	// ETag 与 LastModified 是上游成功响应里的 validator，随内容一起原子落库。
	//
	// 上游没给时写空：saveRecord 无条件赋值，所以重写内容时上一份的校验值会被
	// 一起覆盖掉，不会留着给下一次命中回放。
	ETag         string
	LastModified string
	Immutable    bool
	TTLSeconds   int64
	Header       http.Header
	StoredAt     time.Time
}

func responseRecordInput(upstreamID int64, key, digest string, size int64,
	meta *proxy_svc.Meta, immutable bool, ttl int64, storedAt time.Time,
) *recordInput {
	header := safeResponseHeaders(meta.Header)
	return &recordInput{
		UpstreamID: upstreamID, Key: key, Digest: digest, Size: size,
		ContentType: header.Get("Content-Type"), ETag: header.Get("Etag"),
		LastModified: header.Get("Last-Modified"), Immutable: immutable,
		TTLSeconds: ttl, Header: header, StoredAt: storedAt,
	}
}

var persistedResponseHeaders = []string{
	"Content-Type", "Etag", "Last-Modified", "Cache-Control", "Date", "Age", "Expires",
	"Vary", "Accept-Ranges", "Content-Disposition", "Docker-Content-Digest",
	"X-Checksum-MD5", "X-Checksum-SHA1", "X-Checksum-SHA256", "X-Checksum-SHA512",
}

func safeResponseHeaders(source http.Header) http.Header {
	out := make(http.Header, len(persistedResponseHeaders))
	for _, name := range persistedResponseHeaders {
		for _, value := range source.Values(name) {
			if safe := safeHeaderValue(value); safe != "" {
				out.Add(name, safe)
			}
		}
	}
	return out
}

func safeHeaderValue(value string) string {
	if strings.ContainsAny(value, "\r\n") {
		return ""
	}
	return strings.TrimSpace(value)
}

func correctedInitialAge(header http.Header, now time.Time) int64 {
	age := parseNonnegativeSeconds(header.Get("Age"))
	if date, err := http.ParseTime(header.Get("Date")); err == nil && now.After(date) {
		apparent := durationSeconds(now.Sub(date))
		if apparent > age {
			age = apparent
		}
	}
	return age
}

func parseNonnegativeSeconds(value string) int64 {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "-") {
		return 0
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil || parsed > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(parsed)
}

func durationSeconds(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	return int64(duration / time.Second)
}

// effectiveExpiration 本地新鲜期的截止时刻；返回 0 表示不因时间过期。
//
// 两道上限分开算：源站那道是它声明的寿命减去对象到手时已有的年龄（Age 或 Date 推算，
// 取大者），katch 这道是可变 TTL——封的是对象在这里的驻留时间。以前是先取两者较小的
// 寿命、再统一扣源站年龄，于是 CDN 报的 Age 一旦超过 TTL，对象刚存进来就已经过期：
// index.crates.io/config.json 没有寿命指令却带着 Age: 1420，sum.golang.org 的部分
// tile 是 max-age=10800、Age 几千秒，结果每一次都回源。
//
// spec 对本地新鲜度的约束都还满足：不超过 TTL，不超过源站剩下的新鲜期，命中也不会
// 重启源站寿命（对外的 Age 仍是源站年龄加本地驻留时间，见读路径）。
func effectiveExpiration(now time.Time, ttl int64, immutable bool,
	object *cache_entity.CacheObject,
) int64 {
	lifetime := int64(math.MaxInt64)
	directives := parseCacheControl(object.CacheControl)
	for _, name := range []string{"s-maxage", "max-age"} {
		if value, ok := directives[name]; ok {
			seconds := parseNonnegativeSeconds(value)
			if seconds < lifetime {
				lifetime = seconds
			}
			break
		}
	}
	if expires, err := http.ParseTime(object.OriginExpires); err == nil {
		base := now
		if date, dateErr := http.ParseTime(object.OriginDate); dateErr == nil {
			base = date
		}
		seconds := durationSeconds(expires.Sub(base))
		if seconds < lifetime {
			lifetime = seconds
		}
	}
	remaining := int64(math.MaxInt64)
	if lifetime != math.MaxInt64 {
		remaining = lifetime - min(lifetime, object.OriginAge)
	}
	if !immutable && ttl > 0 && ttl < remaining {
		remaining = ttl
	}
	if remaining == math.MaxInt64 {
		return 0
	}
	return saturatingAdd(now.Unix(), remaining)
}

func saturatingAdd(left, right int64) int64 {
	if right > 0 && left > math.MaxInt64-right {
		return math.MaxInt64
	}
	if right < 0 && left < math.MinInt64-right {
		return math.MinInt64
	}
	return left + right
}

func replayStoredHeaders(object *cache_entity.CacheObject, now time.Time) http.Header {
	header := make(http.Header, 12)
	for name, value := range map[string]string{
		"Cache-Control": object.CacheControl, "Date": object.OriginDate,
		"Expires": object.OriginExpires, "Vary": object.Vary,
		"Accept-Ranges": object.AcceptRanges, "Content-Disposition": object.ContentDisposition,
		"Docker-Content-Digest": object.DockerContentDigest,
		"X-Checksum-MD5":        object.XChecksumMD5, "X-Checksum-SHA1": object.XChecksumSHA1,
		"X-Checksum-SHA256": object.XChecksumSHA256, "X-Checksum-SHA512": object.XChecksumSHA512,
	} {
		if safe := safeHeaderValue(value); safe != "" {
			header.Set(name, safe)
		}
	}
	if object.StoredAt > 0 {
		residence := now.Unix() - object.StoredAt
		if residence < 0 {
			residence = 0
		}
		header.Set("Age", strconv.FormatInt(saturatingAdd(object.OriginAge, residence), 10))
	}
	return header
}

// saveRecord 写入或更新一条缓存记录。
//
// 同一条路径只会有一条记录：重取（可变对象过期、或坏副本重下）要覆盖原来那条，
// 否则表里会为同一个 key 越堆越多，而唯一索引会在第二次写入时直接报错。
func (c *cacheSvc) saveRecord(ctx context.Context, in *recordInput) error {
	repo := cache_repo.CacheObject()
	nowTime := in.StoredAt
	if nowTime.IsZero() {
		nowTime = c.now()
	}
	now := nowTime.Unix()
	object, err := repo.FindByKey(ctx, in.UpstreamID, in.Key)
	if err != nil {
		return err
	}
	previousDigest := ""
	if object == nil {
		object = &cache_entity.CacheObject{UpstreamID: in.UpstreamID, Key: in.Key, Createtime: now}
	} else {
		previousDigest = object.Digest
	}
	object.Digest = in.Digest
	object.Size = in.Size
	object.ContentType = in.ContentType
	// 无条件覆盖，不给上一份内容留旧 validator：上游重写同一路径却不再提供
	// ETag/Last-Modified 时，留着它们会让命中回放一个对不上的校验符。
	object.ETag = in.ETag
	object.LastModified = in.LastModified
	object.Immutable = in.Immutable
	object.CacheControl = ""
	object.OriginDate = ""
	object.OriginAge = 0
	object.OriginExpires = ""
	object.Vary = ""
	object.AcceptRanges = ""
	object.ContentDisposition = ""
	object.DockerContentDigest = ""
	object.XChecksumMD5 = ""
	object.XChecksumSHA1 = ""
	object.XChecksumSHA256 = ""
	object.XChecksumSHA512 = ""
	object.StoredAt = 0
	object.RequiresRevalidation = false
	object.ExpiresAt = 0
	if in.Header != nil {
		object.ContentType = safeHeaderValue(in.Header.Get("Content-Type"))
		object.ETag = safeHeaderValue(in.Header.Get("Etag"))
		object.LastModified = safeHeaderValue(in.Header.Get("Last-Modified"))
		object.CacheControl = safeHeaderValue(in.Header.Get("Cache-Control"))
		object.OriginDate = safeHeaderValue(in.Header.Get("Date"))
		object.OriginExpires = safeHeaderValue(in.Header.Get("Expires"))
		object.Vary = safeHeaderValue(in.Header.Get("Vary"))
		object.AcceptRanges = safeHeaderValue(in.Header.Get("Accept-Ranges"))
		object.ContentDisposition = safeHeaderValue(in.Header.Get("Content-Disposition"))
		object.DockerContentDigest = safeHeaderValue(in.Header.Get("Docker-Content-Digest"))
		object.XChecksumMD5 = safeHeaderValue(in.Header.Get("X-Checksum-MD5"))
		object.XChecksumSHA1 = safeHeaderValue(in.Header.Get("X-Checksum-SHA1"))
		object.XChecksumSHA256 = safeHeaderValue(in.Header.Get("X-Checksum-SHA256"))
		object.XChecksumSHA512 = safeHeaderValue(in.Header.Get("X-Checksum-SHA512"))
		object.StoredAt = now
		object.OriginAge = correctedInitialAge(in.Header, nowTime)
		directives := parseCacheControl(object.CacheControl)
		_, noCache := directives["no-cache"]
		object.RequiresRevalidation = noCache
	}
	ttl := in.TTLSeconds
	if !in.Immutable && ttl <= 0 {
		ttl = c.limits(ctx).MutableTTLSeconds
	}
	object.ExpiresAt = effectiveExpiration(nowTime, ttl, in.Immutable, object)
	object.LastAccessAt = now
	object.Updatetime = now
	if err := repo.Save(ctx, object); err != nil {
		return err
	}
	if previousDigest != "" && previousDigest != in.Digest {
		c.removeIfUnreferenced(ctx, previousDigest)
	}
	return nil
}

// enforceQuota 超配额就按最近最少使用淘汰，直到回到回收水位。
//
// 淘汰只落在不可变且未被 pin 的对象上（由 EvictCandidates 保证）：可变对象由 TTL
// 自行过期，pin 的对象是人明确要求留下的。
//
// 跑过一轮就往事件流上记一条（概览：自动告警与人为变更放在同一条时间线上）。
// 记的是**这一轮**，不是每个被淘汰的对象：一次回收可能带走几百个对象，逐个记
// 会把时间线冲掉，而运维要看的是「这台机器什么时候开始削缓存了」。
func (c *cacheSvc) enforceQuota(ctx context.Context) {
	c.evictMu.Lock()
	defer c.evictMu.Unlock()

	repo := cache_repo.CacheObject()
	total, err := repo.TotalSize(ctx)
	if err != nil {
		logger.Ctx(ctx).Error("统计缓存占用失败", zap.Error(err))
		return
	}
	// 配额与水位在这里现读：站长在设置页把配额改小，下一次写进缓存的对象就会
	// 按新配额触发回收，不必重启进程（决策 3/4）。
	limits := c.limits(ctx)
	quota := limits.CacheQuotaBytes
	if total <= quota {
		return
	}
	// 淘汰到回收水位而不是刚好等于配额，否则之后每写一个对象都要再淘汰一次。
	waterline := quota * int64(limits.CacheReclaimPercent) / 100
	removed, freed := c.reclaim(ctx, repo, total, waterline)
	if removed == 0 {
		// 一个都没淘汰掉（候选空了、或者删不动）：那些情况上面已经各留了一条
		// 日志，而时间线上记一条「回收了 0 个对象」只是噪声。
		return
	}
	metrics.RecordEviction(metrics.EvictionLRU, removed)
	event_svc.Event().Record(ctx, &event_svc.RecordInput{
		Kind: event_entity.KindCacheReclaimed, Actor: event_entity.ActorSystem,
		Detail: map[string]any{
			"removed":     removed,
			"freed_bytes": freed,
			"quota_bytes": quota,
		},
	})
}

// reclaim 按最近最少使用淘汰到水位以下，返回淘汰了几条、腾出多少字节。
//
// 半途出错就带着已经削掉的量返回：那部分淘汰已经发生了，报成 0 会让时间线
// 说谎。调用方持着 evictMu，这里不再加锁。
func (c *cacheSvc) reclaim(ctx context.Context, repo cache_repo.CacheObjectRepo,
	total, waterline int64) (removed, freed int64) {
	for total > waterline {
		candidates, err := repo.EvictCandidates(ctx, evictBatch)
		if err != nil {
			logger.Ctx(ctx).Error("查询淘汰候选失败", zap.Error(err))
			return removed, freed
		}
		if len(candidates) == 0 {
			// 剩下的全是 pin 的或可变的：这不是可以静默忽略的状态，配额已经守不住了。
			logger.Ctx(ctx).Error("缓存超配额但没有可淘汰的对象",
				zap.Int64("total", total), zap.Int64("waterline", waterline))
			return removed, freed
		}
		for _, object := range candidates {
			if err := repo.Delete(ctx, object.ID); err != nil {
				logger.Ctx(ctx).Error("淘汰缓存记录失败", zap.Int64("id", object.ID), zap.Error(err))
				return removed, freed
			}
			// 记一笔「这条是被淘汰走的」：下一次拉到它时，表里同样什么都
			// 查不到，而它和一次首次拉取说的是相反的事。
			c.forgot.remember(object.UpstreamID, object.Key, metrics.MissEvicted)
			c.removeIfUnreferenced(ctx, object.Digest)
			total -= object.Size
			removed++
			freed += object.Size
			if total <= waterline {
				return removed, freed
			}
		}
	}
	return removed, freed
}

// sweepBatch 一趟清理最多收多少条。和淘汰用同一个批量：它们面对的是同一张表。
const sweepBatch = evictBatch

func (c *cacheSvc) Sweep(ctx context.Context) (int64, error) {
	if !c.usable() {
		return 0, nil
	}
	repo := cache_repo.CacheObject()
	now := time.Now().Unix()
	var removed int64
	for {
		expired, err := repo.ExpiredBefore(ctx, now, sweepBatch)
		if err != nil {
			logger.Ctx(ctx).Error("查询过期缓存对象失败", zap.Error(err))
			// 带着已经收掉的数量返回：那部分清理已经发生了，报成 0 会让调用方
			// 以为什么都没做。
			return removed, err
		}
		if len(expired) == 0 {
			break
		}
		batchRemoved := int64(0)
		for _, object := range expired {
			deleted, err := repo.DeleteExpired(ctx, object.ID, now)
			if err != nil {
				logger.Ctx(ctx).Error("删除过期缓存记录失败",
					zap.Int64("id", object.ID), zap.Error(err))
				return removed, err
			}
			if !deleted {
				// 候选查询之后可能有命中路径把旧 registry digest 提升成不可变对象；
				// 条件删除没动这一行时，它的记录与文件都必须原样留下。
				continue
			}
			// 过期被收走的记录同样查不到了，但它是 TTL 到期，不是从没缓存过——
			// 少了这一笔，一个全是可变对象的上游会显示成「几乎都是首次拉取」。
			c.forgot.remember(object.UpstreamID, object.Key, metrics.MissTTL)
			c.removeIfUnreferenced(ctx, object.Digest)
			removed++
			batchRemoved++
		}
		if batchRemoved == 0 {
			// 候选状态可在查询后并发变化；即使 repository 实现或历史数据不一致，
			// 一整页都没删掉时也不能立刻重查同一页形成忙循环。
			break
		}
		if len(expired) < sweepBatch {
			break
		}
	}
	metrics.RecordEviction(metrics.EvictionTTL, removed)
	// 回收放在清理之后：过期对象刚腾出来的空间要先算进去，否则会多削一批
	// 本来不必动的不可变对象。
	c.enforceQuota(ctx)
	return removed, nil
}

func (c *cacheSvc) Put(ctx context.Context, req *PutRequest) error {
	// 少了任何一样都会写出一条指不到任何东西的记录：键为空的记录之后既查不到
	// 也淘汰不掉，内容为空则直接在拷贝时崩掉。
	if req.UpstreamID <= 0 || req.Key == "" || req.Content == nil {
		return ErrPutIncomplete
	}
	if !c.usable() {
		return ErrCacheUnavailable
	}
	writer, err := c.store.Create()
	if err != nil {
		return err
	}
	defer func() { _ = writer.Close() }()
	if _, err := io.Copy(writer, req.Content); err != nil {
		return err
	}
	digest, size, err := writer.Commit()
	if err != nil {
		return err
	}
	if err := c.saveRecord(ctx, &recordInput{
		UpstreamID:  req.UpstreamID,
		Key:         req.Key,
		Digest:      digest,
		Size:        size,
		ContentType: req.ContentType,
		Immutable:   req.Immutable,
		TTLSeconds:  req.TTLSeconds,
	}); err != nil {
		return err
	}
	c.enforceQuota(ctx)
	return nil
}

func (c *cacheSvc) Search(ctx context.Context, req *SearchRequest) (*SearchResponse, error) {
	page, size := req.Page, req.Size
	if page <= 0 {
		page = 1
	}
	if size <= 0 || size > 200 {
		size = 20
	}
	list, total, err := cache_repo.CacheObject().Search(ctx, &cache_entity.SearchOption{
		UpstreamID: req.UpstreamID,
		Keyword:    req.Keyword,
		Offset:     (page - 1) * size,
		Limit:      size,
	})
	if err != nil {
		return nil, err
	}
	return &SearchResponse{List: list, Total: total, Page: page, Size: size}, nil
}

func (c *cacheSvc) Purge(ctx context.Context, req *PurgeRequest) (*PurgeResponse, error) {
	repo := cache_repo.CacheObject()
	var objects []*cache_entity.CacheObject
	skipped := int64(0)
	switch {
	case req.ID > 0:
		object, err := repo.Find(ctx, req.ID)
		if err != nil {
			return nil, err
		}
		if object == nil {
			// 不存在的 id 不能算清除成功：那会让界面上一次点错的清除看起来
			// 生效了，而对象其实还在。
			return nil, i18n.NewNotFoundError(ctx, code.CacheObjectNotFound)
		}
		// 指名道姓清一条时不看 pinned：这是人指着这一条说「就清它」，
		// 再拦一道只会逼他先解开 pin 再清，白绕一圈。
		objects = append(objects, object)
	case req.UpstreamID > 0:
		list, err := repo.ListByUpstream(ctx, req.UpstreamID)
		if err != nil {
			return nil, err
		}
		for _, object := range list {
			// pin 表达的是「这份内容要常驻」。它挡住了 LRU 淘汰，也必须挡住
			// 批量清除，否则清一次上游就把所有 pin 过的对象顺手抹了。
			if object.Pinned {
				skipped++
				continue
			}
			objects = append(objects, object)
		}
	default:
		// 不给「清空一切」留一个不写参数就能触发的形态。
		return nil, i18n.NewError(ctx, code.PurgeTargetRequired)
	}
	removed := int64(0)
	for _, object := range objects {
		if err := repo.Delete(ctx, object.ID); err != nil {
			return nil, err
		}
		// 人手清掉的对象，再被拉回来就是一次首次拉取。这一笔还顺带盖掉它更早
		// 之前留下的那条淘汰记录，否则清完缓存的第一次拉取会报成「被淘汰」。
		c.forgot.remember(object.UpstreamID, object.Key, metrics.MissFirst)
		if c.store != nil {
			c.removeIfUnreferenced(ctx, object.Digest)
		}
		removed++
	}
	metrics.RecordEviction(metrics.EvictionManual, removed)
	return &PurgeResponse{Removed: removed, Skipped: skipped}, nil
}

func (c *cacheSvc) Pin(ctx context.Context, req *PinRequest) error {
	repo := cache_repo.CacheObject()
	// 先确认对象在：SetPinned 更新 0 行也返回 nil，不查一次就等于对着一个
	// 空操作回 200，界面上那个图钉会亮着，而库里什么都没变。
	object, err := repo.Find(ctx, req.ID)
	if err != nil {
		return err
	}
	if object == nil {
		return i18n.NewNotFoundError(ctx, code.CacheObjectNotFound)
	}
	return repo.SetPinned(ctx, req.ID, req.Pinned)
}
