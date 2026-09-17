package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
)

func TestMavenProfileClassify(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("testdata/maven/paths.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Name  string `json:"name"`
		Host  string `json:"host"`
		Path  string `json:"path"`
		Class string `json:"class"`
	}
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}

	classes := map[string]packageprofile.Class{
		"unknown":   packageprofile.ClassUnknown,
		"mutable":   packageprofile.ClassMutable,
		"immutable": packageprofile.ClassImmutable,
	}
	profile := mavenProfile{}
	for _, fixture := range fixtures {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			got := profile.Classify(packageprofile.Request{Host: fixture.Host, Path: fixture.Path})
			if got.Class != classes[fixture.Class] {
				t.Fatalf("Classify() class = %v, want %v", got.Class, classes[fixture.Class])
			}
			if got.Transform {
				t.Fatal("Maven representations must remain byte-transparent")
			}
			if len(got.Variants) != 0 || len(got.MediaTypes) != 0 {
				t.Fatalf("Classify() unexpectedly declared variants or media types: %#v", got)
			}
		})
	}
}

func TestMavenProfileContract(t *testing.T) {
	t.Parallel()

	profile := mavenProfile{}
	description := profile.Describe()
	if description.Profile != upstream_entity.PackageProfileMaven || description.Name != "Maven" {
		t.Fatalf("Describe() = %#v", description)
	}
	if len(description.Variants) != 0 || len(profile.Companions()) != 0 {
		t.Fatal("Maven profile must not declare variants or companions")
	}

	guidance := profile.Guidance()
	if guidance.Client != "Maven / Gradle / sbt" || len(guidance.Configuration) == 0 {
		t.Fatalf("Guidance() = %#v", guidance)
	}

	body := []byte{0x00, 0xff, '<', '&', 0x00}
	result, err := profile.Transform(context.Background(), packageprofile.TransformRequest{
		Body:        body,
		ContentType: "application/octet-stream",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(result.Body, body) || result.ContentType != "application/octet-stream" {
		t.Fatalf("Transform() changed Maven bytes or content type: %#v", result)
	}
}

func TestMavenProfileRegistered(t *testing.T) {
	t.Parallel()

	profile, ok := packageprofile.Lookup(upstream_entity.PackageProfileMaven)
	if !ok {
		t.Fatal("Maven profile is not registered")
	}
	if got := profile.Describe().Profile; got != upstream_entity.PackageProfileMaven {
		t.Fatalf("registered profile = %q", got)
	}
}
