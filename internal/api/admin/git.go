package admin

import "github.com/cago-frame/cago/server/mux"

// GitMirrorItem 一份本地 git 镜像在管理接口上的表示。
//
// 这里给的是运维要看的全部：拉的是哪个仓库、此刻是什么状态、占多大、上一次
// 同步与上一次被访问是什么时候。State 取值见 git_entity 的 Mirror* 常量。
type GitMirrorItem struct {
	ID   int64  `json:"id"`
	Host string `json:"host"`
	Repo string `json:"repo"`
	// State 取值见 git_entity 的 Mirror* 常量：pending / ready / failed / rejected。
	State        string `json:"state"`
	SizeBytes    int64  `json:"size_bytes"`
	LastSyncAt   int64  `json:"last_sync_at"`
	LastAccessAt int64  `json:"last_access_at"`
	// LastError 最近一次失败的原因，成功时清空。
	LastError  string `json:"last_error"`
	Createtime int64  `json:"createtime"`
	Updatetime int64  `json:"updatetime"`
}

// ListGitMirrorsRequest 列出全部镜像记录。
//
// 不分页：规模有配额顶着，一台机器上建出来的仓库是几十条而不是几万条
// （同上游列表——ListUpstreamsRequest 的理由在这里同样成立）。
type ListGitMirrorsRequest struct {
	mux.Meta `path:"/admin/git/mirrors" method:"GET"`
}

// ListGitMirrorsResponse 镜像列表。
type ListGitMirrorsResponse struct {
	List []*GitMirrorItem `json:"list"`
}

// DeleteGitMirrorRequest 删除一条镜像：盘上的目录与库记录都会被清掉，下一次
// 拉取因此重新走穿透并在后台重建。
type DeleteGitMirrorRequest struct {
	mux.Meta `path:"/admin/git/mirrors/:id" method:"DELETE"`
	ID       int64 `uri:"id" binding:"required,gte=1" label:"镜像"`
}

// DeleteGitMirrorResponse 删除成功没有额外信息可返回。
type DeleteGitMirrorResponse struct{}
