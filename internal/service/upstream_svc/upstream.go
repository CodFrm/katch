// Package upstream_svc 是上游的业务层。
package upstream_svc

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cago-frame/cago/pkg/i18n"

	"github.com/CodFrm/katch/internal/api/admin"
	api_upstream "github.com/CodFrm/katch/internal/api/upstream"
	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/pkg/code"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
	_ "github.com/CodFrm/katch/internal/proxy/packageprofile/builtin"
	"github.com/CodFrm/katch/internal/repository/rule_repo"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
	"github.com/CodFrm/katch/internal/service/setting_svc"
)

// UpstreamSvc 上游的业务操作。
type UpstreamSvc interface {
	// FindByHost 供代理层判定白名单用：不存在或已停用时返回 (nil, nil)，
	// 调用方无需再自己看 Enabled——「停用」和「没有这条记录」对外必须是同一件事。
	FindByHost(ctx context.Context, host string) (*upstream_entity.Upstream, error)
	List(ctx context.Context, req *admin.ListUpstreamsRequest) (*admin.ListUpstreamsResponse, error)
	// PackageProfiles 返回本构建实际注册的 profile，供界面选择。
	PackageProfiles() []api_upstream.PackageProfile
	// PublicList 供首页用：只给启用中的上游，且只给站点名片级的字段。
	// 过滤放在业务层而不是交给调用方：让每个调用方各筛一次，迟早有一个忘了筛。
	PublicList(ctx context.Context, req *api_upstream.ListRequest) (*api_upstream.ListResponse, error)
	// Save 新登记一条上游。
	Save(ctx context.Context, req *admin.SaveUpstreamRequest) (*admin.SaveUpstreamResponse, error)
	// Update 整条替换一条已存在的上游，含启停。不存在的 id 是 404 而不是新增：
	// 「这条记录必须已经存在」正是更新与新增之间的那点区别。
	Update(ctx context.Context, req *admin.UpdateUpstreamRequest) (*admin.UpdateUpstreamResponse, error)
	Delete(ctx context.Context, req *admin.DeleteUpstreamRequest) (*admin.DeleteUpstreamResponse, error)
}

type upstreamSvc struct{}

var defaultUpstream = &upstreamSvc{}

// Upstream 返回上游业务层。
func Upstream() UpstreamSvc {
	return defaultUpstream
}

func (u *upstreamSvc) FindByHost(ctx context.Context, host string) (*upstream_entity.Upstream, error) {
	upstream, err := upstream_repo.Upstream().FindByHost(ctx, host)
	if err != nil {
		return nil, err
	}
	if upstream == nil || !upstream.Enabled {
		return nil, nil
	}
	return upstream, nil
}

func (u *upstreamSvc) List(ctx context.Context, req *admin.ListUpstreamsRequest) (*admin.ListUpstreamsResponse, error) {
	list, err := upstream_repo.Upstream().List(ctx)
	if err != nil {
		return nil, err
	}
	baseURL, err := packageBaseURL(ctx, list, req.PreviewPackageProfile)
	if err != nil {
		return nil, err
	}
	resp := &admin.ListUpstreamsResponse{List: make([]*admin.UpstreamItem, 0, len(list))}
	for _, v := range list {
		item := toItem(v)
		item.PackageReadiness = packageReadiness(baseURL, list, v)
		resp.List = append(resp.List, item)
	}
	if req.PreviewPackageProfile != "" {
		candidate := &upstream_entity.Upstream{
			ID: req.PreviewID, Host: req.PreviewHost, Enabled: req.PreviewEnabled,
			Protocols:      upstream_entity.ProtocolSet(req.PreviewProtocols),
			PackageProfile: upstream_entity.NormalizePackageProfile(req.PreviewPackageProfile),
		}
		configured := make([]*upstream_entity.Upstream, 0, len(list)+1)
		for _, current := range list {
			if candidate.ID == 0 || current.ID != candidate.ID {
				configured = append(configured, current)
			}
		}
		configured = append(configured, candidate)
		resp.Preview = packageReadiness(baseURL, configured, candidate)
	}
	return resp, nil
}

