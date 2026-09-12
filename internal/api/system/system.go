// Package system 定义系统自身信息的请求/响应结构。
package system

import "github.com/cago-frame/cago/server/mux"

// VersionRequest 查询构建版本。
type VersionRequest struct {
	mux.Meta `path:"/system/version" method:"GET"`
}

// VersionResponse 构建版本信息。排障时用来确认线上跑的到底是哪个 commit。
type VersionResponse struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}
