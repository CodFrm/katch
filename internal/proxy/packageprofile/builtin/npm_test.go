package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
)

func TestNPMProfileRegistersAndClassifiesRepresentations(t *testing.T) {
	profile, ok := packageprofile.Lookup(upstream_entity.PackageProfileNPM)
	if !ok {
		t.Fatal("npm profile is not registered")
	}
	if got := profile.Describe(); got.Profile != upstream_entity.PackageProfileNPM || got.Name == "" {
		t.Fatalf("description = %+v", got)
	}

	full := profile.Classify(packageprofile.Request{Path: "/@scope%2fpkg", Header: http.Header{
		"Accept": {"application/json"},
	}})
	install := profile.Classify(packageprofile.Request{Path: "/@scope%2fpkg", Header: http.Header{
		"Accept": {"application/vnd.npm.install-v1+json"},
	}})
	for name, got := range map[string]packageprofile.Representation{"full": full, "install-v1": install} {
		if got.Class != packageprofile.ClassMutable || !got.Transform || len(got.Variants) != 1 || got.Variants[0] != "Accept" {
			t.Errorf("%s representation = %+v", name, got)
		}
	}

	cases := []struct {
		path      string
		class     packageprofile.Class
		transform bool
	}{
		{path: "/left-pad", class: packageprofile.ClassMutable, transform: true},
		{path: "/@scope/pkg", class: packageprofile.ClassMutable, transform: true},
		{path: "/left-pad/-/left-pad-1.3.0.tgz", class: packageprofile.ClassImmutable},
		{path: "/@scope%2fpkg/-/pkg-2.0.0.tgz", class: packageprofile.ClassImmutable},
		{path: "/-/package/left-pad/dist-tags", class: packageprofile.ClassMutable},
		{path: "/-/package/@scope%2fpkg/dist-tags/latest", class: packageprofile.ClassMutable},
		{path: "/-/v1/search", class: packageprofile.ClassUnknown},
	}
	for _, tc := range cases {
		got := profile.Classify(packageprofile.Request{Path: tc.path})
		if got.Class != tc.class || got.Transform != tc.transform {
			t.Errorf("Classify(%q) = %+v, want class %v transform %v", tc.path, got, tc.class, tc.transform)
		}
	}
}

