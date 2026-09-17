package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
)

func TestComposerProfileClassifiesOnlyComposer2Metadata(t *testing.T) {
	profile := composerProfile{}

	for _, path := range []string{"/packages.json", "/p2/acme/widget.json", "/p2/acme/widget~dev.json"} {
		representation := profile.Classify(packageprofile.Request{Path: path})
		assertComposerEqual(t, packageprofile.ClassMutable, representation.Class, path)
		assertComposerTrue(t, representation.Transform, path)
		assertComposerEqual(t, []string{"application/json"}, representation.MediaTypes, path)
	}
	for _, path := range []string{"/p/acme/widget.json", "/provider-acme/widget.json", "/dist/widget.zip"} {
		assertComposerTrue(t, !profile.Classify(packageprofile.Request{Path: path}).Recognized(), path)
	}
}

func TestComposerProfileClassifiesOnlyGitHubFullSHADistPaths(t *testing.T) {
	profile := composerProfile{}
	sha := "0123456789abcdef0123456789ABCDEF01234567"

	for _, request := range []packageprofile.Request{
		{Host: "api.github.com", Path: "/repos/acme/widget/zipball/" + sha},
		{Host: "codeload.github.com", Path: "/acme/widget/legacy.zip/" + sha},
	} {
		representation := profile.Classify(request)
		assertComposerEqual(t, packageprofile.ClassImmutable, representation.Class, request.Path)
		assertComposerTrue(t, !representation.Transform, request.Path)
		assertComposerEqual(t, []string{"Authorization"}, representation.Variants, request.Path)
		assertComposerEqual(t, 0, len(representation.MediaTypes), request.Path)
	}

	for _, request := range []packageprofile.Request{
		{Host: "api.github.com", Path: "/repos/acme/widget/zipball/v1.2.3"},
		{Host: "api.github.com", Path: "/repos/acme/widget/zipball/0123456789abcdef0123456789abcdef0123456"},
		{Host: "api.github.com", Path: "/repos/acme/widget/zipball/0123456789abcdef0123456789abcdef012345678"},
		{Host: "api.github.com", Path: "/repos/acme/widget/zipball/0123456789abcdef0123456789abcdef0123456g"},
		{Host: "api.github.com", Path: "/repos/acme/../zipball/" + sha},
		{Host: "api.github.com", Path: "/repos/acme/%2e%2e/zipball/" + sha},
		{Host: "api.github.com", Path: "/repos/acme%2fescape/widget/zipball/" + sha},
		{Host: "api.github.com", Path: "/repos/acme/widget/zipball/" + sha + "/extra"},
		{Host: "codeload.github.com", Path: "/acme/widget/legacy.zip/main"},
		{Host: "codeload.github.com", Path: "/acme/../legacy.zip/" + sha},
		{Host: "codeload.github.com", Path: "/acme/%2E%2E/legacy.zip/" + sha},
		{Host: "codeload.github.com", Path: "/acme/widget/legacy.zip/" + sha + "/extra"},
		{Host: "example.com", Path: "/repos/acme/widget/zipball/" + sha},
		{Host: "example.com", Path: "/acme/widget/legacy.zip/" + sha},
	} {
		assertComposerTrue(t, !profile.Classify(request).Recognized(), request.Host+request.Path)
	}
}

