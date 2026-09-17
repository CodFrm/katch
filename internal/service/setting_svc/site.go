package setting_svc

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/CodFrm/katch/internal/api/site"
)

// SiteInfo 站点名片：名称与拉取命令里用的站点地址。
//
// 它读的就是 setting 表里那两项，没有任何进程内的副本——站长在设置页改完域名，
// 下一个请求给出的命令就换了主机名（决策 3/4 的那条理由）。
func (s *settingSvc) SiteInfo(ctx context.Context, _ *site.InfoRequest) (*site.InfoResponse, error) {
	rt, err := s.Runtime(ctx)
	if err != nil {
		// 名片读不出来就别编一个：默认站名配上空地址，会让界面照着一条拼不通的
		// 命令去教人拉取。
		return nil, err
	}
	return &site.InfoResponse{Name: rt.SiteName, BaseURL: siteBaseURL(rt.SiteDomain)}, nil
}

// BaseURL 只读取包管理器配置所需的站点域名。
func (s *settingSvc) BaseURL(ctx context.Context) (string, error) {
	raw, err := s.current(ctx, settingDefIndex[SiteDomainSetting])
	if err != nil {
		return "", err
	}
	var domain string
	if err := json.Unmarshal(raw, &domain); err != nil {
		return "", err
	}
	return siteBaseURL(domain), nil
}

// siteBaseURL 把设置里的站点域名变成命令里那一段地址。
//
// 设置项存的是域名（`mirror.example.com`），补上 https:// 才是使用者要敲的东西。
// 已经带了协议的值原样留下：内网部署把 katch 挂在 http 上是合法的，硬补一个
// https 会给出一条连不上的命令。尾部斜杠一律去掉，否则拼出来是 `//docker.io`。
func siteBaseURL(domain string) string {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return ""
	}
	if !strings.HasPrefix(domain, "http://") && !strings.HasPrefix(domain, "https://") {
		domain = "https://" + domain
	}
	return strings.TrimRight(domain, "/")
}
