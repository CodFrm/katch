package builtin

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
)

type rpmPathFixture struct {
	Path  string `json:"path"`
	Class string `json:"class"`
}

func TestRPMProfileRegistersAndClassifiesPaths(t *testing.T) {
	profile, ok := packageprofile.Lookup(upstream_entity.PackageProfileRPM)
	if !ok {
		t.Fatal("rpm profile is not registered")
	}
	if got := profile.Describe(); got.Profile != upstream_entity.PackageProfileRPM || got.Name != "rpm" {
		t.Fatalf("description = %+v", got)
	}

	body, err := os.ReadFile("testdata/rpm/paths.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []rpmPathFixture
	if err := json.Unmarshal(body, &fixtures); err != nil {
		t.Fatal(err)
	}
	classes := map[string]packageprofile.Class{
		"unknown":   packageprofile.ClassUnknown,
		"mutable":   packageprofile.ClassMutable,
		"immutable": packageprofile.ClassImmutable,
	}
	for _, fixture := range fixtures {
		t.Run(fixture.Path, func(t *testing.T) {
			got := profile.Classify(packageprofile.Request{Path: fixture.Path})
			if got.Class != classes[fixture.Class] || got.Transform || len(got.Variants) != 0 || len(got.MediaTypes) != 0 {
				t.Fatalf("Classify(%q) = %+v, want class %v without transformation", fixture.Path, got, classes[fixture.Class])
			}
		})
	}
}

func TestRPMProfilePreservesBodiesAndHasNoCompanions(t *testing.T) {
	profile, ok := packageprofile.Lookup(upstream_entity.PackageProfileRPM)
	if !ok {
		t.Fatal("rpm profile is not registered")
	}
	body := []byte{0x00, 0x01, 0x7f, 0x80, 0xff}
	result, err := profile.Transform(context.Background(), packageprofile.TransformRequest{
		Body:        body,
		ContentType: "application/x-rpm",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ContentType != "application/x-rpm" || !slices.Equal(result.Body, body) {
		t.Fatalf("transform result = %+v", result)
	}
	if len(profile.Companions()) != 0 {
		t.Fatalf("companions = %+v", profile.Companions())
	}
}

func TestRPMGuidanceUsesFixedBaseAndDisablesDynamicMirrors(t *testing.T) {
	profile, ok := packageprofile.Lookup(upstream_entity.PackageProfileRPM)
	if !ok {
		t.Fatal("rpm profile is not registered")
	}
	guidance := profile.Guidance()
	if guidance.Client != "dnf/yum" {
		t.Fatalf("client = %q", guidance.Client)
	}
	for _, required := range []string{
		"baseurl=https://<katch>/<rpm-host>/<repository-path>/",
		"mirrorlist=",
		"metalink=",
	} {
		if !slices.Contains(guidance.Configuration, required) {
			t.Errorf("guidance missing %q: %+v", required, guidance.Configuration)
		}
	}
}
