// Package cache_svc 是缓存层：拉取路径在回源之前先问它一次。
//
// 它站在 proxy_svc 之前——命中就由磁盘服务，未命中才回源，并且一边向客户端流式
// 转发一边写入缓存（不得先下载完整对象再开始响应）。缓存坏掉、磁盘不可用、数据库
// 不可用时，它一律退回成纯透传：缓存是优化，它坏掉不该让拉取整体失败。
package cache_svc

import (
	"context"
	"errors"
	"io"
	"net/http"
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
	copyBufferSize = 64 << 10
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
}

type cacheSvc struct {
	store *cache.Store
	// runtime 配额、回收水位与可变对象 TTL 的来源，每次用到时现读。
	runtime setting_svc.RuntimeSource
	// mu 只护 inflight 这张表。
	mu       sync.Mutex
	inflight map[string]*flight
	// evictMu 把淘汰串起来：几个下载同时收尾时各淘汰各的，会把缓存削过头。
	evictMu sync.Mutex
}

// New 构造缓存层。store 为 nil 表示磁盘不可用——此时它是一个纯透传的实现，
// 拉取照常，只是不命中也不写入。
func New(store *cache.Store, opt Options) CacheSvc {
	if opt.Runtime == nil {
		opt.Runtime = setting_svc.Setting()
	}
	return &cacheSvc{store: store, runtime: opt.Runtime, inflight: map[string]*flight{}}
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
	if !c.usable() || !cacheableRequest(target) {
		return proxy_svc.Proxy().Fetch(ctx, target)
	}
	upstream, err := upstream_svc.Upstream().FindByHost(ctx, target.Host)
	if err != nil || upstream == nil {
		// 查不到上游、或者库不可用，都交回代理层：白名单这道闸和它给出的统一
		// 404 只能有一个出处，在这里复述一遍迟早会和那边走偏。
		return proxy_svc.Proxy().Fetch(ctx, target)
	}
	key := cacheKey(target)
	if body, meta, ok := c.serveFromDisk(ctx, upstream, key); ok {
		return body, meta, nil
	}
	return c.fetchAndCache(ctx, target, upstream, key)
}

func (c *cacheSvc) usable() bool {
	return c.store != nil && cache_repo.CacheObject() != nil
}

// cacheableRequest 只有「要一份完整对象」的 GET 才走缓存。
//
// HEAD 没有响应体；带 Range 或条件头的请求拿到的是半截或 304，把它们写进缓存
// 就是把半截当整份。这类请求直接透传给上游，由上游自己回答。
func cacheableRequest(target *proxy_svc.Target) bool {
	if target.Method != http.MethodGet {
		return false
	}
	for _, h := range []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since"} {
		if target.Header.Get(h) != "" {
			return false
		}
	}
	return true
}

// cacheableResponse 哪些响应可以留下来。
func cacheableResponse(meta *proxy_svc.Meta) bool {
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
	cc := strings.ToLower(meta.Header.Get("Cache-Control"))
	return !strings.Contains(cc, "no-store") && !strings.Contains(cc, "private")
}

// cacheKey 缓存键：上游内路径加查询串。
//
// 带上查询串是因为它会改变返回的内容；上游 ID 不进键里，它是表上的另一列。
func cacheKey(target *proxy_svc.Target) string {
	if target.RawQuery == "" {
		return target.Path
	}
	return target.Path + "?" + target.RawQuery
}

