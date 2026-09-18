package builtin

import (
	"context"
	"path"
	"strings"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
)

type rpmProfile struct{}

func init() {
	packageprofile.MustRegister(rpmProfile{})
}

func (rpmProfile) Describe() packageprofile.Description {
	return packageprofile.Description{
		Profile: upstream_entity.PackageProfileRPM,
		Name:    "rpm",
	}
}

func (rpmProfile) Classify(request packageprofile.Request) packageprofile.Representation {
	requestPath := strings.TrimSpace(request.Path)
	base := path.Base(requestPath)
	if base == "mirrorlist" || base == "metalink" {
		return packageprofile.Representation{}
	}
	if rpmRepodataPath(requestPath) {
		if rpmStrongDigestName(base) && !strings.HasPrefix(strings.ToLower(base), "repomd.xml") {
			return packageprofile.Representation{Class: packageprofile.ClassImmutable}
		}
		return packageprofile.Representation{Class: packageprofile.ClassMutable}
	}
	if strings.HasSuffix(strings.ToLower(base), ".rpm") {
		class := packageprofile.ClassMutable
		if rpmImmutablePackageName(base) {
			class = packageprofile.ClassImmutable
		}
		return packageprofile.Representation{Class: class}
	}
	return packageprofile.Representation{}
}

func (rpmProfile) Transform(_ context.Context, request packageprofile.TransformRequest) (*packageprofile.TransformResult, error) {
	return &packageprofile.TransformResult{
		Body:        request.Body,
		ContentType: request.ContentType,
	}, nil
}

func (rpmProfile) Companions() []packageprofile.Companion {
	return nil
}

func (rpmProfile) Guidance() packageprofile.Guidance {
	return packageprofile.Guidance{
		Clients: []string{"dnf", "yum"},
		Configuration: []string{
			"baseurl=https://<katch>/<upstream>/<repository-path>/",
			"mirrorlist=",
			"metalink=",
		},
		Constraints:     []string{"fixed_base", "no_dynamic_mirrors"},
		RuntimeVerified: true,
	}
}

func rpmRepodataPath(requestPath string) bool {
	for _, part := range strings.Split(strings.Trim(requestPath, "/"), "/") {
		if part == "repodata" {
			return true
		}
	}
	return false
}

func rpmStrongDigestName(name string) bool {
	digest, _, found := strings.Cut(name, "-")
	if !found || len(digest) != 64 && len(digest) != 96 && len(digest) != 128 {
		return false
	}
	for _, char := range digest {
		if char < '0' || char > '9' && char < 'A' || char > 'F' && char < 'a' || char > 'f' {
			return false
		}
	}
	return true
}

func rpmImmutablePackageName(name string) bool {
	lower := strings.ToLower(name)
	if !strings.HasSuffix(lower, ".rpm") {
		return false
	}
	withoutExtension := name[:len(name)-len(".rpm")]
	identity, architecture, found := rpmCutLast(withoutExtension, ".")
	if !found || !rpmArchitecture(architecture) {
		return false
	}
	nameVersion, release, found := rpmCutLast(identity, "-")
	if !found || !rpmVersionPart(release) {
		return false
	}
	packageName, version, found := rpmCutLast(nameVersion, "-")
	return found && packageName != "" && rpmVersionPart(version)
}

func rpmCutLast(value, separator string) (string, string, bool) {
	index := strings.LastIndex(value, separator)
	if index < 0 {
		return value, "", false
	}
	return value[:index], value[index+len(separator):], true
}

func rpmArchitecture(value string) bool {
	switch strings.ToLower(value) {
	case "noarch", "src", "nosrc", "x86_64", "aarch64", "ppc64le", "ppc64", "s390x",
		"i686", "i586", "i486", "i386", "armv7hl", "armhfp", "riscv64", "loongarch64":
		return true
	default:
		return false
	}
}

func rpmVersionPart(value string) bool {
	if value == "" {
		return false
	}
	hasDigit := false
	for _, char := range value {
		switch {
		case char >= '0' && char <= '9':
			hasDigit = true
		case char >= 'A' && char <= 'Z', char >= 'a' && char <= 'z':
		case char == '.', char == '_', char == '+', char == '~', char == '^':
		default:
			return false
		}
	}
	return hasDigit
}
