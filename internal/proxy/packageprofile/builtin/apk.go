package builtin

import (
	"context"
	"regexp"
	"strings"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
)

var apkPackagePath = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9+_.-]*[A-Za-z0-9+_])?-[0-9][A-Za-z0-9+._]*-r(?:0|[1-9][0-9]*)\.apk$`)

type apkProfile struct{}

func init() {
	packageprofile.MustRegister(apkProfile{})
}

func (apkProfile) Describe() packageprofile.Description {
	return packageprofile.Description{
		Profile: upstream_entity.PackageProfileAPK,
		Name:    "Alpine APK",
	}
}

func (apkProfile) Classify(request packageprofile.Request) packageprofile.Representation {
	name := request.Path
	if slash := strings.LastIndexByte(name, '/'); slash >= 0 {
		name = name[slash+1:]
	}

	switch {
	case name == "APKINDEX" || name == "APKINDEX.tar.gz":
		return packageprofile.Representation{Class: packageprofile.ClassMutable}
	case apkPackagePath.MatchString(name):
		return packageprofile.Representation{Class: packageprofile.ClassImmutable}
	default:
		return packageprofile.Representation{}
	}
}

func (apkProfile) Transform(_ context.Context, request packageprofile.TransformRequest) (*packageprofile.TransformResult, error) {
	return &packageprofile.TransformResult{
		Body:        request.Body,
		ContentType: request.ContentType,
	}, nil
}

func (apkProfile) Companions() []packageprofile.Companion { return nil }

func (apkProfile) Guidance() packageprofile.Guidance {
	return packageprofile.Guidance{
		Client:        "apk",
		Configuration: []string{"repository"},
	}
}

var _ packageprofile.Profile = apkProfile{}