// serveFromDisk 命中则由磁盘服务，返回的第三个值表示这次是不是命中。
func (c *cacheSvc) serveFromDisk(ctx context.Context, upstream *upstream_entity.Upstream, key string) (io.ReadCloser, *proxy_svc.Meta, bool) {
	repo := cache_repo.CacheObject()
	object, err := repo.FindByKey(ctx, upstream.ID, key)
	if err != nil {
		// 库不可用不该让拉取停摆：当成未命中，回源照常。
		logger.Ctx(ctx).Error("读缓存记录失败", zap.String("key", key), zap.Error(err))
		return nil, nil, false
	}
	if object == nil || object.Expired(time.Now().Unix()) {
		return nil, nil, false
	}
	file, size, err := c.store.Open(object.Digest)
	if err != nil {
		// 记录还在、文件没了：这是磁盘或写入路径出了问题，不能静默自愈了事。
		logger.Ctx(ctx).Error("缓存副本丢失", zap.String("key", key),
			zap.String("digest", object.Digest), zap.Error(err))
		c.dropRecord(ctx, object)
		return nil, nil, false
	}
	if size != object.Size {
		_ = file.Close()
		logger.Ctx(ctx).Error("缓存副本大小与记录不符", zap.String("key", key),
			zap.Int64("want", object.Size), zap.Int64("got", size))
		metrics.RecordIntegrityFailure(upstream.Host)
		c.dropRecord(ctx, object)
		return nil, nil, false
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
		return nil, nil, false
	}
	if err := repo.Touch(ctx, object.ID, time.Now().Unix()); err != nil {
		// 命中已经成立了，访问时间没更新上只影响淘汰顺序，不该让这次拉取失败。
		logger.Ctx(ctx).Warn("更新缓存访问时间失败", zap.Int64("id", object.ID), zap.Error(err))
	}
	header := make(http.Header, 3)
	if object.ContentType != "" {
		header.Set("Content-Type", object.ContentType)
	}
	header.Set("Content-Length", strconv.FormatInt(object.Size, 10))
	header.Set(cacheStatusHeader, cacheStatusHit)
	return file, &proxy_svc.Meta{
		StatusCode:    http.StatusOK,
		Header:        header,
		ContentLength: object.Size,
	}, true
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
	upstream *upstream_entity.Upstream, key string) (io.ReadCloser, *proxy_svc.Meta, error) {
	flightKey := strconv.FormatInt(upstream.ID, 10) + "\x00" + key

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
	body, meta, err := proxy_svc.Proxy().Fetch(fetchCtx, target)
	if err != nil {
		c.forget(flightKey)
		current.startFailed(err)
		return nil, nil, err
	}
	if !cacheableResponse(meta) {
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
	go c.pump(fetchCtx, flightKey, current, body, writer, upstream, key, meta)
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
		return proxy_svc.Proxy().Fetch(ctx, target)
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
	key string, meta *proxy_svc.Meta) {
	defer func() {
		_ = body.Close()
		_ = writer.Close()
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
		} else if err := c.saveRecord(ctx, &recordInput{
			UpstreamID:  upstream.ID,
			Key:         key,
			Digest:      digest,
			Size:        size,
			ContentType: meta.Header.Get("Content-Type"),
			Immutable:   cache.IsImmutable(upstream.ImmutablePatterns, key),
			TTLSeconds:  int64(upstream.MutableTTLSeconds),
		}); err != nil {
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
	Immutable   bool
	// TTLSeconds 可变对象的存活时长，0 表示用全局默认值。
	TTLSeconds int64
}

// saveRecord 写入或更新一条缓存记录。
//
// 同一条路径只会有一条记录：重取（可变对象过期、或坏副本重下）要覆盖原来那条，
// 否则表里会为同一个 key 越堆越多，而唯一索引会在第二次写入时直接报错。
func (c *cacheSvc) saveRecord(ctx context.Context, in *recordInput) error {
	repo := cache_repo.CacheObject()
	now := time.Now().Unix()
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
	object.Immutable = in.Immutable
	object.ExpiresAt = 0
	if !in.Immutable {
		ttl := in.TTLSeconds
		if ttl <= 0 {
			// 上游没单独配 TTL 时用设置里的默认值，现读现用：站长把默认 TTL
			// 改短之后，下一个写进来的可变对象就按新值过期。
			ttl = c.limits(ctx).MutableTTLSeconds
		}
		object.ExpiresAt = now + ttl
	}
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
		for _, object := range expired {
			if err := repo.Delete(ctx, object.ID); err != nil {
				logger.Ctx(ctx).Error("删除过期缓存记录失败",
					zap.Int64("id", object.ID), zap.Error(err))
				return removed, err
			}
			c.removeIfUnreferenced(ctx, object.Digest)
			removed++
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
