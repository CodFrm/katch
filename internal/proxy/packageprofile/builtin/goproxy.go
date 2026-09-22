package builtin

import (
	"context"
	"strings"

	"golang.org/x/mod/sumdb/tlog"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
)

const goChecksumHost = "sum.golang.org"

var goChecksumCompanion = packageprofile.Companion{
	Host:      goChecksumHost,
	Profile:   upstream_entity.PackageProfileGoProxy,
	Transport: upstream_entity.ProtocolStatic,
}

type goProxyProfile struct{}

func init() {
	packageprofile.MustRegister(goProxyProfile{})
}

func (goProxyProfile) Describe() packageprofile.Description {
	return packageprofile.Description{
		Profile: upstream_entity.PackageProfileGoProxy,
		Name:    "goproxy",
	}
}

func (goProxyProfile) Classify(request packageprofile.Request) packageprofile.Representation {
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(request.Host), "."))
	path := request.Path
	if host == goChecksumHost {
		return classifyGoChecksumPath(path)
	}
	return classifyGoModulePath(path)
}

func (goProxyProfile) Transform(_ context.Context, request packageprofile.TransformRequest) (*packageprofile.TransformResult, error) {
	return &packageprofile.TransformResult{
		Body:        append([]byte(nil), request.Body...),
		ContentType: request.ContentType,
	}, nil
}

func (goProxyProfile) Companions() []packageprofile.Companion {
	return []packageprofile.Companion{goChecksumCompanion}
}

func (goProxyProfile) Guidance() packageprofile.Guidance {
	return packageprofile.Guidance{
		Clients: []string{"go"},
		Configuration: []string{
			"export GOPROXY=https://<katch>/<upstream>",
			"export GOSUMDB='sum.golang.org https://<katch>/sumdb/sum.golang.org'",
		},
		Constraints:     []string{"no_fallback"},
		RuntimeVerified: true,
	}
}

func classifyGoModulePath(path string) packageprofile.Representation {
	if len(path) > len("/@v/list") && strings.HasSuffix(path, "/@v/list") ||
		len(path) > len("/@latest") && strings.HasSuffix(path, "/@latest") {
		return packageprofile.Representation{Class: packageprofile.ClassMutable}
	}
	marker := strings.LastIndex(path, "/@v/")
	if marker <= 0 {
		return packageprofile.Representation{}
	}
	version := path[marker+len("/@v/"):]
	for _, suffix := range []string{".info", ".mod", ".zip"} {
		if strings.HasSuffix(version, suffix) && len(version) > len(suffix) {
			return packageprofile.Representation{Class: packageprofile.ClassImmutable}
		}
	}
	return packageprofile.Representation{}
}

func classifyGoChecksumPath(path string) packageprofile.Representation {
	if path == "/latest" {
		return packageprofile.Representation{Class: packageprofile.ClassMutable}
	}
	if strings.HasPrefix(path, "/lookup/") && len(path) > len("/lookup/") {
		return packageprofile.Representation{Class: packageprofile.ClassImmutable}
	}
	if !strings.HasPrefix(path, "/tile/") {
		return packageprofile.Representation{}
	}
	// 路径格式交给 Go 自己的 tlog 解析：tile 索引 ≥1000 时按三位一组编码、除最后一组外
	// 都带 x 前缀（/tile/8/0/x251/154），另有 data 层。真实的 sum.golang.org 树很大，
	// 客户端取的 level-0 tile 几乎全是分组形式；只认三段路径时它们全被当成普通对象，
	// CDN 带来的 Age 一旦超过可变 TTL，刚存进来就已经过期。ParseTilePath 还会回环校验，
	// 只接受规范路径——只有规范路径才由树上的位置唯一确定内容。
	tile, err := tlog.ParseTilePath(strings.TrimPrefix(path, "/"))
	if err != nil {
		return packageprofile.Representation{}
	}
	// 满 tile 的内容由它在树里的位置唯一决定，树只增不改，所以永远不变；部分 tile 会随
	// 树长大被更宽的一版替换。
	if tile.W == 1<<uint(tile.H) {
		return packageprofile.Representation{Class: packageprofile.ClassImmutable}
	}
	return packageprofile.Representation{Class: packageprofile.ClassMutable}
}
