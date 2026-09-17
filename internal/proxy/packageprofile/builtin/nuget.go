package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
)

const (
	nugetAPIHost            = "api.nuget.org"
	nugetRegionalAPIHost    = "nuget.azure.cn"
	nugetSearchUSNCHost     = "azuresearch-usnc.nuget.org"
	nugetSearchEastAsiaHost = "azuresearch-ea.nuget.org"
	nugetSearchSEAsiaHost   = "azuresearch-sea.nuget.org"
	nugetGlobalCDNHost      = "globalcdn.nuget.org"
	nugetWebHost            = "www.nuget.org"
)

var nugetCompanions = []packageprofile.Companion{
	{Host: nugetAPIHost, Profile: upstream_entity.PackageProfileNuGet, Transport: upstream_entity.ProtocolStatic},
	{Host: nugetRegionalAPIHost, Profile: upstream_entity.PackageProfileNuGet, Transport: upstream_entity.ProtocolStatic},
	{Host: nugetSearchUSNCHost, Profile: upstream_entity.PackageProfileNuGet, Transport: upstream_entity.ProtocolStatic},
	{Host: nugetSearchEastAsiaHost, Profile: upstream_entity.PackageProfileNuGet, Transport: upstream_entity.ProtocolStatic},
	{Host: nugetSearchSEAsiaHost, Profile: upstream_entity.PackageProfileNuGet, Transport: upstream_entity.ProtocolStatic},
	{Host: nugetGlobalCDNHost, Profile: upstream_entity.PackageProfileNuGet, Transport: upstream_entity.ProtocolStatic},
	{Host: nugetWebHost, Profile: upstream_entity.PackageProfileNuGet, Transport: upstream_entity.ProtocolStatic},
}

var nugetRegistrationPrefixes = []string{
	"/v3/registration5-semver1/",
	"/v3/registration5-semver2/",
	"/v3/registration5-gz-semver1/",
	"/v3/registration5-gz-semver2/",
}

type nugetProfile struct{}

func init() {
	packageprofile.MustRegister(nugetProfile{})
}

func (nugetProfile) Describe() packageprofile.Description {
	return packageprofile.Description{
		Profile: upstream_entity.PackageProfileNuGet,
		Name:    "NuGet",
	}
}

func (nugetProfile) Classify(request packageprofile.Request) packageprofile.Representation {
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(request.Host), "."))
	path := strings.TrimSpace(request.Path)

	switch host {
	case nugetSearchUSNCHost, nugetSearchEastAsiaHost, nugetSearchSEAsiaHost:
		if path == "/query" || path == "/query/" || path == "/autocomplete" || path == "/autocomplete/" {
			return nugetJSONRepresentation(packageprofile.ClassMutable, true)
		}
		return packageprofile.Representation{}
	case nugetAPIHost:
		return classifyNuGetAPIPath(path)
	case nugetRegionalAPIHost:
		if path == "/v3/index.json" {
			return nugetJSONRepresentation(packageprofile.ClassMutable, true)
		}
		return packageprofile.Representation{}
	case nugetGlobalCDNHost:
		if nugetVersionedReadmePath(path) {
			return packageprofile.Representation{Class: packageprofile.ClassImmutable}
		}
		return packageprofile.Representation{}
	default:
		return packageprofile.Representation{}
	}
}

