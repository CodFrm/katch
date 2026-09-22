package cache_svc

import (
	"net/http"
	"strconv"
	"sync"

	"github.com/CodFrm/katch/internal/metrics"
	"github.com/CodFrm/katch/internal/model/entity/cache_entity"
	"github.com/CodFrm/katch/internal/service/proxy_svc"
)

// 界面上的「回源原因」要的是每一次未命中的归因，而这个判断只有这一层做得出来：
// 拉取路径最外层那个计数器看到的只是一个 200 加一句 MISS，它分不出「从来没缓存
// 过」和「缓存过但被淘汰了」——两者在表上都只是查不到记录。
//
// 归因写在响应头上交给那个计数器收进分钟桶，而不是在这里直接往库里记：决策 16
// 否掉的正是每请求写库。写在响应头上还顺带让计数只有一个出处——四个原因之和
// 恒等于未命中数，因为它们和 hit / denied / origin_error 是同一次判定分出来的。

// upstreamDigestHeader 上游用来回答「你手上那份还是不是我这份」的头。
//
// registry 的 manifest 响应带着它，值就是这份字节的 sha256，和我们内容寻址存
// 下来的摘要同一个格式。静态上游不给这个头，那时我们就不对「内容变没变」下结论。
const upstreamDigestHeader = "Docker-Content-Digest"

// miss 一次未命中，以及它为什么发生。
type miss struct {
	reason metrics.MissReason
	// heldDigest 未命中之前手上那份副本的摘要。
	//
	// 空表示手上没有一份可信的副本可比——从没缓存过是这样，副本已经校验失败
	// 被丢掉了也是这样：那时「我们手上有什么」本身就不可信。
	heldDigest string
	// held 过期但带着上游 validator 的那份副本；有它，回源才能发条件请求并按 304 续期。
	held *cache_entity.CacheObject
}

// stamp 把归因写进响应头，返回同一个 meta 供调用方接着用。
//
// 收口在一处而不是在每个未命中的出口各写一次：归因必须在第一个字节发出去之前
// 定下来（发出去之后这次请求就没得改了），而「上游那份还是不是我们那份」要等
// 回源的响应头到手才知道——这两个时刻之间只有这一个位置。
func (m *miss) stamp(meta *proxy_svc.Meta) *proxy_svc.Meta {
	if meta == nil {
		return meta
	}
	if meta.Header == nil {
		meta.Header = make(http.Header, 1)
	}
	if meta.Header.Get(cacheStatusHeader) == cacheStatusHit {
		// 搭上了一次已经下完的回源：这一份是从缓存里读出来的，不是一次回源，
		// 计数器也会把它记成命中，再盖一个原因上去就成了双份。
		return meta
	}
	if m.changed(meta) {
		m.reason = metrics.MissChanged
	}
	meta.Header.Set(metrics.MissHeader, string(m.reason))
	return meta
}

// changed 上游给出的 digest 和手上那份对不上。
//
// 这才是站长要区分的东西：一个 tag 每小时真的换了内容，和一个 TTL 配得太短、
// 每次回源都拉回一模一样的字节，在「回源了多少次」上完全一样，在该不该调 TTL
// 上却相反。上游不给 digest 时不下结论——没有证据就保留原来的归因，而不是猜。
func (m *miss) changed(meta *proxy_svc.Meta) bool {
	if m.heldDigest == "" {
		return false
	}
	digest := meta.Header.Get(upstreamDigestHeader)
	return digest != "" && digest != m.heldDigest
}

// forgetGeneration 每一代记住多少条被自己收走的键。
//
// 有上限是因为这张表长在内存里，而键来自公开的拉取路径；两代加起来的上限是
// 它的两倍，一条记录就是一个键加一个短枚举。
const forgetGeneration = 4096

// forgotten 记住「这条记录是被我们自己收走的，收的时候算哪一档」。
//
// 淘汰、TTL 清理和人工清缓存都会把记录删掉，于是下一次拉取在表里什么也查不到，
// 和「从来没缓存过」长得一模一样——而这两件事说的正好相反：一个是缓存还没热
// 起来，一个是配额太小或 TTL 太短。
//
// 内存态而不是落库，理由和决策 17 把退避留在内存里是同一条：它只服务于「下一次
// 拉到这个键时怎么归因」，落库要么把每次淘汰变成一次写事务（决策 16 否掉的就是
// 这个），要么留下一张只增不减的墓碑表。代价是进程重启后这一批归因退回首次拉取，
// 而占比条看的是趋势，不是审计。
//
// 分两代整代丢弃，而不是逐条 LRU：这张表只是归因的旁证，为它再维护一条访问
// 顺序链比它本身还贵。
type forgotten struct {
	mu   sync.Mutex
	cur  map[string]metrics.MissReason
	prev map[string]metrics.MissReason
}

func newForgotten() *forgotten {
	return &forgotten{cur: map[string]metrics.MissReason{}}
}

// remember 记下这条记录是被谁、按哪一档收走的。
func (f *forgotten) remember(upstreamID int64, key string, reason metrics.MissReason) {
	name := objectKey(upstreamID, key)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.cur) >= forgetGeneration {
		// 整代翻页：新的一代空着，上一代还能再答一轮。
		f.prev, f.cur = f.cur, map[string]metrics.MissReason{}
	}
	f.cur[name] = reason
	// 新的一代已经有答案了，老的那份不能再压过它。
	delete(f.prev, name)
}

// recall 这个键是不是被我们自己收走的。不是（或者已经忘了）时返回首次拉取。
func (f *forgotten) recall(upstreamID int64, key string) metrics.MissReason {
	name := objectKey(upstreamID, key)
	f.mu.Lock()
	defer f.mu.Unlock()
	if reason, ok := f.cur[name]; ok {
		return reason
	}
	if reason, ok := f.prev[name]; ok {
		return reason
	}
	return metrics.MissFirst
}

// objectKey 一个缓存对象在进程内的名字：上游 id 加上游内的键。
//
// 中间夹一个不可能出现在路径里的字节，否则 (1, "0/a") 和 (10, "/a") 会撞成
// 同一个名字——撞上的后果是两个对象共用一次回源。
func objectKey(upstreamID int64, key string) string {
	return strconv.FormatInt(upstreamID, 10) + "\x00" + key
}
