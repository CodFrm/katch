package cache_svc

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/destination"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
	"github.com/CodFrm/katch/internal/service/proxy_svc"
)

// countingResolver 记下每个主机被解析了多少次，其余行为与允许通过的解析器一致。
//
// 带用户信息的目标照生产的解析器拒掉（destination.Resolve 的第一道判断就是
// target.User != nil）：这一条是下面那条用例的前提，假解析器在这一点上失真的话，
// 用例就会在「拒绝结论被记到主机名下」这个缺陷存在时照样绿。
type countingResolver struct {
	mu     sync.Mutex
	counts map[string]int
}

func (r *countingResolver) Resolve(
	_ context.Context, target *url.URL, _ destination.DestinationRequirement,
) (*destination.ResolvedTarget, error) {
	host := strings.ToLower(strings.TrimSuffix(target.Hostname(), "."))
	r.mu.Lock()
	if r.counts == nil {
		r.counts = map[string]int{}
	}
	r.counts[host]++
	r.mu.Unlock()
	if target.User != nil {
		return nil, destination.ErrDestinationNotAllowed
	}
	cloned := *target
	return &destination.ResolvedTarget{
		URL: &cloned, Authority: target.Host, Host: target.Host,
		ServerName: host, DialAddress: host + ":443",
	}, nil
}

func (r *countingResolver) count(host string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts[host]
}

// 一份元数据里的成千上万个构件 URL 都指向同一个伙伴主机，改写不能因此变成一次链接一次解析。
//
// 真实规模上这不是慢一点：PyPI 的 numpy 索引有 4232 个链接，按一次链接一次 DNS 查询算，
// 一个索引页要几十分钟才回得来，镜像等于不可用。解析的判据只看 scheme/主机/端口，
// 所以同一份正文里重复的主机记一次就够。
func TestGet_MetadataRewriteResolvesEachCompanionHostOnce(t *testing.T) {
	const links = 50
	companions := []string{"cdn.example.com", "files.example.com"}
	resolver := &countingResolver{}
	profile := testProfile{
		description: packageprofile.Description{Profile: upstream_entity.PackageProfileNPM, Name: "test"},
		representation: packageprofile.Representation{
			Class: packageprofile.ClassMutable, Transform: true, MediaTypes: []string{"application/json"},
		},
		transform: func(ctx context.Context, in packageprofile.TransformRequest) (*packageprofile.TransformResult, error) {
			for _, host := range companions {
				for index := 0; index < links; index++ {
					artifact, err := url.Parse(fmt.Sprintf("https://%s/pkg/-/pkg-%d.tgz", host, index))
					if err != nil {
						return nil, err
					}
					if _, err := in.RewriteURL(ctx, artifact, packageprofile.Companion{
						Host: host, Profile: upstream_entity.PackageProfileNPM,
						Transport: upstream_entity.ProtocolStatic,
					}); err != nil {
						return nil, err
					}
				}
			}
			return &packageprofile.TransformResult{Body: []byte(`{}`)}, nil
		},
	}
	up := staticUpstream("metadata.example.com")
	up.PackageProfile = upstream_entity.PackageProfileNPM
	options := transformingOptions(t, profile, 1)
	options.DestinationResolver = resolver
	snapshot := options.RewriteConfig.(fixedRewriteSource).snapshot
	for _, host := range companions {
		snapshot.Upstreams[host] = proxy_svc.RewriteUpstream{
			Profile:    upstream_entity.PackageProfileNPM,
			Transports: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
		}
	}
	o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	})
	svc, _, _ := setupSvc(t, o, up, options)

	drain := func(path string) {
		t.Helper()
		body, meta, err := svc.Get(context.Background(), target(up.Host, path))
		if err != nil {
			t.Fatal(err)
		}
		payload, readErr := io.ReadAll(body)
		closeErr := body.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read/close = %v/%v", readErr, closeErr)
		}
		if meta.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d, body = %q", path, meta.StatusCode, payload)
		}
	}

	drain("/first")
	for _, host := range companions {
		if got := resolver.count(host); got != 1 {
			t.Errorf("%s resolved %d times for one metadata body, want 1", host, got)
		}
	}

	// 记忆化只在一份正文之内有效：跨请求缓存解析结果会把回源那一步的 DNS 重绑定防护也一起放掉。
	drain("/second")
	for _, host := range companions {
		if got := resolver.count(host); got != 2 {
			t.Errorf("%s resolved %d times across two metadata bodies, want 2", host, got)
		}
	}
}

