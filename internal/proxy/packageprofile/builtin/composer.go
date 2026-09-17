package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/url"
	"strings"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
)

const (
	composerPackageToken       = "%package%"
	composerPackagePlaceholder = "__KATCH_COMPOSER_PACKAGE__"
)

type composerProfile struct{}

func init() {
	packageprofile.MustRegister(composerProfile{})
}

func (composerProfile) Describe() packageprofile.Description {
	return packageprofile.Description{
		Profile: upstream_entity.PackageProfileComposer,
		Name:    "Composer 2",
	}
}

func (composerProfile) Classify(request packageprofile.Request) packageprofile.Representation {
	if request.Path != "/packages.json" &&
		(!strings.HasPrefix(request.Path, "/p2/") || !strings.HasSuffix(request.Path, ".json")) {
		return packageprofile.Representation{}
	}
	return packageprofile.Representation{
		Class:      packageprofile.ClassMutable,
		Transform:  true,
		MediaTypes: []string{"application/json"},
	}
}

func (composerProfile) Transform(
	ctx context.Context,
	request packageprofile.TransformRequest,
) (*packageprofile.TransformResult, error) {
	if request.Source == nil || request.RewriteURL == nil || strings.TrimSpace(request.SiteBaseURL) == "" {
		return nil, packageprofile.ErrUnavailable
	}
	mediaType, _, err := mime.ParseMediaType(request.ContentType)
	if err != nil || mediaType != "application/json" {
		return nil, packageprofile.ErrInvalidMetadata
	}

	var body []byte
	switch {
	case request.Source.Path == "/packages.json":
		body, err = rewriteComposerRoot(ctx, request.Body, request.RewriteURL)
	case strings.HasPrefix(request.Source.Path, "/p2/") && strings.HasSuffix(request.Source.Path, ".json"):
		body, err = rewriteComposerPackages(ctx, request.Body, request.RewriteURL)
	default:
		return nil, packageprofile.ErrInvalidMetadata
	}
	if err != nil {
		return nil, err
	}
	return &packageprofile.TransformResult{Body: body, ContentType: "application/json"}, nil
}

func (composerProfile) Companions() []packageprofile.Companion { return nil }

func (composerProfile) Guidance() packageprofile.Guidance {
	return packageprofile.Guidance{
		Client: "composer",
		Configuration: []string{
			"composer config --global repos.packagist composer <site-base>/repo.packagist.org",
			"composer install --prefer-dist",
		},
	}
}

func rewriteComposerRoot(
	ctx context.Context,
	body []byte,
	rewrite packageprofile.RewriteURL,
) ([]byte, error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, packageprofile.ErrInvalidMetadata
	}
	var metadataURL string
	if err := json.Unmarshal(document["metadata-url"], &metadataURL); err != nil {
		return nil, packageprofile.ErrInvalidMetadata
	}
	rewritten, err := rewriteComposerTemplate(ctx, metadataURL, rewrite)
	if err != nil {
		return nil, err
	}
	document["metadata-url"], err = json.Marshal(rewritten)
	if err != nil {
		return nil, fmt.Errorf("marshal composer metadata-url: %w", err)
	}
	return marshalComposerDocument(document)
}

func rewriteComposerTemplate(
	ctx context.Context,
	template string,
	rewrite packageprofile.RewriteURL,
) (string, error) {
	if strings.Count(template, composerPackageToken) != 1 || strings.Contains(template, composerPackagePlaceholder) {
		return "", packageprofile.ErrInvalidMetadata
	}
	parsed, err := url.Parse(strings.Replace(template, composerPackageToken, composerPackagePlaceholder, 1))
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", packageprofile.ErrInvalidMetadata
	}
	rewritten, err := rewrite(ctx, parsed, composerCompanion(
		parsed.Hostname(), upstream_entity.PackageProfileComposer,
	))
	if err != nil {
		return "", err
	}
	if rewritten == nil {
		return "", packageprofile.ErrUnavailable
	}
	result := rewritten.String()
	if strings.Count(result, composerPackagePlaceholder) != 1 {
		return "", packageprofile.ErrInvalidMetadata
	}
	return strings.Replace(result, composerPackagePlaceholder, composerPackageToken, 1), nil
}

func rewriteComposerPackages(
	ctx context.Context,
	body []byte,
	rewrite packageprofile.RewriteURL,
) ([]byte, error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, packageprofile.ErrInvalidMetadata
	}
	var packages map[string][]json.RawMessage
	if err := json.Unmarshal(document["packages"], &packages); err != nil {
		return nil, packageprofile.ErrInvalidMetadata
	}
	for name, versions := range packages {
		for index, version := range versions {
			rewritten, err := rewriteComposerVersion(ctx, version, rewrite)
			if err != nil {
				return nil, err
			}
			versions[index] = rewritten
		}
		packages[name] = versions
	}
	var err error
	document["packages"], err = json.Marshal(packages)
	if err != nil {
		return nil, fmt.Errorf("marshal composer packages: %w", err)
	}
	return marshalComposerDocument(document)
}

func rewriteComposerVersion(
	ctx context.Context,
	body json.RawMessage,
	rewrite packageprofile.RewriteURL,
) (json.RawMessage, error) {
	var version map[string]json.RawMessage
	if err := json.Unmarshal(body, &version); err != nil {
		return nil, packageprofile.ErrInvalidMetadata
	}
	distBody, hasDist := version["dist"]
	if !hasDist || string(distBody) == "null" {
		return body, nil
	}
	var dist map[string]json.RawMessage
	if err := json.Unmarshal(distBody, &dist); err != nil {
		return nil, packageprofile.ErrInvalidMetadata
	}
	urlBody, hasURL := dist["url"]
	if !hasURL {
		return body, nil
	}
	var rawURL string
	if err := json.Unmarshal(urlBody, &rawURL); err != nil {
		return nil, packageprofile.ErrInvalidMetadata
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, packageprofile.ErrInvalidMetadata
	}
	rewritten, err := rewrite(ctx, parsed, composerCompanion(
		parsed.Hostname(), upstream_entity.PackageProfileNone,
	))
	if err != nil {
		if errors.Is(err, packageprofile.ErrUnavailable) {
			return nil, packageprofile.ErrUnavailable
		}
		return nil, err
	}
	if rewritten == nil {
		return nil, packageprofile.ErrUnavailable
	}
	dist["url"], err = json.Marshal(rewritten.String())
	if err != nil {
		return nil, fmt.Errorf("marshal composer dist URL: %w", err)
	}
	version["dist"], err = json.Marshal(dist)
	if err != nil {
		return nil, fmt.Errorf("marshal composer dist: %w", err)
	}
	return json.Marshal(version)
}

func composerCompanion(
	host string,
	profile upstream_entity.PackageProfile,
) packageprofile.Companion {
	return packageprofile.Companion{
		Host:      host,
		Profile:   profile,
		Transport: upstream_entity.ProtocolStatic,
	}
}

func marshalComposerDocument(document map[string]json.RawMessage) ([]byte, error) {
	body, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("marshal composer metadata: %w", err)
	}
	return body, nil
}