func TestNPMTransformRewritesEveryTarballWithoutRepresentationCollision(t *testing.T) {
	profile, ok := packageprofile.Lookup(upstream_entity.PackageProfileNPM)
	if !ok {
		t.Fatal("npm profile is not registered")
	}
	fixtures := []struct {
		name          string
		file          string
		wantVersions  int
		marker        string
		wantIntegrity string
		wantQuery     bool
	}{
		{name: "full", file: "testdata/npm/packument-full.json", wantVersions: 2, marker: "readme", wantIntegrity: "sha512-full-two", wantQuery: true},
		{name: "install-v1", file: "testdata/npm/packument-install-v1.json", wantVersions: 1, marker: "installVersion", wantIntegrity: "sha512-install-two"},
	}
	for _, tc := range fixtures {
		t.Run(tc.name, func(t *testing.T) {
			body, err := os.ReadFile(tc.file)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			result, err := profile.Transform(context.Background(), packageprofile.TransformRequest{
				Body: body, ContentType: "application/json",
				Source:      mustURL(t, "https://registry.npmjs.org/@scope%2fpkg"),
				SiteBaseURL: "https://katch.example.com",
				RewriteURL: func(_ context.Context, source *url.URL, companion packageprofile.Companion) (*url.URL, error) {
					calls++
					if companion.Host != "registry.npmjs.org" || companion.Profile != upstream_entity.PackageProfileNPM || companion.Transport != upstream_entity.ProtocolStatic {
						t.Fatalf("companion = %+v", companion)
					}
					return url.Parse("https://katch.example.com/registry.npmjs.org" + source.EscapedPath() + querySuffix(source))
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Readme         json.RawMessage   `json:"readme"`
				InstallVersion json.RawMessage   `json:"installVersion"`
				DistTags       map[string]string `json:"dist-tags"`
				Versions       map[string]struct {
					Name    string      `json:"name"`
					Version string      `json:"version"`
					Dist    npmTestDist `json:"dist"`
				} `json:"versions"`
			}
			if err := json.Unmarshal(result.Body, &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Versions) != tc.wantVersions || calls != tc.wantVersions {
				t.Fatalf("versions = %d, rewrite calls = %d", len(got.Versions), calls)
			}
			if tc.marker == "readme" && len(got.Readme) == 0 || tc.marker == "installVersion" && len(got.InstallVersion) == 0 {
				t.Fatalf("representation marker %q missing from %s", tc.marker, result.Body)
			}
			preservedIntegrity := false
			for key, version := range got.Versions {
				if version.Name != "@scope/pkg" || version.Version != key {
					t.Errorf("version data changed for %q: %+v", key, version)
				}
				if !strings.HasPrefix(version.Dist.Tarball, "https://katch.example.com/registry.npmjs.org/@scope%2fpkg/") {
					t.Errorf("tarball leaked or escaping changed: %q", version.Dist.Tarball)
				}
				if version.Dist.Integrity == tc.wantIntegrity {
					preservedIntegrity = version.Dist.Shasum != ""
				}
			}
			if !preservedIntegrity || got.DistTags["latest"] != "2.0.0" {
				t.Error("integrity, shasum, or dist-tags were not preserved")
			}
			if tc.wantQuery && !strings.Contains(string(result.Body), "download=1") {
				t.Error("tarball query was not preserved")
			}
			if strings.Contains(string(result.Body), "\"https://registry.npmjs.org/") {
				t.Fatalf("original dist.tarball leaked: %s", result.Body)
			}
		})
	}
}

func TestNPMTransformFailsClosed(t *testing.T) {
	profile, ok := packageprofile.Lookup(upstream_entity.PackageProfileNPM)
	if !ok {
		t.Fatal("npm profile is not registered")
	}
	valid := []byte(`{"name":"pkg","versions":{"1.0.0":{"dist":{"tarball":"https://registry.npmjs.org/pkg/-/pkg-1.0.0.tgz"}}}}`)
	tests := []struct {
		name string
		in   packageprofile.TransformRequest
		want error
	}{
		{name: "missing site", in: packageprofile.TransformRequest{Body: valid, RewriteURL: successfulNPMRewrite}, want: packageprofile.ErrUnavailable},
		{name: "missing rewriter", in: packageprofile.TransformRequest{Body: valid, SiteBaseURL: "https://katch.example.com"}, want: packageprofile.ErrUnavailable},
		{name: "missing companion", in: packageprofile.TransformRequest{Body: valid, SiteBaseURL: "https://katch.example.com", RewriteURL: unavailableNPMRewrite}, want: packageprofile.ErrUnavailable},
		{name: "malformed", in: packageprofile.TransformRequest{Body: []byte(`{`), SiteBaseURL: "https://katch.example.com", RewriteURL: successfulNPMRewrite}, want: packageprofile.ErrInvalidMetadata},
		{name: "missing tarball", in: packageprofile.TransformRequest{Body: []byte(`{"versions":{"1.0.0":{"dist":{}}}}`), SiteBaseURL: "https://katch.example.com", RewriteURL: successfulNPMRewrite}, want: packageprofile.ErrInvalidMetadata},
		{name: "wrong host", in: packageprofile.TransformRequest{Body: []byte(`{"versions":{"1.0.0":{"dist":{"tarball":"https://evil.example/pkg.tgz"}}}}`), SiteBaseURL: "https://katch.example.com", RewriteURL: successfulNPMRewrite}, want: packageprofile.ErrInvalidMetadata},
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

type npmTestDist struct {
	Tarball   string `json:"tarball"`
	Integrity string `json:"integrity"`
	Shasum    string `json:"shasum"`
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func querySuffix(source *url.URL) string {
	if source.RawQuery == "" {
		return ""
	}
	return "?" + source.RawQuery
}

func unavailableNPMRewrite(context.Context, *url.URL, packageprofile.Companion) (*url.URL, error) {
	return nil, packageprofile.ErrUnavailable
}

func successfulNPMRewrite(_ context.Context, source *url.URL, _ packageprofile.Companion) (*url.URL, error) {
	return url.Parse("https://katch.example.com/registry.npmjs.org" + source.EscapedPath())
}
