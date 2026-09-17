package builtin

import (
	"context"
	"strings"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
)

type mavenProfile struct{}

func init() {
	packageprofile.MustRegister(mavenProfile{})
}

func (mavenProfile) Describe() packageprofile.Description {
	return packageprofile.Description{
		Profile: upstream_entity.PackageProfileMaven,
		Name:    "Maven",
	}
}

func (mavenProfile) Classify(request packageprofile.Request) packageprofile.Representation {
	parts, ok := mavenPathParts(request.Path)
	if !ok {
		return packageprofile.Representation{}
	}

	filename := parts[len(parts)-1]
	if isMavenMetadata(filename) {
		return packageprofile.Representation{Class: packageprofile.ClassMutable}
	}
	if len(parts) < 4 {
		return packageprofile.Representation{}
	}

	artifact := parts[len(parts)-3]
	version := parts[len(parts)-2]
	if strings.HasSuffix(version, "-SNAPSHOT") {
		if !isMavenArtifact(filename, artifact, version) && !isUniqueMavenSnapshot(filename, artifact, version) {
			return packageprofile.Representation{}
		}
		return packageprofile.Representation{Class: packageprofile.ClassMutable}
	}
	if !isMavenArtifact(filename, artifact, version) {
		return packageprofile.Representation{}
	}
	if !hasImmutableMavenReleaseContract(request.Host) {
		return packageprofile.Representation{Class: packageprofile.ClassMutable}
	}
	return packageprofile.Representation{Class: packageprofile.ClassImmutable}
}

func (mavenProfile) Transform(_ context.Context, request packageprofile.TransformRequest) (*packageprofile.TransformResult, error) {
	return &packageprofile.TransformResult{
		Body:        append([]byte(nil), request.Body...),
		ContentType: request.ContentType,
	}, nil
}

func (mavenProfile) Companions() []packageprofile.Companion { return nil }

func (mavenProfile) Guidance() packageprofile.Guidance {
	return packageprofile.Guidance{
		Client: "Maven / Gradle / sbt",
		Configuration: []string{
			"Maven repository URL: https://<katch>/repo.maven.apache.org/maven2",
			"Gradle repository URL: https://<katch>/repo.maven.apache.org/maven2",
			"sbt repository URL: https://<katch>/repo.maven.apache.org/maven2",
		},
	}
}

func mavenPathParts(requestPath string) ([]string, bool) {
	if !strings.HasPrefix(requestPath, "/") || strings.HasSuffix(requestPath, "/") {
		return nil, false
	}
	parts := strings.Split(strings.TrimPrefix(requestPath, "/"), "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, false
		}
	}
	return parts, true
}

func isMavenMetadata(filename string) bool {
	const metadata = "maven-metadata.xml"
	if filename == metadata {
		return true
	}
	return isMavenSidecar(filename, metadata)
}

func isMavenArtifact(filename, artifact, version string) bool {
	prefix := artifact + "-" + version
	if !strings.HasPrefix(filename, prefix) || len(filename) == len(prefix) {
		return false
	}
	remainder := filename[len(prefix):]
	if remainder[0] != '.' && remainder[0] != '-' {
		return false
	}
	return strings.Contains(remainder, ".")
}

func isUniqueMavenSnapshot(filename, artifact, version string) bool {
	baseVersion := strings.TrimSuffix(version, "-SNAPSHOT")
	prefix := artifact + "-" + baseVersion + "-"
	return strings.HasPrefix(filename, prefix) && strings.Contains(filename[len(prefix):], ".")
}

func isMavenSidecar(filename, base string) bool {
	remainder := strings.TrimPrefix(filename, base)
	if remainder == filename || remainder == "" {
		return false
	}
	for _, suffix := range []string{".asc", ".md5", ".sha1", ".sha256", ".sha512"} {
		if strings.HasPrefix(remainder, suffix) {
			return isMavenSidecar(base+strings.TrimPrefix(remainder, suffix), base) || remainder == suffix
		}
	}
	return false
}

func hasImmutableMavenReleaseContract(host string) bool {
	switch strings.ToLower(strings.TrimSuffix(host, ".")) {
	case "repo1.maven.org", "repo.maven.apache.org":
		return true
	default:
		return false
	}
}
