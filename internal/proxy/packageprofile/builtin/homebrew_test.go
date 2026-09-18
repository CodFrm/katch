package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
)

type homebrewPathFixture struct {
	Path  string `json:"path"`
	Class string `json:"class"`
}

func TestHomebrewProfileRegistersAndClassifiesOnlyAPIRepresentations(t *testing.T) {
	profile, ok := packageprofile.Lookup(upstream_entity.PackageProfileHomebrew)
	if !ok {
		t.Fatal("homebrew profile is not registered")
	}
	if got := profile.Describe(); got.Profile != upstream_entity.PackageProfileHomebrew ||
		got.Name != "homebrew" || len(got.Variants) != 0 {
		t.Fatalf("description = %+v", got)
	}

	body, err := os.ReadFile("testdata/homebrew/paths.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []homebrewPathFixture
	if err := json.Unmarshal(body, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		t.Run(fixture.Path, func(t *testing.T) {
			got := profile.Classify(packageprofile.Request{Path: fixture.Path})
			want := packageprofile.ClassUnknown
			if fixture.Class == "mutable" {
				want = packageprofile.ClassMutable
			}
			if got.Class != want || got.Transform || len(got.Variants) != 0 || len(got.MediaTypes) != 0 {
				t.Fatalf("Classify(%q) = %+v, want class %v without transformation", fixture.Path, got, want)
			}
		})
	}
}

func TestHomebrewAPIAndJWSRemainByteIdentical(t *testing.T) {
	profile, ok := packageprofile.Lookup(upstream_entity.PackageProfileHomebrew)
	if !ok {
		t.Fatal("homebrew profile is not registered")
	}
	body := []byte{'{', '"', 'p', 'a', 'y', 'l', 'o', 'a', 'd', '"', ':', '"', 0xff, '"', '}', '\n', 0}
	result, err := profile.Transform(context.Background(), packageprofile.TransformRequest{
		Body: body, ContentType: "application/jose+json; charset=utf-8",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(result.Body, body) {
		t.Fatalf("body changed: got %x, want %x", result.Body, body)
	}
	if result.ContentType != "application/jose+json; charset=utf-8" {
		t.Fatalf("content type = %q", result.ContentType)
	}
}

func TestHomebrewBottleAndTapConfigurationContract(t *testing.T) {
	profile, ok := packageprofile.Lookup(upstream_entity.PackageProfileHomebrew)
	if !ok {
		t.Fatal("homebrew profile is not registered")
	}
	wantCompanions := []packageprofile.Companion{
		{Host: "ghcr.io", Profile: upstream_entity.PackageProfileNone, Transport: upstream_entity.ProtocolRegistry},
		{Host: "pkg-containers.githubusercontent.com", Profile: upstream_entity.PackageProfileNone, Transport: upstream_entity.ProtocolStatic},
		{Host: "github.com", Profile: upstream_entity.PackageProfileNone, Transport: upstream_entity.ProtocolGit},
	}
	gotCompanions := profile.Companions()
	if len(gotCompanions) != len(wantCompanions) {
		t.Fatalf("companions = %+v", gotCompanions)
	}
	for i := range wantCompanions {
		if gotCompanions[i] != wantCompanions[i] {
			t.Fatalf("companion[%d] = %+v, want %+v", i, gotCompanions[i], wantCompanions[i])
		}
	}

	guidance := profile.Guidance()
	wantConfiguration := []string{
		"export HOMEBREW_API_DOMAIN=https://<katch>/<upstream>/api",
		"export HOMEBREW_ARTIFACT_DOMAIN=https://<katch>/registry/ghcr.io",
		"export HOMEBREW_ARTIFACT_DOMAIN_NO_FALLBACK=1",
		"export HOMEBREW_BREW_GIT_REMOTE=https://<katch>/github.com/Homebrew/brew.git",
		"export HOMEBREW_CORE_GIT_REMOTE=https://<katch>/github.com/Homebrew/homebrew-core.git",
	}
	if len(guidance.Clients) != 1 || guidance.Clients[0] != "homebrew" ||
		len(guidance.Configuration) != len(wantConfiguration) {
		t.Fatalf("guidance = %+v", guidance)
	}
	for i := range wantConfiguration {
		if guidance.Configuration[i] != wantConfiguration[i] {
			t.Fatalf("configuration[%d] = %q, want %q", i, guidance.Configuration[i], wantConfiguration[i])
		}
	}
	advertised := strings.ToLower(strings.Join(guidance.Configuration, "\n"))
	if strings.Contains(advertised, "build-from-source") || strings.Contains(advertised, "source_url") ||
		strings.Contains(advertised, "bottle_domain") {
		t.Fatalf("unsupported source or legacy bottle configuration advertised: %s", advertised)
	}
}