// TestGet_MetadataRewriteIgnoresTheMetadataPort 元数据里的端口既不改结果，也不多花一次解析。
//
// 改写出来的地址不带端口（拼的是 SiteBaseURL + "/" + 主机 + 路径），客户端回头来取时
// 走的是这个上游注册的 origin——自建 Nexus 那种挂在 8081 上的上游正是靠这一点工作的。
// 既然端口不参与任何判断，它就不该进记忆化的键：进了的话，一份写着几万个不同端口的
// 正文就能把按主机记一次省下来的 DNS 查询全部要回去。
func TestGet_MetadataRewriteIgnoresTheMetadataPort(t *testing.T) {
	const host = "cdn.example.com"
	resolver := &countingResolver{}
	var mapped []string
	profile := testProfile{
		description: packageprofile.Description{Profile: upstream_entity.PackageProfileNPM, Name: "test"},
		representation: packageprofile.Representation{
			Class: packageprofile.ClassMutable, Transform: true, MediaTypes: []string{"application/json"},
		},
		transform: func(ctx context.Context, in packageprofile.TransformRequest) (*packageprofile.TransformResult, error) {
			mapped = nil
			for _, raw := range []string{
				"https://" + host + ":8443/pkg/-/a.tgz",
				"https://" + host + "/pkg/-/b.tgz",
				"https://" + host + ":443/pkg/-/c.tgz",
			} {
				artifact, err := url.Parse(raw)
				if err != nil {
					return nil, err
				}
				rewritten, err := in.RewriteURL(ctx, artifact, packageprofile.Companion{
					Host: host, Profile: upstream_entity.PackageProfileNPM,
					Transport: upstream_entity.ProtocolStatic,
				})
				if err != nil {
					return nil, fmt.Errorf("%s 改写失败：%w", raw, err)
				}
				mapped = append(mapped, rewritten.String())
			}
			return &packageprofile.TransformResult{Body: []byte(`{}`)}, nil
		},
	}
	up := staticUpstream("metadata.example.com")
	up.PackageProfile = upstream_entity.PackageProfileNPM
	options := transformingOptions(t, profile, 1)
	options.DestinationResolver = resolver
	options.RewriteConfig.(fixedRewriteSource).snapshot.Upstreams[host] = proxy_svc.RewriteUpstream{
		Profile:    upstream_entity.PackageProfileNPM,
		Transports: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
	}
	o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	})
	svc, _, _ := setupSvc(t, o, up, options)

	body, meta, err := svc.Get(context.Background(), target(up.Host, "/pkg"))
	if err != nil {
		t.Fatal(err)
	}
	payload, readErr := io.ReadAll(body)
	closeErr := body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read/close = %v/%v", readErr, closeErr)
	}
	if meta.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %q", meta.StatusCode, payload)
	}
	want := []string{
		"https://katch.example.com/" + host + "/pkg/-/a.tgz",
		"https://katch.example.com/" + host + "/pkg/-/b.tgz",
		"https://katch.example.com/" + host + "/pkg/-/c.tgz",
	}
	if len(mapped) != len(want) {
		t.Fatalf("改写出 %d 条，要的是 %d 条：%v", len(mapped), len(want), mapped)
	}
	for index := range want {
		if mapped[index] != want[index] {
			t.Errorf("第 %d 条改写成 %q，要的是 %q", index+1, mapped[index], want[index])
		}
	}
	if got := resolver.count(host); got != 1 {
		t.Errorf("%s 解析了 %d 次，要的是 1 次——端口进了记忆化的键", host, got)
	}
}

