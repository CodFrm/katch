package builtin

import (
	"context"
	"strconv"
	"strings"

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
	parts := strings.Split(strings.TrimPrefix(path, "/tile/"), "/")
	if !strings.HasPrefix(path, "/tile/") {
		return packageprofile.Representation{}
	}
	if len(parts) == 3 && decimal(parts[0]) && decimal(parts[1]) && decimal(parts[2]) {
		return packageprofile.Representation{Class: packageprofile.ClassImmutable}
	}
	if len(parts) == 4 && strings.HasSuffix(parts[2], ".p") &&
		decimal(parts[0]) && decimal(parts[1]) && decimal(strings.TrimSuffix(parts[2], ".p")) && decimal(parts[3]) {
		return packageprofile.Representation{Class: packageprofile.ClassMutable}
	}
	return packageprofile.Representation{}
}

func decimal(value string) bool {
	if value == "" {
		return false
	}
	_, err := strconv.ParseUint(value, 10, 64)
	return err == nil
}
