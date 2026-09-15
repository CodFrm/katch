// Package event_entity 定义事件流的实体。
//
// 事件流是后台概览上的那条时间线：自动告警与人为变更放在一起，带操作人
// （「管理接口与界面」一节）。它是**只追加**的观察记录，不是审批流——
// 规则的草稿与版本回滚在这一轮的 Out of scope 里，这张表只回答
// 「什么时候、谁、把什么改成了什么」。
package event_entity

// 事件类别。这些值会落库并原样发给界面，界面按它查翻译表，
// 因此它们是**稳定枚举**，不是给人读的句子：一旦写成英文散文，
// 中英双语的界面就只剩下把后端字符串原样贴给用户这一条路。
//
// 改这些字面量等于改一份已经落库的历史：旧行不会跟着变，只会在界面上翻不出来。
const (
	// KindUpstreamCreated 新增了一个上游。
	KindUpstreamCreated = "upstream_created"
	// KindUpstreamUpdated 改了一个上游（含启停）。
	KindUpstreamUpdated = "upstream_updated"
	// KindUpstreamDeleted 删了一个上游。
	KindUpstreamDeleted = "upstream_deleted"
	// KindRuleCreated 新增了一条访问规则。
	KindRuleCreated = "rule_created"
	// KindRuleUpdated 改了一条访问规则。
	KindRuleUpdated = "rule_updated"
	// KindRuleDeleted 删了一条访问规则。
	KindRuleDeleted = "rule_deleted"
	// KindSettingChanged 改了运行时设置。
	KindSettingChanged = "setting_changed"
	// KindAdminKeyRotated 轮换了管理密钥。
	KindAdminKeyRotated = "admin_key_rotated"
	// KindUpstreamDegraded 上游连续回源失败，进入退避（决策 17：界面标为限流中/降级）。
	KindUpstreamDegraded = "upstream_degraded"
	// KindUpstreamRecovered 上游回源重新成功，退出退避。
	KindUpstreamRecovered = "upstream_recovered"
	// KindCacheReclaimed 缓存超配额，跑了一次回收。
	KindCacheReclaimed = "cache_reclaimed"
	// KindGitMirrorPending 某个 git 仓库被拉到了，登记为待建镜像。
	//
	// 四种状态各一个类别，而不是一个 git_mirror_state 加一个 to 字段：
	// 界面上的时间线按类别取文案，合成一个类别就得在文案里再分一次支，
	// 而「建成了」和「建不了」本来就是两件该分开说的事。
	KindGitMirrorPending = "git_mirror_pending"
	// KindGitMirrorReady 镜像建成，盘上有一份可用的裸仓库。
	KindGitMirrorReady = "git_mirror_ready"
	// KindGitMirrorFailed 建镜像失败，原因在细节里。
	KindGitMirrorFailed = "git_mirror_failed"
	// KindGitMirrorRejected 仓库体积超过单仓上限，此后永久穿透。
	KindGitMirrorRejected = "git_mirror_rejected"
	// KindGitMirrorEvicted 镜像总量超过配额，这个仓库整份被回收掉。
	//
	// 和 cache_reclaimed 分开：那一条说的是一轮回收删了多少个对象，而镜像按
	// 仓库粒度整个删，站长要知道的恰恰是「消失的是哪个仓库」——合成一个汇总
	// 数字，时间线就答不出「我的 clone 为什么又开始穿透了」。
	KindGitMirrorEvicted = "git_mirror_evicted"
)

// 操作人。同样是稳定枚举，界面据此把人为变更和自动事件在同一条时间线上区分开。
//
// 只有两个取值：这一轮没有账号体系，后台只有一把管理密钥（Out of scope 的
// 「多用户与角色」）。真有了多个操作人，这里加的是取值，不是再开一张表。
const (
	// ActorAdmin 带着管理密钥的人。
	ActorAdmin = "admin"
	// ActorSystem katch 自己：退避、缓存回收这些没有人按下按钮的事。
	ActorSystem = "system"
)

// Event 时间线上的一条事件。
//
// 没有 updatetime：事件只追加、不修改——一条改过的历史记录不再是历史。
type Event struct {
	ID int64 `gorm:"column:id;primary_key" json:"id"`
	// Kind 事件类别，取上面那组枚举。
	Kind string `gorm:"column:kind" json:"kind"`
	// Actor 操作人，取上面那组枚举。
	Actor string `gorm:"column:actor" json:"actor"`
	// UpstreamID 这条事件属于哪个上游，0 表示与具体上游无关（全局规则、设置、密钥）。
	UpstreamID int64 `gorm:"column:upstream_id" json:"upstream_id"`
	// Detail 「改了什么」的结构化记录，一个 JSON 对象。
	//
	// 存的是字段而不是一句拼好的话：句子里没有界面能翻译的东西，也没法按字段
	// 筛选，而两套语言的文案本来就该由界面自己组织。
	Detail     string `gorm:"column:detail" json:"detail"`
	Createtime int64  `gorm:"column:createtime" json:"createtime"`
}
