package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
)

func TestNuGetProfileRegistersDeclaresCompanionsAndClassifies(t *testing.T) {
	profile, ok := packageprofile.Lookup(upstream_entity.PackageProfileNuGet)
	if !ok {
		t.Fatal("nuget profile is not registered")
	}
	if got := profile.Describe(); got.Profile != upstream_entity.PackageProfileNuGet || got.Name != "NuGet" {
		t.Fatalf("description = %+v", got)
	}

	wantCompanions := []packageprofile.Companion{
		{Host: "api.nuget.org", Profile: upstream_entity.PackageProfileNuGet, Transport: upstream_entity.ProtocolStatic},
		{Host: "azuresearch-usnc.nuget.org", Profile: upstream_entity.PackageProfileNuGet, Transport: upstream_entity.ProtocolStatic},
		{Host: "www.nuget.org", Profile: upstream_entity.PackageProfileNuGet, Transport: upstream_entity.ProtocolStatic},
	}
	if got := profile.Companions(); !reflect.DeepEqual(got, wantCompanions) {
		t.Fatalf("companions = %+v, want %+v", got, wantCompanions)
	}

	tests := []struct {
		name      string
		host      string
		path      string
		class     packageprofile.Class
		transform bool
	}{
		{name: "service index", host: "api.nuget.org", path: "/v3/index.json", class: packageprofile.ClassMutable, transform: true},
		{name: "registration index", host: "api.nuget.org", path: "/v3/registration5-gz-semver2/example.package/index.json", class: packageprofile.ClassMutable, transform: true},
		{name: "registration leaf", host: "api.nuget.org", path: "/v3/registration5-semver1/example.package/1.0.0.json", class: packageprofile.ClassMutable, transform: true},
		{name: "search", host: "azuresearch-usnc.nuget.org", path: "/query", class: packageprofile.ClassMutable, transform: true},
		{name: "autocomplete", host: "azuresearch-usnc.nuget.org", path: "/autocomplete", class: packageprofile.ClassMutable, transform: true},
		{name: "package versions", host: "api.nuget.org", path: "/v3-flatcontainer/example.package/index.json", class: packageprofile.ClassMutable},
		{name: "package", host: "api.nuget.org", path: "/v3-flatcontainer/example.package/1.0.0/example.package.1.0.0.nupkg", class: packageprofile.ClassImmutable},
		{name: "package hash", host: "api.nuget.org", path: "/v3-flatcontainer/example.package/1.0.0/example.package.1.0.0.nupkg.sha512", class: packageprofile.ClassImmutable},
		{name: "nuspec", host: "api.nuget.org", path: "/v3-flatcontainer/example.package/1.0.0/example.package.nuspec", class: packageprofile.ClassImmutable},
		{name: "catalog entry", host: "api.nuget.org", path: "/v3/catalog0/data/2026.09.16.00.00.00/example.package.1.0.0.json", class: packageprofile.ClassImmutable, transform: true},
		{name: "repository signatures", host: "api.nuget.org", path: "/v3-index/repository-signatures/index.json", class: packageprofile.ClassMutable, transform: true},
		{name: "publish remains unsupported", host: "www.nuget.org", path: "/api/v2/package", class: packageprofile.ClassUnknown},
		{name: "lookalike registration", host: "api.nuget.org", path: "/v3/registration-evil/package/index.json", class: packageprofile.ClassUnknown},
		{name: "wrong host", host: "example.com", path: "/v3/index.json", class: packageprofile.ClassUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := profile.Classify(packageprofile.Request{Host: tc.host, Path: tc.path, Header: http.Header{"Accept": {"application/json"}}})
			if got.Class != tc.class || got.Transform != tc.transform {
				t.Fatalf("Classify(%q, %q) = %+v, want class %v transform %v", tc.host, tc.path, got, tc.class, tc.transform)
			}
		})
	}
}

