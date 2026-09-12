// Package system_ctr 处理系统自身信息的请求。
package system_ctr

import (
	"context"

	"github.com/CodFrm/katch/internal/api/system"
	"github.com/CodFrm/katch/internal/buildinfo"
)

// System 系统信息控制器。
type System struct{}

// NewSystem 构造系统信息控制器。
func NewSystem() *System {
	return &System{}
}

// Version 返回构建版本信息。
func (s *System) Version(_ context.Context, _ *system.VersionRequest) (*system.VersionResponse, error) {
	return &system.VersionResponse{
		Version: buildinfo.Version,
		Commit:  buildinfo.Commit,
	}, nil
}
