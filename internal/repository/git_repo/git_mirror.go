// Package git_repo 是 git 本地镜像的数据访问层。
package git_repo

import (
	"context"

	"github.com/cago-frame/cago/database/db"

	"github.com/CodFrm/katch/internal/model/entity/git_entity"
)

//go:generate mockgen -source git_mirror.go -destination mock/git_mirror.go -package mock_git_repo

// GitMirrorRepo 镜像记录的存取。
//
// 「还没镜像过」不是错误：FindByRepo 未命中时返回 (nil, nil)，每一个第一次被拉到
// 的仓库都会走这一条，当成错误就是每次冷拉取都在日志里留一条 error。
type GitMirrorRepo interface {
	FindByRepo(ctx context.Context, host, repo string) (*git_entity.GitMirror, error)
	// Find 按主键取一条，没有时是 (nil, nil)，理由同 FindByRepo：管理界面删一条
	// 已经不在的镜像不该在日志里留一条 error。
	Find(ctx context.Context, id int64) (*git_entity.GitMirror, error)
	// Save 主键为空时插入，否则整行更新。
	Save(ctx context.Context, mirror *git_entity.GitMirror) error
	// Touch 只更新访问时间，不整行写回：每一次请求都会碰它，而整行写回会把
	// 后台那趟建镜像刚写下的状态覆盖回调用方手上那份旧的。
	Touch(ctx context.Context, id int64, at int64) error
	// Delete 删一条镜像记录，管理界面手动删除或配额淘汰时用。
	Delete(ctx context.Context, id int64) error
	// List 列出全部镜像记录，管理界面用。不分页：规模有配额顶着，一台机器上
	// 建出来的仓库是几十条而不是几万条（同上游列表）。
	List(ctx context.Context) ([]*git_entity.GitMirror, error)
	// TotalSize ready 状态镜像占用的总字节数，配额判定用。pending 还没落盘，
	// failed/rejected 建失败时已经把半成品清掉了，两者都不占地方。
	TotalSize(ctx context.Context) (int64, error)
	// EvictCandidates 按最后访问时间最旧的顺序给出可淘汰的 ready 镜像。
	EvictCandidates(ctx context.Context, limit int) ([]*git_entity.GitMirror, error)
}

var defaultGitMirror GitMirrorRepo

// GitMirror 返回已注册的实现。
func GitMirror() GitMirrorRepo {
	return defaultGitMirror
}

// RegisterGitMirror 注册实现，由 main 装配、由测试注入 mock。
func RegisterGitMirror(i GitMirrorRepo) {
	defaultGitMirror = i
}

type gitMirrorRepo struct{}

// NewGitMirror 构造基于 gorm 的实现。
func NewGitMirror() GitMirrorRepo {
	return &gitMirrorRepo{}
}

func (g *gitMirrorRepo) FindByRepo(ctx context.Context, host, repo string) (*git_entity.GitMirror, error) {
	ret := &git_entity.GitMirror{}
	if err := db.Ctx(ctx).Where("host=? AND repo=?", host, repo).First(ret).Error; err != nil {
		if db.RecordNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return ret, nil
}

// Save 整行写回。
//
// 用 Save 而不是 Updates(struct)：后者跳过零值字段，于是一条从 failed 重新建成
// ready 的镜像会永远挂着上一次的失败原因。
func (g *gitMirrorRepo) Save(ctx context.Context, mirror *git_entity.GitMirror) error {
	return db.Ctx(ctx).Save(mirror).Error
}

func (g *gitMirrorRepo) Touch(ctx context.Context, id int64, at int64) error {
	return db.Ctx(ctx).Model(&git_entity.GitMirror{}).Where("id=?", id).
		UpdateColumn("last_access_at", at).Error
}

func (g *gitMirrorRepo) Find(ctx context.Context, id int64) (*git_entity.GitMirror, error) {
	ret := &git_entity.GitMirror{}
	if err := db.Ctx(ctx).Where("id=?", id).First(ret).Error; err != nil {
		if db.RecordNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return ret, nil
}

func (g *gitMirrorRepo) Delete(ctx context.Context, id int64) error {
	return db.Ctx(ctx).Where("id=?", id).Delete(&git_entity.GitMirror{}).Error
}

func (g *gitMirrorRepo) List(ctx context.Context) ([]*git_entity.GitMirror, error) {
	list := make([]*git_entity.GitMirror, 0)
	if err := db.Ctx(ctx).Order("last_access_at desc").Find(&list).Error; err != nil {
		return nil, err
	}
	return list, nil
}

func (g *gitMirrorRepo) TotalSize(ctx context.Context) (int64, error) {
	var total int64
	// COALESCE 不能省：一张空表（或者没有 ready 记录）上的 SUM 是 NULL，
	// 扫进 int64 会报错，而「还没有任何一份建成的镜像」是正常状态。
	if err := db.Ctx(ctx).Model(&git_entity.GitMirror{}).
		Where("state=?", git_entity.MirrorReady).
		Select("COALESCE(SUM(size_bytes), 0)").Scan(&total).Error; err != nil {
		return 0, err
	}
	return total, nil
}

func (g *gitMirrorRepo) EvictCandidates(ctx context.Context, limit int) ([]*git_entity.GitMirror, error) {
	list := make([]*git_entity.GitMirror, 0, limit)
	// 只挑 ready 的：pending 还没落盘，failed/rejected 早已经把半成品清掉，
	// 淘汰它们既腾不出空间，也会把一条正在重建的记录踩掉。
	if err := db.Ctx(ctx).Where("state=?", git_entity.MirrorReady).
		Order("last_access_at asc").Limit(limit).Find(&list).Error; err != nil {
		return nil, err
	}
	return list, nil
}