func (u *upstreamSvc) PackageProfiles() []api_upstream.PackageProfile {
	descriptions := packageprofile.Descriptions()
	profiles := make([]api_upstream.PackageProfile, 0, len(descriptions)+1)
	profiles = append(profiles, api_upstream.PackageProfile{
		Profile: upstream_entity.PackageProfileNone,
		Name:    "None",
	})
	for _, description := range descriptions {
		profiles = append(profiles, api_upstream.PackageProfile{
			Profile: description.Profile,
			Name:    description.Name,
		})
	}
	return profiles
}

func (u *upstreamSvc) PublicList(ctx context.Context, _ *api_upstream.ListRequest) (*api_upstream.ListResponse, error) {
	list, err := upstream_repo.Upstream().List(ctx)
	if err != nil {
		return nil, err
	}
	baseURL, err := packageBaseURL(ctx, list, upstream_entity.PackageProfileNone)
	if err != nil {
		return nil, err
	}
	resp := &api_upstream.ListResponse{List: make([]*api_upstream.Item, 0, len(list))}
	for _, v := range list {
		if !v.Enabled {
			continue
		}
		resp.List = append(resp.List, &api_upstream.Item{
			Host:              v.Host,
			Protocols:         protocols(v),
			PackageProfile:    upstream_entity.NormalizePackageProfile(v.PackageProfile),
			PackageReadiness:  packageReadiness(baseURL, list, v),
			LibraryCompletion: v.LibraryCompletion,
		})
	}
	return resp, nil
}

func (u *upstreamSvc) Save(ctx context.Context, req *admin.SaveUpstreamRequest) (*admin.SaveUpstreamResponse, error) {
	id, err := u.write(ctx, 0, req.Spec())
	if err != nil {
		return nil, err
	}
	return &admin.SaveUpstreamResponse{ID: id}, nil
}

func (u *upstreamSvc) Update(ctx context.Context, req *admin.UpdateUpstreamRequest) (*admin.UpdateUpstreamResponse, error) {
	id, err := u.write(ctx, req.ID, req.Spec())
	if err != nil {
		return nil, err
	}
	return &admin.UpdateUpstreamResponse{ID: id}, nil
}

// write 落一条上游，id 为 0 时新增、否则整条替换那一条，返回它的 id。
//
// 新增与更新共用这一段而不是各写一份：两者的差别只有「要不要先把库里那条捞出来」，
// 余下的重名判定、默认策略兜底与字段映射一模一样，分成两份迟早会只改其中一份。
func (u *upstreamSvc) write(ctx context.Context, id int64, spec *admin.UpstreamSpec) (int64, error) {
	profile := upstream_entity.NormalizePackageProfile(spec.PackageProfile)
	if !profile.Valid() {
		return 0, i18n.NewError(ctx, code.SettingValueInvalid, "package_profile", "未知 profile")
	}
	if profile != upstream_entity.PackageProfileNone &&
		!upstream_entity.ProtocolSet(spec.Protocols).Has(upstream_entity.ProtocolStatic) {
		return 0, i18n.NewError(ctx, code.SettingValueInvalid,
			"package_profile", "非 none profile 必须启用 static 协议")
	}
	spec.PackageProfile = profile

	var savedID int64
	err := upstream_repo.RewriteConfig().Transaction(ctx, func(txCtx context.Context) error {
		var err error
		savedID, err = u.writeTx(txCtx, id, spec)
		return err
	})
	return savedID, err
}

