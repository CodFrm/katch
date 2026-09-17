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

func TestComposerProfileRewritesRootMetadataTemplateLiterally(t *testing.T) {
	body := readComposerFixture(t, "packages.json")
	profile := composerProfile{}

	result, err := profile.Transform(context.Background(), packageprofile.TransformRequest{
		Body:        body,
		ContentType: "application/json",
		Source:      mustComposerURL(t, "https://repo.packagist.org/packages.json"),
		SiteBaseURL: "https://mirror.example",
		RewriteURL: composerTestRewriter(map[string]bool{
			"repo.packagist.org": true,
		}),
	})
	requireComposerNoError(t, err)
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

	result, err := profile.Transform(context.Background(), packageprofile.TransformRequest{
		Body:        body,
		ContentType: "application/json; charset=utf-8",
		Source:      mustComposerURL(t, "https://repo.packagist.org/p2/acme/widget.json"),
		SiteBaseURL: "https://mirror.example",
		RewriteURL: composerTestRewriter(map[string]bool{
			"api.github.com": true,
		}),
	})
	requireComposerNoError(t, err)

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

func TestComposerProfileFailsClosedWithoutDistCompanion(t *testing.T) {
	profile := composerProfile{}

	_, err := profile.Transform(context.Background(), packageprofile.TransformRequest{
		Body:        readComposerFixture(t, "package.json"),
		ContentType: "application/json",
		Source:      mustComposerURL(t, "https://repo.packagist.org/p2/acme/widget.json"),
		SiteBaseURL: "https://mirror.example",
		RewriteURL:  composerTestRewriter(nil),
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
	assertComposerEqual(t, 0, len(profile.Companions()))
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

func composerTestRewriter(registered map[string]bool) packageprofile.RewriteURL {
	return func(_ context.Context, source *url.URL, companion packageprofile.Companion) (*url.URL, error) {
		if companion.Host != source.Hostname() || companion.Transport != "static" || !registered[source.Hostname()] {
			return nil, packageprofile.ErrUnavailable
		}
		rewritten := *source
		rewritten.Scheme = "https"
		rewritten.Host = "mirror.example"
		rewritten.Path = "/" + source.Host + source.Path
		rewritten.RawPath = ""
		return &rewritten, nil
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
