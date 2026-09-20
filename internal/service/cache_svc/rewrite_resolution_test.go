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