func (nugetProfile) Transform(ctx context.Context, request packageprofile.TransformRequest) (*packageprofile.TransformResult, error) {
	if strings.TrimSpace(request.SiteBaseURL) == "" || request.RewriteURL == nil {
		return nil, packageprofile.ErrUnavailable
	}
	if _, err := absoluteHTTPURL(request.SiteBaseURL); err != nil {
		return nil, packageprofile.ErrUnavailable
	}

	decoder := json.NewDecoder(bytes.NewReader(request.Body))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil || document == nil {
		return nil, packageprofile.ErrInvalidMetadata
	}
	if _, ok := document.(map[string]any); !ok {
		return nil, packageprofile.ErrInvalidMetadata
	}
	if err := ensureNuGetJSONEnd(decoder); err != nil {
		return nil, err
	}
	if err := rewriteNuGetDocument(ctx, request, document); err != nil {
		return nil, err
	}
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

func (nugetProfile) Companions() []packageprofile.Companion {
	return append([]packageprofile.Companion(nil), nugetCompanions...)
}

func (nugetProfile) Guidance() packageprofile.Guidance {
	return packageprofile.Guidance{
		Client: "dotnet",
		Configuration: []string{
			"dotnet restore --source https://<katch>/api.nuget.org/v3/index.json",
			"dotnet package search <term> --source https://<katch>/api.nuget.org/v3/index.json",
		},
	}
}

func classifyNuGetAPIPath(path string) packageprofile.Representation {
	if path == "/v3/index.json" {
		return nugetJSONRepresentation(packageprofile.ClassMutable, true)
	}
	for _, prefix := range nugetRegistrationPrefixes {
		if strings.HasPrefix(path, prefix) && strings.HasSuffix(strings.ToLower(path), ".json") {
			return nugetJSONRepresentation(packageprofile.ClassMutable, true)
		}
	}
	if strings.HasPrefix(path, "/v3/catalog0/") && strings.HasSuffix(strings.ToLower(path), ".json") {
		class := packageprofile.ClassImmutable
		if path == "/v3/catalog0/index.json" {
			class = packageprofile.ClassMutable
		}
		return nugetJSONRepresentation(class, true)
	}
	if strings.HasPrefix(path, "/v3-index/repository-signatures/") && strings.HasSuffix(strings.ToLower(path), ".json") {
		return nugetJSONRepresentation(packageprofile.ClassMutable, true)
	}
	if strings.HasPrefix(path, "/v3-flatcontainer/") {
		if nugetFlatContainerVersionIndex(path) {
			return nugetJSONRepresentation(packageprofile.ClassMutable, false)
		}
		if nugetVersionedPackagePath(path) {
			return packageprofile.Representation{Class: packageprofile.ClassImmutable}
		}
	}
	return packageprofile.Representation{}
}

func nugetJSONRepresentation(class packageprofile.Class, transform bool) packageprofile.Representation {
	return packageprofile.Representation{
		Class:      class,
		Transform:  transform,
		MediaTypes: []string{"application/json"},
	}
}

func nugetFlatContainerVersionIndex(path string) bool {
	parts := splitNuGetPath(path)
	return len(parts) == 3 && parts[0] == "v3-flatcontainer" && parts[1] != "" && strings.EqualFold(parts[2], "index.json")
}

func nugetVersionedPackagePath(path string) bool {
	parts := splitNuGetPath(path)
	return len(parts) == 4 && parts[0] == "v3-flatcontainer" && parts[1] != "" && parts[2] != "" && parts[3] != ""
}

func nugetVersionedReadmePath(path string) bool {
	if !strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	return len(parts) == 4 && parts[0] == "v3-flatcontainer" &&
		nugetLiteralSegment(parts[1]) && nugetLiteralSegment(parts[2]) && parts[3] == "readme"
}

func nugetLiteralSegment(segment string) bool {
	if segment == "" || segment == "." || segment == ".." {
		return false
	}
	for i := range len(segment) {
		char := segment[i]
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '.' || char == '-' || char == '_' || char == '+' {
			continue
		}
		return false
	}
	return true
}

func splitNuGetPath(path string) []string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}

func ensureNuGetJSONEnd(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return packageprofile.ErrInvalidMetadata
	}
	return nil
}

func rewriteNuGetDocument(ctx context.Context, request packageprofile.TransformRequest, node any) error {
	switch value := node.(type) {
	case []any:
		for _, item := range value {
			if err := rewriteNuGetDocument(ctx, request, item); err != nil {
				return err
			}
		}
	case map[string]any:
		for key, item := range value {
			if key == "@context" {
				continue
			}
			switch key {
			case "@id", "registration", "packageContent":
				rewritten, err := rewriteNuGetNavigationURL(ctx, request, item)
				if err != nil {
					return err
				}
				value[key] = rewritten
			case "catalogEntry":
				if raw, ok := item.(string); ok {
					rewritten, err := rewriteNuGetNavigationURL(ctx, request, raw)
					if err != nil {
						return err
					}
					value[key] = rewritten
					continue
				}
				if _, ok := item.(map[string]any); !ok {
					return packageprofile.ErrInvalidMetadata
				}
				if err := rewriteNuGetDocument(ctx, request, item); err != nil {
					return err
				}
			default:
				if err := rewriteNuGetDocument(ctx, request, item); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func rewriteNuGetNavigationURL(ctx context.Context, request packageprofile.TransformRequest, value any) (string, error) {
	raw, ok := value.(string)
	if !ok || strings.TrimSpace(raw) == "" {
		return "", packageprofile.ErrInvalidMetadata
	}
	target, err := url.Parse(raw)
	if err != nil {
		return "", packageprofile.ErrInvalidMetadata
	}
	if !target.IsAbs() {
		if request.Source == nil || !request.Source.IsAbs() {
			return "", packageprofile.ErrInvalidMetadata
		}
		target = request.Source.ResolveReference(target)
	}
	if target.Scheme != "https" || target.User != nil || target.Host == "" || target.Port() != "" {
		return "", packageprofile.ErrInvalidMetadata
	}
	companion, ok := nugetCompanionForHost(target.Hostname())
	if !ok {
		return "", packageprofile.ErrInvalidMetadata
	}
	mapped, err := request.RewriteURL(ctx, target, companion)
	if err != nil {
		if errors.Is(err, packageprofile.ErrUnavailable) {
			return "", packageprofile.ErrUnavailable
		}
		return "", packageprofile.ErrInvalidMetadata
	}
	if mapped == nil || !mapped.IsAbs() || mapped.Host == "" || mapped.User != nil || (mapped.Scheme != "https" && mapped.Scheme != "http") {
		return "", packageprofile.ErrUnavailable
	}
	return mapped.String(), nil
}

func nugetCompanionForHost(host string) (packageprofile.Companion, bool) {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, companion := range nugetCompanions {
		if host == companion.Host {
			return companion, true
		}
	}
	return packageprofile.Companion{}, false
}
