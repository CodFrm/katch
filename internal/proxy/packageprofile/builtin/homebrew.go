package builtin

import (
	"context"
	"strings"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
)

type homebrewProfile struct{}

func init() {
	packageprofile.MustRegister(homebrewProfile{})
}

func (homebrewProfile) Describe() packageprofile.Description {
	return packageprofile.Description{
		Profile: upstream_entity.PackageProfileHomebrew,
		Name:    "homebrew",
	}
}

func (homebrewProfile) Classify(request packageprofile.Request) packageprofile.Representation {
	if strings.HasPrefix(request.Path, "/api/") && len(request.Path) > len("/api/") {
		return packageprofile.Representation{Class: packageprofile.ClassMutable}
	}
	return packageprofile.Representation{}
}

func (homebrewProfile) Transform(_ context.Context, request packageprofile.TransformRequest) (*packageprofile.TransformResult, error) {
	return &packageprofile.TransformResult{
		Body:        append([]byte(nil), request.Body...),
		ContentType: request.ContentType,
	}, nil
}

func (homebrewProfile) Companions() []packageprofile.Companion {
	return []packageprofile.Companion{
		{
			Host:      "ghcr.io",
			Profile:   upstream_entity.PackageProfileNone,
			Transport: upstream_entity.ProtocolRegistry,
		},
		{
			Host:      "pkg-containers.githubusercontent.com",
			Profile:   upstream_entity.PackageProfileNone,
			Transport: upstream_entity.ProtocolStatic,
		},
		{
			Host:      "github.com",
			Profile:   upstream_entity.PackageProfileNone,
			Transport: upstream_entity.ProtocolGit,
		},
	}
}

func (homebrewProfile) Guidance() packageprofile.Guidance {
	return packageprofile.Guidance{
		Client: "Homebrew",
		Configuration: []string{
			"HOMEBREW_API_DOMAIN=https://<katch>/formulae.brew.sh/api",
			"HOMEBREW_ARTIFACT_DOMAIN=https://<katch>/registry/ghcr.io",
			"HOMEBREW_ARTIFACT_DOMAIN_NO_FALLBACK=1",
			"HOMEBREW_BREW_GIT_REMOTE=https://<katch>/github.com/Homebrew/brew.git",
			"HOMEBREW_CORE_GIT_REMOTE=https://<katch>/github.com/Homebrew/homebrew-core.git",
		},
	}
}