func (u *upstreamSvc) writeTx(ctx context.Context, id int64, spec *admin.UpstreamSpec) (int64, error) {
	now := time.Now().Unix()
	upstream := &upstream_entity.Upstream{Createtime: now}
	var before *upstream_entity.Upstream
	if id != 0 {
		exist, err := upstream_repo.Upstream().Find(ctx, id)
		if err != nil {
			return 0, err
		}
		if exist == nil {
			return 0, i18n.NewNotFoundError(ctx, code.UpstreamNotFound)
		}
		copied := *exist
		copied.Protocols = append(upstream_entity.ProtocolSet(nil), exist.Protocols...)
		before = &copied
		// 以库里那条为底再覆盖字段，而不是拿一个空实体去 Save：后者会把
		// createtime 这类请求里没有的字段一起清零。
		upstream = exist
	}
	// host 是白名单的 key，重复注册必须挡住：同一个 host 有两条记录时，
	// 分发命中哪条取决于查询顺序，停用其中一条看起来会毫无效果。
	byHost, err := upstream_repo.Upstream().FindByHost(ctx, spec.Host)
	if err != nil {
		return 0, err
	}
	if byHost != nil && byHost.ID != id {
		return 0, i18n.NewError(ctx, code.UpstreamHostExists)
	}
	policy := spec.DefaultPolicy
	if policy == "" {
		policy = upstream_entity.PolicyAllowAll
	}
	upstream.Host = spec.Host
	upstream.Protocols = upstream_entity.ProtocolSet(spec.Protocols)
	upstream.PackageProfile = spec.PackageProfile
	upstream.Origin = spec.Origin
	// 整条替换，所以 enabled 原样照抄：这里任何一处「false 就不覆盖」的写法，
	// 都会让停用这个动作在库里什么也没发生。
	upstream.Enabled = spec.Enabled
	upstream.ImmutablePatterns = upstream_entity.PatternList(spec.ImmutablePatterns)
	upstream.MutableTTLSeconds = spec.MutableTTLSeconds
	upstream.DefaultPolicy = policy
	upstream.LibraryCompletion = spec.LibraryCompletion
	upstream.Note = spec.Note
	upstream.Updatetime = now
	if err := upstream_repo.Upstream().Save(ctx, upstream); err != nil {
		return 0, err
	}
	if rewriteUpstreamChanged(before, upstream) {
		if err := upstream_repo.RewriteConfig().AdvanceGeneration(ctx); err != nil {
			return 0, err
		}
	}
	return upstream.ID, nil
}

func rewriteUpstreamChanged(before, after *upstream_entity.Upstream) bool {
	if before == nil {
		return after.Enabled
	}
	return before.Host != after.Host || before.Enabled != after.Enabled ||
		upstream_entity.NormalizePackageProfile(before.PackageProfile) != after.PackageProfile ||
		!sameProtocols(before.Protocols, after.Protocols)
}

func sameProtocols(a, b upstream_entity.ProtocolSet) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, protocol := range a {
		counts[protocol]++
	}
	for _, protocol := range b {
		counts[protocol]--
		if counts[protocol] < 0 {
			return false
		}
	}
	return true
}

func (u *upstreamSvc) Delete(ctx context.Context, req *admin.DeleteUpstreamRequest) (*admin.DeleteUpstreamResponse, error) {
	var resp *admin.DeleteUpstreamResponse
	err := upstream_repo.RewriteConfig().Transaction(ctx, func(txCtx context.Context) error {
		exist, err := upstream_repo.Upstream().Find(txCtx, req.ID)
		if err != nil {
			return err
		}
		if exist == nil {
			return i18n.NewNotFoundError(txCtx, code.UpstreamNotFound)
		}
		if err := rule_repo.AccessRule().DeleteByUpstream(txCtx, req.ID); err != nil {
			return err
		}
		if err := upstream_repo.Upstream().Delete(txCtx, req.ID); err != nil {
			return err
		}
		if exist.Enabled {
			if err := upstream_repo.RewriteConfig().AdvanceGeneration(txCtx); err != nil {
				return err
			}
		}
		resp = &admin.DeleteUpstreamResponse{}
		return nil
	})
	return resp, err
}