func TestNuGetTransformRewritesProtocolNavigationOnly(t *testing.T) {
	profile := mustNuGetProfile(t)
	tests := []struct {
		name       string
		file       string
		source     string
		wantCalls  int
		wantSuffix []string
	}{
		{
			name: "service index", file: "testdata/nuget/service-index.json", source: "https://api.nuget.org/v3/index.json", wantCalls: 4,
			wantSuffix: []string{"/api.nuget.org/v3/registration5-gz-semver2/", "/azuresearch-usnc.nuget.org/query?semVerLevel=2.0.0", "/api.nuget.org/v3-flatcontainer/", "/api.nuget.org/v3-index/repository-signatures/index.json"},
		},
		{
			name: "registration", file: "testdata/nuget/registration.json", source: "https://api.nuget.org/v3/registration5-gz-semver2/example.package/index.json", wantCalls: 5,
			wantSuffix: []string{"/api.nuget.org/v3/registration5-gz-semver2/example.package/index.json", "/api.nuget.org/v3/registration5-gz-semver2/example.package/page/1.0.0/1.0.0.json", "/api.nuget.org/v3-flatcontainer/example.package/1.0.0/example.package.1.0.0.nupkg?download=true", "/api.nuget.org/v3/catalog0/data/2026.09.16.00.00.00/example.package.1.0.0.json"},
		},
		{
			name: "search", file: "testdata/nuget/search.json", source: "https://azuresearch-usnc.nuget.org/query?q=example", wantCalls: 4,
			wantSuffix: []string{"/api.nuget.org/v3/registration5-gz-semver2/example.package/index.json", "/api.nuget.org/v3-flatcontainer/example.package/1.0.0/example.package.1.0.0.nupkg", "/api.nuget.org/v3/registration5-gz-semver2/example.package/1.0.0.json"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body, err := os.ReadFile(tc.file)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			result, err := profile.Transform(context.Background(), packageprofile.TransformRequest{
				Body: body, ContentType: "application/json; charset=utf-8", Source: mustURL(t, tc.source), SiteBaseURL: "https://katch.example.com/base",
				RewriteURL: func(_ context.Context, source *url.URL, companion packageprofile.Companion) (*url.URL, error) {
					calls++
					if companion.Host != source.Hostname() || companion.Profile != upstream_entity.PackageProfileNuGet || companion.Transport != upstream_entity.ProtocolStatic {
						t.Fatalf("source %s used companion %+v", source, companion)
					}
					return url.Parse("https://katch.example.com/" + source.Host + source.EscapedPath() + querySuffix(source))
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if calls != tc.wantCalls {
				t.Fatalf("rewrite calls = %d, want %d", calls, tc.wantCalls)
			}
			if result.ContentType != "application/json; charset=utf-8" {
				t.Fatalf("content type = %q", result.ContentType)
			}
			for _, suffix := range tc.wantSuffix {
				if !strings.Contains(string(result.Body), "https://katch.example.com"+suffix) {
					t.Errorf("rewritten body missing %q: %s", suffix, result.Body)
				}
			}
			for _, preserved := range []string{"https://project.example/package", "https://licenses.example/license", "https://images.example/icon.png", "https://docs.example/package", "https://learn.microsoft.com/nuget/api/service-index"} {
				if strings.Contains(string(body), preserved) && !strings.Contains(string(result.Body), preserved) {
					t.Errorf("non-protocol URL %q changed", preserved)
				}
			}
			var decoded any
			if err := json.Unmarshal(result.Body, &decoded); err != nil {
				t.Fatalf("result is not JSON: %v", err)
			}
		})
	}
}

func TestNuGetTransformFailsClosed(t *testing.T) {
	profile := mustNuGetProfile(t)
	valid := []byte(`{"resources":[{"@id":"https://api.nuget.org/v3-flatcontainer/","@type":"PackageBaseAddress/3.0.0"}]}`)
	tests := []struct {
		name string
		in   packageprofile.TransformRequest
		want error
	}{
		{name: "missing site", in: packageprofile.TransformRequest{Body: valid, Source: mustURL(t, "https://api.nuget.org/v3/index.json"), RewriteURL: successfulNuGetRewrite}, want: packageprofile.ErrUnavailable},
		{name: "missing rewriter", in: packageprofile.TransformRequest{Body: valid, Source: mustURL(t, "https://api.nuget.org/v3/index.json"), SiteBaseURL: "https://katch.example.com"}, want: packageprofile.ErrUnavailable},
		{name: "missing companion", in: packageprofile.TransformRequest{Body: valid, Source: mustURL(t, "https://api.nuget.org/v3/index.json"), SiteBaseURL: "https://katch.example.com", RewriteURL: unavailableNuGetRewrite}, want: packageprofile.ErrUnavailable},
		{name: "malformed", in: packageprofile.TransformRequest{Body: []byte(`{`), Source: mustURL(t, "https://api.nuget.org/v3/index.json"), SiteBaseURL: "https://katch.example.com", RewriteURL: successfulNuGetRewrite}, want: packageprofile.ErrInvalidMetadata},
		{name: "unknown host", in: packageprofile.TransformRequest{Body: []byte(`{"@id":"https://evil.example/v3/index.json"}`), Source: mustURL(t, "https://api.nuget.org/v3/index.json"), SiteBaseURL: "https://katch.example.com", RewriteURL: successfulNuGetRewrite}, want: packageprofile.ErrInvalidMetadata},
		{name: "userinfo", in: packageprofile.TransformRequest{Body: []byte(`{"packageContent":"https://user@api.nuget.org/v3-flatcontainer/p/1.0.0/p.1.0.0.nupkg"}`), Source: mustURL(t, "https://api.nuget.org/v3/index.json"), SiteBaseURL: "https://katch.example.com", RewriteURL: successfulNuGetRewrite}, want: packageprofile.ErrInvalidMetadata},
		{name: "downgrade", in: packageprofile.TransformRequest{Body: []byte(`{"registration":"http://api.nuget.org/v3/registration5/p/index.json"}`), Source: mustURL(t, "https://api.nuget.org/v3/index.json"), SiteBaseURL: "https://katch.example.com", RewriteURL: successfulNuGetRewrite}, want: packageprofile.ErrInvalidMetadata},
		{name: "relative without source", in: packageprofile.TransformRequest{Body: []byte(`{"@id":"page.json"}`), SiteBaseURL: "https://katch.example.com", RewriteURL: successfulNuGetRewrite}, want: packageprofile.ErrInvalidMetadata},
		{name: "non-string navigation", in: packageprofile.TransformRequest{Body: []byte(`{"packageContent":42}`), Source: mustURL(t, "https://api.nuget.org/v3/index.json"), SiteBaseURL: "https://katch.example.com", RewriteURL: successfulNuGetRewrite}, want: packageprofile.ErrInvalidMetadata},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := profile.Transform(context.Background(), tc.in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func mustNuGetProfile(t *testing.T) packageprofile.Profile {
	t.Helper()
	profile, ok := packageprofile.Lookup(upstream_entity.PackageProfileNuGet)
	if !ok {
		t.Fatal("nuget profile is not registered")
	}
	return profile
}

func unavailableNuGetRewrite(context.Context, *url.URL, packageprofile.Companion) (*url.URL, error) {
	return nil, packageprofile.ErrUnavailable
}

func successfulNuGetRewrite(_ context.Context, source *url.URL, _ packageprofile.Companion) (*url.URL, error) {
	return url.Parse("https://katch.example.com/" + source.Host + source.EscapedPath() + querySuffix(source))
}
