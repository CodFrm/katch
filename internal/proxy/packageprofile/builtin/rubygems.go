package builtin

import (
	"context"
	"strings"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
)

type rubyGemsProfile struct{}

func init() {
	packageprofile.MustRegister(rubyGemsProfile{})
}

func (rubyGemsProfile) Describe() packageprofile.Description {
	return packageprofile.Description{
		Profile: upstream_entity.PackageProfileRubyGems,
		Name:    "RubyGems",
	}
}

func (rubyGemsProfile) Classify(request packageprofile.Request) packageprofile.Representation {
	if rubyGemsArtifactPath(request.Path) {
		return packageprofile.Representation{Class: packageprofile.ClassImmutable}
	}
	if rubyGemsIndexPath(request.Path) {
		return packageprofile.Representation{Class: packageprofile.ClassMutable}
	}
	return packageprofile.Representation{}
}

func (rubyGemsProfile) Transform(_ context.Context, request packageprofile.TransformRequest) (*packageprofile.TransformResult, error) {
	return &packageprofile.TransformResult{
		Body:        request.Body,
		ContentType: request.ContentType,
	}, nil
}

func (rubyGemsProfile) Companions() []packageprofile.Companion { return nil }

func (rubyGemsProfile) Guidance() packageprofile.Guidance {
	return packageprofile.Guidance{
		Client: "RubyGems / Bundler",
		Configuration: []string{
			"gem sources --add https://<katch>/rubygems.org/ --remove https://rubygems.org/",
			"bundle config set --global mirror.https://rubygems.org https://<katch>/rubygems.org/",
		},
	}
}

func rubyGemsArtifactPath(path string) bool {
	const prefix = "/gems/"
	if !strings.HasPrefix(path, prefix) || strings.Contains(path[len(prefix):], "/") {
		return false
	}
	name := strings.TrimSuffix(path[len(prefix):], ".gem")
	if name == path[len(prefix):] {
		return false
	}
	for index := 0; index+2 < len(name); index++ {
		if name[index] == '-' && name[index+1] >= '0' && name[index+1] <= '9' {
			return true
		}
	}
	return false
}

func rubyGemsIndexPath(path string) bool {
	switch path {
	case "/versions", "/names", "/specs.4.8.gz", "/latest_specs.4.8.gz", "/prerelease_specs.4.8.gz", "/api/v1/dependencies":
		return true
	}
	if strings.HasPrefix(path, "/info/") {
		name := strings.TrimPrefix(path, "/info/")
		return name != "" && !strings.Contains(name, "/")
	}
	if strings.HasPrefix(path, "/quick/Marshal.4.8/") {
		name := strings.TrimPrefix(path, "/quick/Marshal.4.8/")
		return name != "" && !strings.Contains(name, "/") && strings.HasSuffix(name, ".gemspec.rz")
	}
	return false
}