func packageBaseURL(
	ctx context.Context,
	configured []*upstream_entity.Upstream,
	previewProfile upstream_entity.PackageProfile,
) (string, error) {
	if upstream_entity.NormalizePackageProfile(previewProfile) != upstream_entity.PackageProfileNone {
		return setting_svc.Setting().BaseURL(ctx)
	}
	for _, current := range configured {
		if current != nil &&
			upstream_entity.NormalizePackageProfile(current.PackageProfile) != upstream_entity.PackageProfileNone {
			return setting_svc.Setting().BaseURL(ctx)
		}
	}
	return "", nil
}

func packageReadiness(
	baseURL string,
	configured []*upstream_entity.Upstream,
	candidate *upstream_entity.Upstream,
) *api_upstream.PackageReadiness {
	profileName := upstream_entity.NormalizePackageProfile(candidate.PackageProfile)
	if profileName == upstream_entity.PackageProfileNone {
		return nil
	}
	readiness := &api_upstream.PackageReadiness{
		Ready:      true,
		Missing:    make([]string, 0, 3),
		Companions: make([]api_upstream.PackageCompanion, 0),
		Guidance:   packageGuidance(profileName, candidate.Host, baseURL),
	}
	profile, ok := packageprofile.Lookup(profileName)
	if !ok {
		readiness.Ready = false
		readiness.Missing = append(readiness.Missing, "profile")
		return readiness
	}
	if strings.TrimSpace(baseURL) == "" {
		readiness.Ready = false
		readiness.Missing = append(readiness.Missing, "site_domain")
	}
	if !candidate.Enabled {
		readiness.Ready = false
		readiness.Missing = append(readiness.Missing, "upstream_disabled")
	}
	if !candidate.Protocols.Has(upstream_entity.ProtocolStatic) {
		readiness.Ready = false
		readiness.Missing = append(readiness.Missing, "static_transport")
	}

	companions := profile.Companions()
	readiness.Companions = make([]api_upstream.PackageCompanion, 0, len(companions))
	for _, required := range companions {
		item := api_upstream.PackageCompanion{
			Host:           required.Host,
			Transport:      required.Transport,
			PackageProfile: upstream_entity.NormalizePackageProfile(required.Profile),
		}
		current := configuredUpstream(configured, required.Host)
		if strings.EqualFold(
			strings.TrimSuffix(strings.TrimSpace(candidate.Host), "."),
			strings.TrimSuffix(strings.TrimSpace(required.Host), "."),
		) {
			current = candidate
		}
		switch {
		case current == nil:
			item.Reason = "missing"
		case !current.Enabled:
			item.Reason = "disabled"
		case !current.Protocols.Has(required.Transport):
			item.Reason = "transport"
		case upstream_entity.NormalizePackageProfile(current.PackageProfile) != item.PackageProfile:
			item.Reason = "profile"
		default:
			item.Ready = true
		}
		if !item.Ready {
			readiness.Ready = false
		}
		readiness.Companions = append(readiness.Companions, item)
	}
	return readiness
}

func configuredUpstream(configured []*upstream_entity.Upstream, host string) *upstream_entity.Upstream {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	for _, current := range configured {
		if current == nil {
			continue
		}
		currentHost := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(current.Host), "."))
		if currentHost == host {
			return current
		}
	}
	return nil
}

