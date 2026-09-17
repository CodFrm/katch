package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"

	"golang.org/x/mod/semver"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
)

const (
	cargoIndexHost  = "index.crates.io"
	cargoStaticHost = "static.crates.io"
)

var cargoStaticCompanion = packageprofile.Companion{
	Host:      cargoStaticHost,
	Profile:   upstream_entity.PackageProfileCargo,
	Transport: upstream_entity.ProtocolStatic,
}

type cargoProfile struct{}

func init() {
	packageprofile.MustRegister(cargoProfile{})
}

func (cargoProfile) Describe() packageprofile.Description {
	return packageprofile.Description{
		Profile: upstream_entity.PackageProfileCargo,
		Name:    "Cargo",
	}
}

func (cargoProfile) Classify(request packageprofile.Request) packageprofile.Representation {
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(request.Host), "."))
	path := request.Path
	switch host {
	case cargoIndexHost:
		if path == "/config.json" {
			return packageprofile.Representation{
				Class:      packageprofile.ClassMutable,
				Transform:  true,
				MediaTypes: []string{"application/json", "application/octet-stream"},
			}
		}
		if cargoSparseMetadataPath(path) {
			return packageprofile.Representation{Class: packageprofile.ClassMutable}
		}
	case cargoStaticHost:
		if cargoCratePath(path) {
			return packageprofile.Representation{Class: packageprofile.ClassImmutable}
		}
	}
	return packageprofile.Representation{}
}

func (cargoProfile) Transform(ctx context.Context, request packageprofile.TransformRequest) (*packageprofile.TransformResult, error) {
	if !cargoAbsoluteHTTPURL(request.SiteBaseURL) || request.RewriteURL == nil {
		return nil, packageprofile.ErrUnavailable
	}

	var document map[string]json.RawMessage
	if err := json.Unmarshal(request.Body, &document); err != nil || document == nil {
		return nil, packageprofile.ErrInvalidMetadata
	}
	var downloadBase string
	if err := json.Unmarshal(document["dl"], &downloadBase); err != nil || downloadBase == "" {
		return nil, packageprofile.ErrInvalidMetadata
	}
	target, err := url.Parse(downloadBase)
	if err != nil || !validCargoDownloadBase(target) {
		return nil, packageprofile.ErrInvalidMetadata
	}

	mapped, err := request.RewriteURL(ctx, target, cargoStaticCompanion)
	if err != nil {
		if errors.Is(err, packageprofile.ErrUnavailable) {
			return nil, packageprofile.ErrUnavailable
		}
		return nil, packageprofile.ErrInvalidMetadata
	}
	if mapped == nil || !mapped.IsAbs() || mapped.Host == "" || mapped.User != nil || (mapped.Scheme != "https" && mapped.Scheme != "http") || strings.ContainsAny(mapped.String(), "{}") {
		return nil, packageprofile.ErrUnavailable
	}
	rewrittenDownload, err := json.Marshal(mapped.String())
	if err != nil {
		return nil, packageprofile.ErrInvalidMetadata
	}
	document["dl"] = rewrittenDownload
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

func (cargoProfile) Companions() []packageprofile.Companion {
	return []packageprofile.Companion{cargoStaticCompanion}
}

func (cargoProfile) Guidance() packageprofile.Guidance {
	return packageprofile.Guidance{
		Client: "Cargo",
		Configuration: []string{
			"[source.crates-io]",
			"replace-with = \"katch\"",
			"[source.katch]",
			"registry = \"sparse+https://<katch>/index.crates.io/\"",
		},
	}
}

func validCargoDownloadBase(target *url.URL) bool {
	if target == nil || target.Scheme != "https" || target.User != nil || target.RawQuery != "" || target.Fragment != "" {
		return false
	}
	if strings.ToLower(strings.TrimSuffix(target.Hostname(), ".")) != cargoStaticHost || target.Port() != "" {
		return false
	}
	return target.EscapedPath() == "/crates" || target.EscapedPath() == "/crates/"
}

func cargoAbsoluteHTTPURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.IsAbs() && parsed.Host != "" && parsed.User == nil && (parsed.Scheme == "https" || parsed.Scheme == "http")
}

func cargoSparseMetadataPath(path string) bool {
	parts := cargoPathParts(path)
	if len(parts) < 2 {
		return false
	}
	name := parts[len(parts)-1]
	if !cargoName(name) {
		return false
	}
	switch len(name) {
	case 1:
		return len(parts) == 2 && parts[0] == "1"
	case 2:
		return len(parts) == 2 && parts[0] == "2"
	case 3:
		return len(parts) == 3 && parts[0] == "3" && parts[1] == name[:1]
	default:
		return len(parts) == 3 && parts[0] == name[:2] && parts[1] == name[2:4]
	}
}

func cargoCratePath(path string) bool {
	parts := cargoPathParts(path)
	if len(parts) < 3 || parts[0] != "crates" || !cargoName(parts[1]) {
		return false
	}
	if len(parts) == 4 {
		return parts[3] == "download" && cargoVersion(parts[2])
	}
	if len(parts) != 3 {
		return false
	}
	prefix := parts[1] + "-"
	if !strings.HasPrefix(parts[2], prefix) || !strings.HasSuffix(parts[2], ".crate") {
		return false
	}
	version := strings.TrimSuffix(strings.TrimPrefix(parts[2], prefix), ".crate")
	return cargoVersion(version)
}

func cargoVersion(version string) bool {
	core := version
	if suffix := strings.IndexAny(core, "-+"); suffix >= 0 {
		core = core[:suffix]
	}
	return strings.Count(core, ".") == 2 && semver.IsValid("v"+version)
}

func cargoPathParts(path string) []string {
	if path == "" || !strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") {
		return nil
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for _, part := range parts {
		if part == "" {
			return nil
		}
	}
	return parts
}

func cargoName(name string) bool {
	if name == "" {
		return false
	}
	for i := range len(name) {
		char := name[i]
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' && char != '_' {
			return false
		}
	}
	return true
}
