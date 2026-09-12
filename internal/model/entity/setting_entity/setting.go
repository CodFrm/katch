// Package setting_entity 定义运行时设置的实体。
package setting_entity

// Setting 运行时设置的一行键值。
//
// 这里放的是「进程跑起来之后才生效、因而应该能在界面上改」的东西；
// 监听地址、数据库 DSN、日志、初始管理密钥那类「进程起不来就没法从界面改」的
// 配置留在 configs/config.yaml 里。
type Setting struct {
	// Key 是主键。用字符串主键而不是自增 id：设置是按键读写的，
	// 自增 id 只会多出一次「先查 id 再更新」。
	Key        string `gorm:"column:key;primary_key" json:"key"`
	Value      string `gorm:"column:value" json:"value"`
	Createtime int64  `gorm:"column:createtime" json:"createtime"`
	Updatetime int64  `gorm:"column:updatetime" json:"updatetime"`
}