func packageGuidance(
	profileName upstream_entity.PackageProfile,
	host string,
	baseURL string,
) api_upstream.PackageGuidance {
	guidance := api_upstream.PackageGuidance{
		Clients:         packageClients(profileName),
		Constraints:     packageConstraints(profileName, baseURL),
		RuntimeVerified: false,
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return guidance
	}
	prefix := baseURL + "/" + strings.Trim(strings.TrimSpace(host), "/")
	if profile, ok := packageprofile.Lookup(profileName); ok {
		declared := profile.Guidance()
		guidance.Configuration = expandDeclaredGuidance(declared.Configuration, prefix, baseURL, host)
	}

	switch profileName {
	case upstream_entity.PackageProfileNPM:
		guidance.Configuration = []string{
			"npm config set registry " + prefix + "/",
			"npm config set replace-registry-host always",
			"pnpm config set registry " + prefix + "/",
			"yarn config set registry " + prefix + "/",
			fmt.Sprintf("npmRegistryServer: %q", prefix+"/"),
			fmt.Sprintf("[install]\nregistry = %q", prefix+"/"),
		}
	case upstream_entity.PackageProfilePyPI:
		guidance.Configuration = []string{
			"pip install --index-url " + prefix + "/simple/ <package>",
			"uv pip install --index-url " + prefix + "/simple/ <package>",
			"poetry source add --priority=primary katch " + prefix + "/simple/",
		}
	case upstream_entity.PackageProfileGoProxy:
		guidance.Configuration = []string{
			"export GOPROXY=" + prefix,
			"export GOSUMDB='sum.golang.org " + baseURL + "/sumdb/sum.golang.org'",
		}
	case upstream_entity.PackageProfileMaven:
		guidance.Configuration = []string{
			fmt.Sprintf("<repository><id>katch</id><url>%s/</url></repository>", prefix),
			fmt.Sprintf("repositories { maven { url = uri(%q) } }", prefix+"/"),
			fmt.Sprintf("resolvers += %q at %q", "katch", prefix+"/"),
		}
	case upstream_entity.PackageProfileCargo:
		guidance.Configuration = []string{
			fmt.Sprintf("[source.crates-io]\nreplace-with = %q\n[source.katch]\nregistry = %q", "katch", "sparse+"+prefix+"/"),
		}
	case upstream_entity.PackageProfileNuGet:
		guidance.Configuration = []string{
			"dotnet nuget add source " + prefix + "/v3/index.json --name katch",
			"dotnet restore --source " + prefix + "/v3/index.json",
			"dotnet package search <term> --source " + prefix + "/v3/index.json",
		}
	case upstream_entity.PackageProfileRubyGems:
		guidance.Configuration = []string{
			"gem sources --add " + prefix + "/ --remove https://rubygems.org/",
			"bundle config set --global mirror.https://rubygems.org " + prefix + "/",
		}
	case upstream_entity.PackageProfileAPT:
		guidance.Configuration = []string{
			"deb [signed-by=/usr/share/keyrings/debian-archive-keyring.gpg] " + prefix + "/<repository> <suite> <components>",
		}
	case upstream_entity.PackageProfileRPM:
		guidance.Configuration = []string{
			"baseurl=" + prefix + "/<repository-path>/",
			"mirrorlist=",
			"metalink=",
		}
	case upstream_entity.PackageProfileAPK:
		guidance.Configuration = []string{prefix + "/<release>/<repository>/<architecture>"}
	case upstream_entity.PackageProfileComposer:
		guidance.Configuration = []string{
			"composer config --global repos.packagist composer " + prefix,
			"composer install --prefer-dist",
		}
	case upstream_entity.PackageProfileHomebrew:
		guidance.Configuration = []string{
			"export HOMEBREW_API_DOMAIN=" + prefix + "/api",
			"export HOMEBREW_ARTIFACT_DOMAIN=" + baseURL + "/v2/ghcr.io",
			"export HOMEBREW_ARTIFACT_DOMAIN_NO_FALLBACK=1",
			"export HOMEBREW_BREW_GIT_REMOTE=" + baseURL + "/github.com/Homebrew/brew.git",
			"export HOMEBREW_CORE_GIT_REMOTE=" + baseURL + "/github.com/Homebrew/homebrew-core.git",
		}
	}
	return guidance
}

func expandDeclaredGuidance(lines []string, prefix, baseURL, host string) []string {
	expanded := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.ReplaceAll(line, "https://<katch>/<upstream>", prefix)
		line = strings.ReplaceAll(line, "https://<katch>/<rpm-host>", prefix)
		line = strings.ReplaceAll(line, "https://<katch>/"+host, prefix)
		line = strings.ReplaceAll(line, "https://<katch>", baseURL)
		line = strings.ReplaceAll(line, "<site-base>", baseURL)
		expanded = append(expanded, line)
	}
	return expanded
}

