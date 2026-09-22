package builtin

import (
	"context"
	"path"
	"strings"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
)

type aptProfile struct{}

func init() {
	packageprofile.MustRegister(aptProfile{})
}

func (aptProfile) Describe() packageprofile.Description {
	return packageprofile.Description{
		Profile: upstream_entity.PackageProfileAPT,
		Name:    "APT",
	}
}

func (aptProfile) Classify(request packageprofile.Request) packageprofile.Representation {
	segments := aptPathSegments(request.Path)
	for index, segment := range segments {
		if segment == "pool" && index+1 < len(segments) {
			return packageprofile.Representation{Class: packageprofile.ClassImmutable}
		}
	}

	for index, segment := range segments {
		if segment != "dists" || index+1 >= len(segments) {
			continue
		}
		for child := index + 2; child < len(segments); child++ {
			if segments[child] == "by-hash" && child+2 < len(segments) {
				return packageprofile.Representation{Class: packageprofile.ClassImmutable}
			}
		}
		return packageprofile.Representation{Class: packageprofile.ClassMutable}
	}

	return packageprofile.Representation{}
}

func (aptProfile) Transform(_ context.Context, request packageprofile.TransformRequest) (*packageprofile.TransformResult, error) {
	return &packageprofile.TransformResult{
		Body:        request.Body,
		ContentType: request.ContentType,
	}, nil
}

func (aptProfile) Companions() []packageprofile.Companion { return nil }

func (aptProfile) Guidance() packageprofile.Guidance {
	return packageprofile.Guidance{
		Clients: []string{"apt"},
		Configuration: []string{
			"deb [signed-by=/usr/share/keyrings/debian-archive-keyring.gpg] https://<katch>/<upstream>/<repository> <suite> <components>",
		},
		Constraints:     []string{"fixed_base"},
		RuntimeVerified: true,
	}
}

func aptPathSegments(value string) []string {
	cleaned := path.Clean("/" + strings.TrimSpace(value))
	if cleaned == "/" {
		return nil
	}
	return strings.Split(strings.TrimPrefix(cleaned, "/"), "/")
}
