package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
)

type aptPathFixture struct {
	Path  string `json:"path"`
	Class string `json:"class"`
}

func TestAPTProfileClassifiesRepositoryPaths(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "apt", "paths.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []aptPathFixture
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}

	profile := aptProfile{}
	for _, fixture := range fixtures {
		t.Run(fixture.Path, func(t *testing.T) {
			got := profile.Classify(packageprofile.Request{Path: fixture.Path})
			if got.Class != aptFixtureClass(t, fixture.Class) {
				t.Fatalf("Classify(%q).Class = %v, want %s", fixture.Path, got.Class, fixture.Class)
			}
			if got.Transform {
				t.Fatalf("Classify(%q).Transform = true, want byte-transparent response", fixture.Path)
			}
			if len(got.Variants) != 0 || len(got.MediaTypes) != 0 {
				t.Fatalf("Classify(%q) added representation metadata: %#v", fixture.Path, got)
			}
		})
	}
}

func TestAPTProfilePreservesSignedMetadataBytes(t *testing.T) {
	profile := aptProfile{}
	body := []byte("-----BEGIN PGP SIGNED MESSAGE-----\r\nHash: SHA256\r\n\r\nSuite: stable\r\n\x00\xff\r\n-----BEGIN PGP SIGNATURE-----\r\nopaque-signature\r\n-----END PGP SIGNATURE-----\r\n")

	result, err := profile.Transform(context.Background(), packageprofile.TransformRequest{
		Body:        body,
		ContentType: "application/octet-stream",
	})
	if err != nil {
		t.Fatalf("Transform() error = %v", err)
	}
	if result == nil {
		t.Fatal("Transform() returned nil result")
	}
	if !bytes.Equal(result.Body, body) {
		t.Fatalf("Transform() changed signed body\ngot:  %q\nwant: %q", result.Body, body)
	}
	if result.ContentType != "application/octet-stream" {
		t.Fatalf("Transform() ContentType = %q", result.ContentType)
	}
}

func TestAPTProfileContract(t *testing.T) {
	profile := aptProfile{}
	description := profile.Describe()
	if description.Profile != upstream_entity.PackageProfileAPT || description.Name != "APT" {
		t.Fatalf("Describe() = %#v", description)
	}
	if len(description.Variants) != 0 {
		t.Fatalf("Describe().Variants = %#v", description.Variants)
	}
	if companions := profile.Companions(); len(companions) != 0 {
		t.Fatalf("Companions() = %#v", companions)
	}
	guidance := profile.Guidance()
	if guidance.Client != "apt" || len(guidance.Configuration) != 1 {
		t.Fatalf("Guidance() = %#v", guidance)
	}
}

func aptFixtureClass(t *testing.T, value string) packageprofile.Class {
	t.Helper()
	switch value {
	case "unknown":
		return packageprofile.ClassUnknown
	case "mutable":
		return packageprofile.ClassMutable
	case "immutable":
		return packageprofile.ClassImmutable
	default:
		t.Fatalf("unknown fixture class %q", value)
		return packageprofile.ClassUnknown
	}
}
