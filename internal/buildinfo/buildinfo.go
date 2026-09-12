// Package buildinfo 保存构建期注入的版本信息。
//
// 两个变量都由 Makefile 通过 -ldflags -X 写入，源码里留的是「没有注入」时的
// 占位值：排障时看到 dev(unknown) 就说明这个二进制不是 make build 出来的。
package buildinfo

var (
	// Version 语义化版本号，由 Makefile 的 VERSION 注入。
	Version = "dev"
	// Commit 构建时的 git 短 SHA，由 Makefile 的 COMMIT 注入。
	Commit = "unknown"
)
