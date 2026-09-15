// Package git_ctr 处理 git 本地镜像管理接口的请求。
package git_ctr

import (
	"context"
	"errors"
	"net/http"

	"github.com/cago-frame/cago/pkg/utils/httputils"

	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/model/entity/git_entity"
	"github.com/CodFrm/katch/internal/service/git_svc"
)

// 这两个码只在本包内使用，不进 internal/pkg/code 那张全局表：muxclient 按
// `code != 0` 判定一次调用是不是成功（见 cago 的 client.go），给 0 会让一个
// 404/409 的响应被当成成功悄悄放过去。界面按状态码而不是按这两个数分支
// （lib/errors.ts 认不出的码会退到一条通用文案），所以它们不必是稳定契约。
const (
	codeMirrorNotFound = 1
	codeMirrorInUse    = 2
)

// Git 本地镜像管理控制器。
type Git struct{}

// NewGit 构造本地镜像管理控制器。
func NewGit() *Git {
	return &Git{}
}

// List 列出全部镜像记录，管理界面的镜像列表页用。
//
// 这里做请求与实体的互译，而不是让 git_svc 收发 admin 包的结构体：镜像层同时
// 长在拉取路径上（Ensure/Lookup/Answer），让它依赖一组管理接口的 DTO 会把两条
// 路径绑在一起（同 cache_ctr.Search 的理由）。
func (g *Git) List(ctx context.Context, _ *admin.ListGitMirrorsRequest) (*admin.ListGitMirrorsResponse, error) {
	list, err := git_svc.List(ctx)
	if err != nil {
		return nil, err
	}
	resp := &admin.ListGitMirrorsResponse{List: make([]*admin.GitMirrorItem, 0, len(list))}
	for _, mirror := range list {
		resp.List = append(resp.List, toItem(mirror))
	}
	return resp, nil
}

// Delete 删除一条镜像：盘上的目录与库记录都会被清掉，下一次拉取因此重新走
// 穿透并在后台重建（镜像生命周期一节）。
func (g *Git) Delete(ctx context.Context, req *admin.DeleteGitMirrorRequest) (*admin.DeleteGitMirrorResponse, error) {
	err := git_svc.Delete(ctx, req.ID)
	switch {
	case errors.Is(err, git_svc.ErrMirrorNotFound):
		// 不存在的 id 不能算删除成功：那会让界面上一次点错的删除看起来生效了，
		// 而记录其实还在（同 cache_ctr 对不存在对象的处理）。
		return nil, httputils.NewError(http.StatusNotFound, codeMirrorNotFound, err.Error())
	case errors.Is(err, git_svc.ErrMirrorInUse):
		// 冲突而不是「你的请求有问题」：这个 id 本身没错，只是此刻不是删它的
		// 时机——半路抽掉目录会让正在进行的那次 clone 散架。
		return nil, httputils.NewError(http.StatusConflict, codeMirrorInUse, err.Error())
	case err != nil:
		return nil, err
	}
	return &admin.DeleteGitMirrorResponse{}, nil
}

func toItem(mirror *git_entity.GitMirror) *admin.GitMirrorItem {
	return &admin.GitMirrorItem{
		ID:           mirror.ID,
		Host:         mirror.Host,
		Repo:         mirror.Repo,
		State:        mirror.State,
		SizeBytes:    mirror.SizeBytes,
		LastSyncAt:   mirror.LastSyncAt,
		LastAccessAt: mirror.LastAccessAt,
		LastError:    mirror.LastError,
		Createtime:   mirror.Createtime,
		Updatetime:   mirror.Updatetime,
	}
}