func TestComposerProfileRewritesRootMetadataTemplateLiterally(t *testing.T) {
	body := readComposerFixture(t, "packages.json")
	profile := composerProfile{}

	var gotCompanion packageprofile.Companion
	resolver := composerTestResolver("https://mirror.example", map[string]packageprofile.Companion{
		"repo.packagist.org": {
			Host:      "repo.packagist.org",
			Profile:   upstream_entity.PackageProfileComposer,
			Transport: upstream_entity.ProtocolStatic,
		},
	})
	result, err := profile.Transform(context.Background(), packageprofile.TransformRequest{
		Body:        body,
		ContentType: "application/json",
		Source:      mustComposerURL(t, "https://repo.packagist.org/packages.json"),
		SiteBaseURL: "https://mirror.example",
		RewriteURL: func(ctx context.Context, source *url.URL, companion packageprofile.Companion) (*url.URL, error) {
			gotCompanion = companion
			return resolver(ctx, source, companion)
		},
	})
	requireComposerNoError(t, err)
	assertComposerEqual(t, packageprofile.Companion{
		Host:      "repo.packagist.org",
		Profile:   upstream_entity.PackageProfileComposer,
		Transport: upstream_entity.ProtocolStatic,
	}, gotCompanion)
	assertComposerTrue(t, strings.Contains(string(result.Body), `"metadata-url":"https://mirror.example/repo.packagist.org/p2/%package%.json"`))
	assertComposerTrue(t, !strings.Contains(string(result.Body), "%25package%"))

	var got, original map[string]any
	requireComposerNoError(t, json.Unmarshal(result.Body, &got))
	requireComposerNoError(t, json.Unmarshal(body, &original))
	assertComposerEqual(t, original["notify-batch"], got["notify-batch"])
	assertComposerEqual(t, original["providers-url"], got["providers-url"])
}

func TestComposerProfileRewritesOnlyDistURLAndPreservesPackageData(t *testing.T) {
	body := readComposerFixture(t, "package.json")
	profile := composerProfile{}

	var gotCompanion packageprofile.Companion
	resolver := composerTestResolver("https://mirror.example", map[string]packageprofile.Companion{
		"api.github.com": {
			Host:      "api.github.com",
			Profile:   upstream_entity.PackageProfileComposer,
			Transport: upstream_entity.ProtocolStatic,
		},
	})
	result, err := profile.Transform(context.Background(), packageprofile.TransformRequest{
		Body:        body,
		ContentType: "application/json; charset=utf-8",
		Source:      mustComposerURL(t, "https://repo.packagist.org/p2/acme/widget.json"),
		SiteBaseURL: "https://mirror.example",
		RewriteURL: func(ctx context.Context, source *url.URL, companion packageprofile.Companion) (*url.URL, error) {
			gotCompanion = companion
			return resolver(ctx, source, companion)
		},
	})
	requireComposerNoError(t, err)
	assertComposerEqual(t, packageprofile.Companion{
		Host:      "api.github.com",
		Profile:   upstream_entity.PackageProfileComposer,
		Transport: upstream_entity.ProtocolStatic,
	}, gotCompanion)

	var got, original map[string]any
	requireComposerNoError(t, json.Unmarshal(result.Body, &got))
	requireComposerNoError(t, json.Unmarshal(body, &original))
	gotVersion := composerVersion(t, got)
	originalVersion := composerVersion(t, original)
	gotDist := gotVersion["dist"].(map[string]any)
	originalDist := originalVersion["dist"].(map[string]any)
	assertComposerEqual(t, "https://mirror.example/api.github.com/repos/acme/widget/zipball/0123456789abcdef", gotDist["url"])
	assertComposerEqual(t, originalDist["reference"], gotDist["reference"])
	assertComposerEqual(t, originalDist["shasum"], gotDist["shasum"])
	assertComposerEqual(t, originalVersion["require"], gotVersion["require"])
	assertComposerEqual(t, originalVersion["source"], gotVersion["source"])
	assertComposerEqual(t, originalVersion["support"], gotVersion["support"])
	assertComposerEqual(t, "application/json", result.ContentType)
}

