// Package code 定义 katch 的业务错误码与对应文案。
//
// 错误码是对外契约的一部分：前端按码分支、按 msg 展示，所以码值只增不改。
package code

import "github.com/cago-frame/cago/pkg/i18n"

const (
	// AdminKeyInvalid 管理密钥无效。
	//
	// 「未提供密钥」和「密钥错误」共用这一个码，不是偷懒：两者必须返回完全相同的
	// 响应，分成两个码就等于告诉探测者「密钥这个字段你找对了，只是值不对」。
	AdminKeyInvalid = iota + 10000
	// UpstreamNotFound 上游不存在。
	UpstreamNotFound
	// UpstreamHostExists 上游主机名已存在。
	UpstreamHostExists
	// AdminKeyNotInitialized 库里没有管理密钥，配置里也没给初始密钥。
	AdminKeyNotInitialized
	// RuleNotFound 访问规则不存在。
	RuleNotFound
	// CacheObjectNotFound 缓存对象不存在。
	CacheObjectNotFound
	// PurgeTargetRequired 清缓存没说清谁。
	PurgeTargetRequired
	// SettingKeyUnknown 写入了一个不认识的设置项。
	SettingKeyUnknown
	// SettingValueInvalid 设置项的值不合法（类型不对或超出取值范围）。
	SettingValueInvalid
	// StorageUnavailable 数据库连不上，管理面暂时不可用。
	//
	// 它和「系统错误」分开是有意的：这一条说的是存储临时挂了、待会儿再来，
	// 而拉取仍然在用进程内的快照继续服务（失败与降级一节）。
	StorageUnavailable
	// CacheTreePathInvalid 目录树路径不合法：以 / 开头或结尾、含空段或 ..、过长。
	CacheTreePathInvalid
	// CacheTreeKeywordInvalid 目录树搜索词为空或过长。
	CacheTreeKeywordInvalid
)

func init() {
	i18n.Register("zh-cn", zhCN)
}

var zhCN = map[int]string{
	AdminKeyInvalid:         "管理密钥无效",
	UpstreamNotFound:        "上游不存在",
	UpstreamHostExists:      "该上游主机名已存在",
	AdminKeyNotInitialized:  "尚未设置管理密钥，请在 configs/config.yaml 的 admin.initialKey 中给出初始密钥",
	RuleNotFound:            "访问规则不存在",
	CacheObjectNotFound:     "缓存对象不存在",
	PurgeTargetRequired:     "请指定要清理的缓存对象或上游",
	SettingKeyUnknown:       "没有 %s 这个设置项",
	SettingValueInvalid:     "设置项 %s 的值不合法：%s",
	StorageUnavailable:      "数据库暂时不可用，管理功能稍后恢复；镜像拉取不受影响",
	CacheTreePathInvalid:    "目录路径不合法",
	CacheTreeKeywordInvalid: "请输入不超过 256 个字符的搜索词",
}
