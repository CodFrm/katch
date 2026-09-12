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
)

func init() {
	i18n.Register("zh-cn", zhCN)
}

var zhCN = map[int]string{
	AdminKeyInvalid:        "管理密钥无效",
	UpstreamNotFound:       "上游不存在",
	UpstreamHostExists:     "该上游主机名已存在",
	AdminKeyNotInitialized: "尚未设置管理密钥，请在 configs/config.yaml 的 admin.initialKey 中给出初始密钥",
	RuleNotFound:           "访问规则不存在",
}