// TestGet_MetadataRewriteDoesNotPoisonAHostWithARejectedURL 一条被拒的构件 URL 不能
// 把同主机的其它链接一起拖下水。
//
// 解析结论是按主机记住的，而带用户信息的目标在解析器那里是无条件拒绝。两件事凑在一起
// 时，元数据里任何一条 https://someone@host/... 都会把 host 的结论钉成「拒绝」，于是同
// 一份正文里后面那些干净的链接全部改写失败，整份元数据被判 ErrInvalidMetadata——一个
// 上游随手放进来的用户信息就能让这个主机的所有包在镜像上消失。所以带用户信息的目标
// 一律现算，不进也不读那份记忆。
func TestGet_MetadataRewriteDoesNotPoisonAHostWithARejectedURL(t *testing.T) {
	const host = "cdn.example.com"
	resolver := &countingResolver{}
	profile := testProfile{
		description: packageprofile.Description{Profile: upstream_entity.PackageProfileNPM, Name: "test"},
		representation: packageprofile.Representation{
			Class: packageprofile.ClassMutable, Transform: true, MediaTypes: []string{"application/json"},
		},
		transform: func(ctx context.Context, in packageprofile.TransformRequest) (*packageprofile.TransformResult, error) {
			companion := packageprofile.Companion{
				Host: host, Profile: upstream_entity.PackageProfileNPM,
				Transport: upstream_entity.ProtocolStatic,
			}
			dirty, err := url.Parse("https://someone@" + host + "/pkg/-/dirty.tgz")
			if err != nil {
				return nil, err
			}
			if _, err := in.RewriteURL(ctx, dirty, companion); err == nil {
				return nil, fmt.Errorf("带用户信息的构件 URL 被接受了")
			}
			clean, err := url.Parse("https://" + host + "/pkg/-/clean.tgz")
			if err != nil {
				return nil, err
			}
			if _, err := in.RewriteURL(ctx, clean, companion); err != nil {
				return nil, fmt.Errorf("同一主机的干净 URL 被前一条的拒绝结论传染：%w", err)
			}
			return &packageprofile.TransformResult{Body: []byte(`{}`)}, nil
		},
	}
	up := staticUpstream("metadata.example.com")
	up.PackageProfile = upstream_entity.PackageProfileNPM
	options := transformingOptions(t, profile, 1)
	options.DestinationResolver = resolver
	options.RewriteConfig.(fixedRewriteSource).snapshot.Upstreams[host] = proxy_svc.RewriteUpstream{
		Profile:    upstream_entity.PackageProfileNPM,
		Transports: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
	}
	o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	})
	svc, _, _ := setupSvc(t, o, up, options)

	body, meta, err := svc.Get(context.Background(), target(up.Host, "/pkg"))
	if err != nil {
		t.Fatal(err)
	}
	payload, readErr := io.ReadAll(body)
	closeErr := body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read/close = %v/%v", readErr, closeErr)
	}
	if meta.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %q", meta.StatusCode, payload)
	}
}

// TestGet_MetadataRewriteRejectsInvalidPortsWhateverTheirPosition 非法端口不管排在第几条都被拒。
//
// 端口不进记忆化的键（见 TestGet_MetadataRewriteIgnoresTheMetadataPort），可端口是否
// 合法是解析器会判的一项：https://h:0/ 与 https://h:99999/ 在 destination 那里都是
// 「invalid port」。只靠记忆时，这样一条链接排在同主机的干净链接后面就会命中「已放行」
// 被照常改写，排在前面则会被拒——同一份元数据的结论取决于链接的先后。所以端口先按
// destination 自己的规则单独判一次，再进记忆。
func TestGet_MetadataRewriteRejectsInvalidPortsWhateverTheirPosition(t *testing.T) {
	const host = "cdn.example.com"
	profile := testProfile{
		description: packageprofile.Description{Profile: upstream_entity.PackageProfileNPM, Name: "test"},
		representation: packageprofile.Representation{
			Class: packageprofile.ClassMutable, Transform: true, MediaTypes: []string{"application/json"},
		},
		transform: func(ctx context.Context, in packageprofile.TransformRequest) (*packageprofile.TransformResult, error) {
			companion := packageprofile.Companion{
				Host: host, Profile: upstream_entity.PackageProfileNPM,
				Transport: upstream_entity.ProtocolStatic,
			}
			clean, err := url.Parse("https://" + host + "/pkg/-/clean.tgz")
			if err != nil {
				return nil, err
			}
			if _, err := in.RewriteURL(ctx, clean, companion); err != nil {
				return nil, fmt.Errorf("干净的构件 URL 被拒了：%w", err)
			}
			for _, raw := range []string{
				"https://" + host + ":0/pkg/-/zero.tgz",
				"https://" + host + ":99999/pkg/-/overflow.tgz",
			} {
				bad, err := url.Parse(raw)
				if err != nil {
					return nil, err
				}
				if _, err := in.RewriteURL(ctx, bad, companion); err == nil {
					return nil, fmt.Errorf("%s 排在干净链接后面，被照常改写了", raw)
				}
			}
			return &packageprofile.TransformResult{Body: []byte(`{}`)}, nil
		},
	}
	up := staticUpstream("metadata.example.com")
	up.PackageProfile = upstream_entity.PackageProfileNPM
	options := transformingOptions(t, profile, 1)
	options.DestinationResolver = &countingResolver{}
	options.RewriteConfig.(fixedRewriteSource).snapshot.Upstreams[host] = proxy_svc.RewriteUpstream{
		Profile:    upstream_entity.PackageProfileNPM,
		Transports: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
	}
	o := newOrigin(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	})
	svc, _, _ := setupSvc(t, o, up, options)

	body, meta, err := svc.Get(context.Background(), target(up.Host, "/pkg"))
	if err != nil {
		t.Fatal(err)
	}
	payload, readErr := io.ReadAll(body)
	closeErr := body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read/close = %v/%v", readErr, closeErr)
	}
	if meta.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %q", meta.StatusCode, payload)
	}
}