func TestComposerProfileFailsClosedOnCompanionProfileMismatch(t *testing.T) {
	profile := composerProfile{}
	tests := []struct {
		name       string
		body       []byte
		source     string
		registered map[string]packageprofile.Companion
	}{
		{
			name:   "root metadata requires composer profile",
			body:   readComposerFixture(t, "packages.json"),
			source: "https://repo.packagist.org/packages.json",
			registered: map[string]packageprofile.Companion{
				"repo.packagist.org": {
					Host:      "repo.packagist.org",
					Profile:   upstream_entity.PackageProfileNone,
					Transport: upstream_entity.ProtocolStatic,
				},
			},
		},
		{
			name:   "dist requires composer profile",
			body:   readComposerFixture(t, "package.json"),
			source: "https://repo.packagist.org/p2/acme/widget.json",
			registered: map[string]packageprofile.Companion{
				"api.github.com": {
					Host:      "api.github.com",
					Profile:   upstream_entity.PackageProfileNone,
					Transport: upstream_entity.ProtocolStatic,
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := profile.Transform(context.Background(), packageprofile.TransformRequest{
				Body:        test.body,
				ContentType: "application/json",
				Source:      mustComposerURL(t, test.source),
				SiteBaseURL: "https://mirror.example",
				RewriteURL:  composerTestResolver("https://mirror.example", test.registered),
			})
			if !errors.Is(err, packageprofile.ErrUnavailable) {
				t.Fatalf("Transform() error = %v, want ErrUnavailable", err)
			}
		})
	}
}

func TestComposerProfileFailsClosedWithoutDistCompanion(t *testing.T) {
	profile := composerProfile{}

	_, err := profile.Transform(context.Background(), packageprofile.TransformRequest{
		Body:        readComposerFixture(t, "package.json"),
		ContentType: "application/json",
		Source:      mustComposerURL(t, "https://repo.packagist.org/p2/acme/widget.json"),
		SiteBaseURL: "https://mirror.example",
		RewriteURL:  composerTestResolver("https://mirror.example", nil),
	})
	if err == nil {
		t.Fatal("Transform() error = nil, want unavailable")
	}
	if !errors.Is(err, packageprofile.ErrUnavailable) {
		t.Fatalf("Transform() error = %v, want ErrUnavailable", err)
	}
}

func TestComposerProfileDescription(t *testing.T) {
	profile := composerProfile{}

	assertComposerEqual(t, upstream_entity.PackageProfileComposer, profile.Describe().Profile)
	assertComposerEqual(t, []packageprofile.Companion{
		{Host: "api.github.com", Profile: upstream_entity.PackageProfileComposer, Transport: upstream_entity.ProtocolStatic},
		{Host: "codeload.github.com", Profile: upstream_entity.PackageProfileComposer, Transport: upstream_entity.ProtocolStatic},
	}, profile.Companions())
	assertComposerEqual(t, "composer", profile.Guidance().Client)
}

func readComposerFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile("testdata/composer/" + name)
	requireComposerNoError(t, err)
	return body
}

func mustComposerURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	requireComposerNoError(t, err)
	return parsed
}

func composerTestResolver(
	siteBaseURL string,
	registered map[string]packageprofile.Companion,
) packageprofile.RewriteURL {
	return func(_ context.Context, source *url.URL, companion packageprofile.Companion) (*url.URL, error) {
		host := strings.ToLower(strings.TrimSuffix(source.Hostname(), "."))
		if companion.Host != host {
			return nil, packageprofile.ErrInvalidMetadata
		}
		configured, ok := registered[host]
		if !ok || (companion.Transport != "" && configured.Transport != companion.Transport) ||
			(companion.Profile != "" && upstream_entity.NormalizePackageProfile(configured.Profile) != companion.Profile) {
			return nil, packageprofile.ErrUnavailable
		}
		rewritten, err := url.Parse(strings.TrimSuffix(siteBaseURL, "/") + "/" + host + source.EscapedPath())
		if err != nil {
			return nil, packageprofile.ErrUnavailable
		}
		rewritten.RawQuery = source.RawQuery
		rewritten.Fragment = source.Fragment
		return rewritten, nil
	}
}

func composerVersion(t *testing.T, document map[string]any) map[string]any {
	t.Helper()
	packages, ok := document["packages"].(map[string]any)
	assertComposerTrue(t, ok)
	versions, ok := packages["acme/widget"].([]any)
	assertComposerTrue(t, ok)
	assertComposerEqual(t, 1, len(versions))
	version, ok := versions[0].(map[string]any)
	assertComposerTrue(t, ok)
	return version
}

func requireComposerNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func assertComposerTrue(t *testing.T, condition bool, message ...string) {
	t.Helper()
	if !condition {
		t.Fatalf("condition is false: %v", message)
	}
}

func assertComposerEqual(t *testing.T, want, got any, message ...string) {
	t.Helper()
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("got %#v, want %#v: %v", got, want, message)
	}
}
