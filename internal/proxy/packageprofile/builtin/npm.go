package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
)

const npmRegistryHost = "registry.npmjs.org"

var npmCompanion = packageprofile.Companion{
	Host:      npmRegistryHost,
	Profile:   upstream_entity.PackageProfileNPM,
	Transport: upstream_entity.ProtocolStatic,
}

type npmProfile struct{}

func init() {
	packageprofile.MustRegister(npmProfile{})
}

func (npmProfile) Describe() packageprofile.Description {
	return packageprofile.Description{
		Profile:  upstream_entity.PackageProfileNPM,
		Name:     "npm",
		Variants: []string{"Accept"},
	}
}

func (npmProfile) Classify(request packageprofile.Request) packageprofile.Representation {
	path := strings.TrimSpace(request.Path)
	if npmTarballPath(path) {
		return packageprofile.Representation{Class: packageprofile.ClassImmutable}
	}
	if npmDistTagPath(path) {
		return packageprofile.Representation{Class: packageprofile.ClassMutable}
	}
	if npmPackumentPath(path) {
		return packageprofile.Representation{
			Class:      packageprofile.ClassMutable,
			Transform:  true,
			Variants:   []string{"Accept"},
			MediaTypes: []string{"application/json", "application/vnd.npm.install-v1+json"},
		}
	}
	return packageprofile.Representation{}
}

func (npmProfile) Transform(ctx context.Context, request packageprofile.TransformRequest) (*packageprofile.TransformResult, error) {
	if strings.TrimSpace(request.SiteBaseURL) == "" || request.RewriteURL == nil {
		return nil, packageprofile.ErrUnavailable
	}
	if _, err := absoluteHTTPURL(request.SiteBaseURL); err != nil {
		return nil, packageprofile.ErrUnavailable
	}

	var document map[string]json.RawMessage
	if err := json.Unmarshal(request.Body, &document); err != nil || document == nil {
		return nil, packageprofile.ErrInvalidMetadata
	}
	versionsRaw, ok := document["versions"]
	if !ok {
		return nil, packageprofile.ErrInvalidMetadata
	}
	var versions map[string]json.RawMessage
	if err := json.Unmarshal(versionsRaw, &versions); err != nil || versions == nil {
		return nil, packageprofile.ErrInvalidMetadata
	}

	for version, versionRaw := range versions {
		rewritten, err := rewriteNPMVersion(ctx, request, versionRaw)
		if err != nil {
			return nil, err
		}
		versions[version] = rewritten
	}
	rewrittenVersions, err := json.Marshal(versions)
	if err != nil {
		return nil, packageprofile.ErrInvalidMetadata
	}
	document["versions"] = rewrittenVersions
	body, err := json.Marshal(document)
	if err != nil {
		return nil, packageprofile.ErrInvalidMetadata
	}
	contentType := request.ContentType
	if contentType == "" {
		contentType = "application/json"
	}
	return &packageprofile.TransformResult{Body: body, ContentType: contentType}, nil
}

func (npmProfile) Companions() []packageprofile.Companion {
	return []packageprofile.Companion{npmCompanion}
}

func (npmProfile) Guidance() packageprofile.Guidance {
	return packageprofile.Guidance{
		Client: "npm",
		Configuration: []string{
			"registry=https://<katch>/registry.npmjs.org/",
			"replace-registry-host=always",
			"Regenerate existing lockfiles when the client cannot replace registry hosts.",
		},
	}
}

func rewriteNPMVersion(ctx context.Context, request packageprofile.TransformRequest, raw json.RawMessage) (json.RawMessage, error) {
	var version map[string]json.RawMessage
	if err := json.Unmarshal(raw, &version); err != nil || version == nil {
		return nil, packageprofile.ErrInvalidMetadata
	}
	distRaw, ok := version["dist"]
	if !ok {
		return nil, packageprofile.ErrInvalidMetadata
	}
	var dist map[string]json.RawMessage
	if err := json.Unmarshal(distRaw, &dist); err != nil || dist == nil {
		return nil, packageprofile.ErrInvalidMetadata
	}
	var tarball string
	if err := json.Unmarshal(dist["tarball"], &tarball); err != nil || strings.TrimSpace(tarball) == "" {
		return nil, packageprofile.ErrInvalidMetadata
	}
	target, err := url.Parse(tarball)
	if err != nil {
		return nil, packageprofile.ErrInvalidMetadata
	}
	if !target.IsAbs() {
		if request.Source == nil {
			return nil, packageprofile.ErrInvalidMetadata
		}
		target = request.Source.ResolveReference(target)
	}
	if err := validateNPMTarballURL(target); err != nil {
		return nil, err
	}
	mapped, err := request.RewriteURL(ctx, target, npmCompanion)
	if err != nil {
		if errors.Is(err, packageprofile.ErrUnavailable) {
			return nil, packageprofile.ErrUnavailable
		}
		return nil, packageprofile.ErrInvalidMetadata
	}
	if mapped == nil || !mapped.IsAbs() {
		return nil, packageprofile.ErrUnavailable
	}
	rewrittenTarball, err := json.Marshal(mapped.String())
	if err != nil {
		return nil, packageprofile.ErrInvalidMetadata
	}
	dist["tarball"] = rewrittenTarball
	rewrittenDist, err := json.Marshal(dist)
	if err != nil {
		return nil, packageprofile.ErrInvalidMetadata
	}
	version["dist"] = rewrittenDist
	return json.Marshal(version)
}

func validateNPMTarballURL(target *url.URL) error {
	if target == nil || target.User != nil || (target.Scheme != "https" && target.Scheme != "http") {
		return packageprofile.ErrInvalidMetadata
	}
	host := strings.ToLower(strings.TrimSuffix(target.Hostname(), "."))
	if host != npmRegistryHost || !npmTarballPath(target.EscapedPath()) {
		return packageprofile.ErrInvalidMetadata
	}
	return nil
}

func absoluteHTTPURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, packageprofile.ErrUnavailable
	}
	return parsed, nil
}

func npmTarballPath(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), ".tgz") && strings.Contains(path, "/-/")
}

func npmDistTagPath(path string) bool {
	parts := splitNPMPath(path)
	if len(parts) < 4 || parts[0] != "-" || parts[1] != "package" {
		return false
	}
	for _, part := range parts[2:] {
		if part == "dist-tags" {
			return true
		}
	}
	return false
}

func npmPackumentPath(path string) bool {
	parts := splitNPMPath(path)
	if len(parts) == 1 {
		return parts[0] != "" && parts[0] != "-"
	}
	return len(parts) == 2 && strings.HasPrefix(parts[0], "@") && parts[1] != ""
}

func splitNPMPath(path string) []string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}
