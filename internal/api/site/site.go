// Package site 定义站点名片接口的请求与响应结构。
//
// 这个接口不要密钥，也不受「首页是否公开上游列表」那个开关管：站点名称是页面标题，
// 站点地址是拉取命令里的那一段主机名——把它们藏起来，界面连自己叫什么、该让人敲
// 什么命令都说不出来，而这两样并不描述这台镜像站代理了什么。
package site

import (
	"github.com/cago-frame/cago/server/mux"

	api_upstream "github.com/CodFrm/katch/internal/api/upstream"
)

// InfoRequest 查站点名片。
type InfoRequest struct {
	mux.Meta `path:"/site" method:"GET"`
}

// InfoResponse 站点名片。
type InfoResponse struct {
	// Name 站点名称，界面标题与首页那张名片用。
	Name string `json:"name"`
	// BaseURL 拉取命令里的站点地址，形如 https://katch.dev，不带尾部斜杠。
	//
	// 拉取助手拼的每一条命令都以它开头（`docker pull <base_url>/docker.io/...`），
	// 所以站长在设置里改掉站点域名之后，下一个请求拿到的命令就换了主机名。
	//
	// 站长没配站点域名时是空串，而**不是**服务端替他猜一个：katch 很可能被反代在
	// 另一个域名后面，进程自己看见的监听地址不是使用者敲得通的那个。空串时由调用方
	// 用它自己访问到的地址兜底——那个地址至少是确实通的。
	BaseURL string `json:"base_url"`
	// PackageProfiles 是本构建实际注册的 profile；界面据此提供选择，不另抄一份枚举。
	PackageProfiles []api_upstream.PackageProfile `json:"package_profiles"`
}
