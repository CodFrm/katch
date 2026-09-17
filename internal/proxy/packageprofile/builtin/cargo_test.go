package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"slices"
	"testing"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
)

func TestCargoProfileRegistersWithStaticCompanion(t *testing.T) {
	profile, ok := packageprofile.Lookup(upstream_entity.PackageProfileCargo)
	if !ok {
		t.Fatal("cargo profile is not registered")
	}
	if got := profile.Describe(); got.Profile != upstream_entity.PackageProfileCargo || got.Name != "Cargo" {
		t.Fatalf("description = %+v", got)
	}
	companions := profile.Companions()
	if len(companions) != 1 || companions[0].Host != "static.crates.io" || companions[0].Profile != upstream_entity.PackageProfileCargo || companions[0].Transport != upstream_entity.ProtocolStatic {
		t.Fatalf("companions = %+v", companions)
	}
}

func TestCargoProfileClassifiesSparseMetadataAndVersionedCrates(t *testing.T) {
	profile, ok := packageprofile.Lookup(upstream_entity.PackageProfileCargo)
	if !ok {
		t.Fatal("cargo profile is not registered")
	}
	tests := []struct {
		name      string
		host      string
		path      string
		class     packageprofile.Class
		transform bool
	}{
		{name: "sparse config", host: "index.crates.io", path: "/config.json", class: packageprofile.ClassMutable, transform: true},
		{name: "one character crate metadata", host: "index.crates.io", path: "/1/a", class: packageprofile.ClassMutable},
		{name: "two character crate metadata", host: "index.crates.io", path: "/2/ab", class: packageprofile.ClassMutable},
		{name: "three character crate metadata", host: "index.crates.io", path: "/3/a/abc", class: packageprofile.ClassMutable},
		{name: "long crate metadata", host: "index.crates.io", path: "/se/rd/serde", class: packageprofile.ClassMutable},
		{name: "versioned crate", host: "static.crates.io", path: "/crates/serde/serde-1.0.219.crate", class: packageprofile.ClassImmutable},
		{name: "config on static host", host: "static.crates.io", path: "/config.json", class: packageprofile.ClassUnknown},
		{name: "crate on index host", host: "index.crates.io", path: "/crates/serde/serde-1.0.219.crate", class: packageprofile.ClassUnknown},
		{name: "git index path", host: "index.crates.io", path: "/.git/HEAD", class: packageprofile.ClassUnknown},
		{name: "unversioned crate", host: "static.crates.io", path: "/crates/serde/latest.crate", class: packageprofile.ClassUnknown},
		{name: "unknown host", host: "mirror.example.com", path: "/config.json", class: packageprofile.ClassUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := profile.Classify(packageprofile.Request{Host: tc.host, Path: tc.path})
			if got.Class != tc.class || got.Transform != tc.transform {
				t.Fatalf("Classify(%q, %q) = %+v, want class %v transform %v", tc.host, tc.path, got, tc.class, tc.transform)
			}
			if tc.transform {
				wantMediaTypes := []string{"application/json", "application/octet-stream"}
				if !slices.Equal(got.MediaTypes, wantMediaTypes) {
					t.Fatalf("transform media types = %v, want %v", got.MediaTypes, wantMediaTypes)
				}
			}
		})
	}
}

func TestCargoTransformRewritesPlainDownloadBaseAndPreservesConfig(t *testing.T) {
	profile, ok := packageprofile.Lookup(upstream_entity.PackageProfileCargo)
	if !ok {
		t.Fatal("cargo profile is not registered")
	}
	body, err := os.ReadFile("testdata/cargo/config.json")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	result, err := profile.Transform(context.Background(), packageprofile.TransformRequest{
		Body: body, ContentType: "application/octet-stream",
		Source:      cargoTestURL(t, "https://index.crates.io/config.json"),
		SiteBaseURL: "https://katch.example.com",
		RewriteURL: func(_ context.Context, source *url.URL, companion packageprofile.Companion) (*url.URL, error) {
			calls++
			if source.String() != "https://static.crates.io/crates" {
				t.Fatalf("rewrite source = %q", source)
			}
			if companion.Host != "static.crates.io" || companion.Profile != upstream_entity.PackageProfileCargo || companion.Transport != upstream_entity.ProtocolStatic {
				t.Fatalf("companion = %+v", companion)
			}
			return url.Parse("https://katch.example.com/static.crates.io/crates")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("rewrite calls = %d", calls)
	}
	var got struct {
		Download     string `json:"dl"`
		API          string `json:"api"`
		AuthRequired bool   `json:"auth-required"`
	}
	if err := json.Unmarshal(result.Body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Download != "https://katch.example.com/static.crates.io/crates" {
		t.Fatalf("dl = %q", got.Download)
	}
	if got.API != "https://crates.io" || got.AuthRequired {
		t.Fatalf("trailing config fields changed: %+v", got)
	}
	if result.ContentType != "application/octet-stream" {
		t.Fatalf("content type = %q", result.ContentType)
	}
}

func TestCargoTransformRejectsUnsupportedConfig(t *testing.T) {
	profile, ok := packageprofile.Lookup(upstream_entity.PackageProfileCargo)
	if !ok {
		t.Fatal("cargo profile is not registered")
	}
	valid := []byte(`{"dl":"https://static.crates.io/crates","api":"https://crates.io"}`)
	successfulRewrite := func(_ context.Context, _ *url.URL, _ packageprofile.Companion) (*url.URL, error) {
		return url.Parse("https://katch.example.com/static.crates.io/crates")
	}
	tests := []struct {
		name string
		body []byte
		site string
		rw   packageprofile.RewriteURL
		want error
	}{
		{name: "missing site", body: valid, rw: successfulRewrite, want: packageprofile.ErrUnavailable},
		{name: "missing rewriter", body: valid, site: "https://katch.example.com", want: packageprofile.ErrUnavailable},
		{name: "missing companion", body: valid, site: "https://katch.example.com", rw: func(context.Context, *url.URL, packageprofile.Companion) (*url.URL, error) {
			return nil, packageprofile.ErrUnavailable
		}, want: packageprofile.ErrUnavailable},
		{name: "malformed json", body: []byte(`{`), site: "https://katch.example.com", rw: successfulRewrite, want: packageprofile.ErrInvalidMetadata},
		{name: "missing dl", body: []byte(`{"api":"https://crates.io"}`), site: "https://katch.example.com", rw: successfulRewrite, want: packageprofile.ErrInvalidMetadata},
		{name: "template", body: []byte(`{"dl":"https://static.crates.io/crates/{crate}/{crate}-{version}.crate"}`), site: "https://katch.example.com", rw: successfulRewrite, want: packageprofile.ErrInvalidMetadata},
		{name: "checksum template", body: []byte(`{"dl":"https://static.crates.io/crates/{sha256-checksum}"}`), site: "https://katch.example.com", rw: successfulRewrite, want: packageprofile.ErrInvalidMetadata},
		{name: "wrong host", body: []byte(`{"dl":"https://downloads.example.com/crates"}`), site: "https://katch.example.com", rw: successfulRewrite, want: packageprofile.ErrInvalidMetadata},
		{name: "wrong path", body: []byte(`{"dl":"https://static.crates.io/api/v1/crates"}`), site: "https://katch.example.com", rw: successfulRewrite, want: packageprofile.ErrInvalidMetadata},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := profile.Transform(context.Background(), packageprofile.TransformRequest{Body: tc.body, SiteBaseURL: tc.site, RewriteURL: tc.rw})
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func cargoTestURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
