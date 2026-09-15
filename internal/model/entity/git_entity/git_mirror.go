// Package git_entity 定义 git 本地镜像的实体。
package git_entity

// 镜像状态。这些值会落库，也会原样发给界面，因此是**稳定枚举**而不是给人读的
// 句子：一旦写成散文，中英双语的界面就只剩把后端字符串贴给用户这一条路。
const (
	// MirrorPending 已经登记、还没建成。此间的请求一律穿透。
	MirrorPending = "pending"
	// MirrorReady 盘上有一份可用的裸仓库。
	MirrorReady = "ready"
	// MirrorFailed 建镜像失败，原因记在 LastError 上。
	MirrorFailed = "failed"
	// MirrorRejected 仓库体积超过单仓上限，此后永久穿透（能力边界一节）。
	//
	// 和 failed 分开：failed 说的是「这次没成」，rejected 说的是「这个仓库
	// 本来就不该由本地应答」。合成一种状态，界面就没法把一次上游抖动和一个
	// 3 GB 的仓库分开说。
	MirrorRejected = "rejected"
)

// GitMirror 一个上游仓库在本地的镜像。
//
// 它是镜像的**目录之外的那份真相**：进程重启后据此知道盘上哪些镜像可用，
// 不必扫盘推断；也据此知道哪些仓库已经试过并失败、或者大到不该镜像。
type GitMirror struct {
	ID int64 `gorm:"column:id;primary_key" json:"id"`
	// Host 上游主机名，和 Repo 一起唯一确定一个仓库。
	Host string `gorm:"column:host" json:"host"`
	// Repo 仓库在上游侧的路径，转义形态、以 / 开头（dispatch.ClassifyGit 给的那一个）。
	Repo string `gorm:"column:repo" json:"repo"`
	// State 取上面那组枚举。
	State string `gorm:"column:state" json:"state"`
	// LastSyncAt 最后一次和上游同步成功的时刻（秒），没成功过是 0。
	LastSyncAt int64 `gorm:"column:last_sync_at" json:"last_sync_at"`
	// LastAccessAt 最后一次被请求的时刻（秒）。配额淘汰按它排序——镜像按仓库
	// 粒度整个删，和对象缓存的 LRU 不是一回事，所以这一列在这张表上。
	LastAccessAt int64 `gorm:"column:last_access_at" json:"last_access_at"`
	// SizeBytes 盘上这份镜像占多少字节，建成时量一次。
	SizeBytes int64 `gorm:"column:size_bytes" json:"size_bytes"`
	// LastError 最近一次失败的原因，成功时清空。
	//
	// 存一句原始的错误信息而不是一个码：镜像失败的原因是上游给的（拨不通、
	// 403、仓库不存在），穷举不了，而站长要看的恰恰是那句话本身。
	LastError  string `gorm:"column:last_error" json:"last_error"`
	Createtime int64  `gorm:"column:createtime" json:"createtime"`
	Updatetime int64  `gorm:"column:updatetime" json:"updatetime"`
}
