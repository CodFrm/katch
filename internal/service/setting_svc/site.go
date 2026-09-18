package setting_svc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
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

// canonicalSiteDomain 校验并规范化站点地址，空值是合法的「还没配」。
//
// 这一项不是随便一段文本：它会被拼进给使用者的拉取命令、profile 改写出来的
// metadata 地址，以及「这条上游准备好了没有」的判据。带查询串的值会让拼在后面的
// 上游路径掉进查询串里，带用户名密码的值会把凭据印进公开页面，非 http(s) 的值
// 拼出来的命令根本不是一个能拉取的地址——所以坏值要在写进库之前就被挡住，而不是
// 等到界面上印出一条连不通的命令。
//
// 允许两种写法：光的主机名（`mirror.example`，按既有语义补 https）和完整的
// http(s) 地址（内网部署挂在 http 上、带端口、带一段基路径都是合法的）。
func canonicalSiteDomain(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if strings.ContainsAny(value, " \t\r\n#") {
		return "", fmt.Errorf("不能包含空白字符或 #")
	}
	candidate := value
	lower := strings.ToLower(value)
	switch {
	case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"):
	case strings.Contains(value, "://"):
		return "", fmt.Errorf("只支持 http 或 https 地址")
	default:
		candidate = "https://" + value
	}
	u, err := url.Parse(candidate)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("应为主机名或 http(s) 地址")
	}
	if u.User != nil {
		return "", fmt.Errorf("不能带用户名或密码")
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("缺少主机名")
	}
	if port := u.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", fmt.Errorf("端口应在 1 到 65535 之间")
		}
	}
	if u.RawQuery != "" || u.ForceQuery {
		return "", fmt.Errorf("不能带查询串")
	}
	if u.EscapedPath() != u.Path {
		return "", fmt.Errorf("路径里有需要转义的字符")
	}
	if candidate != value {
		// 光主机名的写法原样留着：设置页上显示的还是站长填的那一行。
		return strings.TrimRight(value, "/"), nil
	}
	// 自带协议的写法按解析结果重拼：`HTTP://` 这种大写协议留着原样，会让下面
	// 那个大小写敏感的前缀判断认不出它，再补一次 https:// 拼出双协议的地址。
	return u.Scheme + "://" + u.Host + strings.TrimRight(u.EscapedPath(), "/"), nil
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