func packageClients(profile upstream_entity.PackageProfile) []string {
	clients := map[upstream_entity.PackageProfile][]string{
		upstream_entity.PackageProfileNPM:      {"npm", "pnpm", "yarn_classic", "yarn_berry", "bun"},
		upstream_entity.PackageProfilePyPI:     {"pip", "uv", "poetry"},
		upstream_entity.PackageProfileGoProxy:  {"go"},
		upstream_entity.PackageProfileMaven:    {"maven", "gradle", "sbt"},
		upstream_entity.PackageProfileCargo:    {"cargo"},
		upstream_entity.PackageProfileNuGet:    {"dotnet", "nuget"},
		upstream_entity.PackageProfileRubyGems: {"gem", "bundler"},
		upstream_entity.PackageProfileAPT:      {"apt"},
		upstream_entity.PackageProfileRPM:      {"dnf", "yum"},
		upstream_entity.PackageProfileAPK:      {"apk"},
		upstream_entity.PackageProfileComposer: {"composer"},
		upstream_entity.PackageProfileHomebrew: {"homebrew"},
	}
	return append([]string(nil), clients[profile]...)
}

func packageConstraints(profile upstream_entity.PackageProfile, baseURL string) []string {
	constraints := map[upstream_entity.PackageProfile][]string{
		upstream_entity.PackageProfileNPM:      {"trailing_slash", "old_lockfile"},
		upstream_entity.PackageProfilePyPI:     {"trailing_slash"},
		upstream_entity.PackageProfileGoProxy:  {"no_fallback"},
		upstream_entity.PackageProfileMaven:    {"fixed_base"},
		upstream_entity.PackageProfileCargo:    {"trailing_slash", "sparse_only"},
		upstream_entity.PackageProfileNuGet:    {"read_only"},
		upstream_entity.PackageProfileRubyGems: {"trailing_slash"},
		upstream_entity.PackageProfileAPT:      {"fixed_base"},
		upstream_entity.PackageProfileRPM:      {"fixed_base", "no_dynamic_mirrors"},
		upstream_entity.PackageProfileAPK:      {"fixed_base"},
		upstream_entity.PackageProfileComposer: {"dist_only", "old_lockfile"},
		upstream_entity.PackageProfileHomebrew: {"no_fallback", "bottles_only"},
	}
	out := append([]string(nil), constraints[profile]...)
	lowerBase := strings.ToLower(strings.TrimSpace(baseURL))
	if strings.HasPrefix(lowerBase, "http://localhost") || strings.HasPrefix(lowerBase, "http://127.0.0.1") {
		out = append(out, "insecure_localhost")
	}
	return out
}

// protocols 把协议集合拷成一份普通 []string，不把存储形态泄漏给调用方。
//
// 拷贝而不是转换：直接转换出去的切片与实体共享底层数组，调用方改一下就把
// 那条上游的判定改了。
func protocols(v *upstream_entity.Upstream) []string {
	out := make([]string, 0, len(v.Protocols))
	out = append(out, v.Protocols...)
	return out
}

// toItem 把实体映射成对外结构。模式列表转成 []string，不把存储形态泄漏出去。
func toItem(v *upstream_entity.Upstream) *admin.UpstreamItem {
	patterns := make([]string, 0, len(v.ImmutablePatterns))
	patterns = append(patterns, v.ImmutablePatterns...)
	return &admin.UpstreamItem{
		ID:                v.ID,
		Host:              v.Host,
		Protocols:         protocols(v),
		PackageProfile:    upstream_entity.NormalizePackageProfile(v.PackageProfile),
		Origin:            v.Origin,
		Enabled:           v.Enabled,
		ImmutablePatterns: patterns,
		MutableTTLSeconds: v.MutableTTLSeconds,
		DefaultPolicy:     v.DefaultPolicy,
		LibraryCompletion: v.LibraryCompletion,
		Note:              v.Note,
		Createtime:        v.Createtime,
		Updatetime:        v.Updatetime,
	}
}
