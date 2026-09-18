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

func TestAPKClassifyPaths(t *testing.T) {
	data, err := os.ReadFile("testdata/apk/paths.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Path  string `json:"path"`
		Class string `json:"class"`
	}
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}

	wantClasses := map[string]packageprofile.Class{
		"unknown":   packageprofile.ClassUnknown,
		"mutable":   packageprofile.ClassMutable,
		"immutable": packageprofile.ClassImmutable,
	}
	profile := apkProfile{}
	for _, fixture := range fixtures {
		t.Run(fixture.Path, func(t *testing.T) {
			got := profile.Classify(packageprofile.Request{Path: fixture.Path})
			if got.Class != wantClasses[fixture.Class] {
				t.Fatalf("Classify(%q).Class = %v, want %v", fixture.Path, got.Class, wantClasses[fixture.Class])
			}
			if got.Transform {
				t.Fatalf("Classify(%q).Transform = true, signed APK data must stay byte-transparent", fixture.Path)
			}
			if got.Recognized() {
				if len(got.Variants) != 1 || got.Variants[0] != "Origin" {
					t.Fatalf("Classify(%q).Variants = %#v, want []string{\"Origin\"}", fixture.Path, got.Variants)
				}
			} else if len(got.Variants) != 0 {
				t.Fatalf("Classify(%q).Variants = %#v, unknown paths must not declare variants", fixture.Path, got.Variants)
			}
		})
	}
}

func TestAPKTransformPreservesSignedBytes(t *testing.T) {
	body := []byte{0x1f, 0x8b, 0x08, 0x00, 0xff, 0x00, 0x7f}
	original := append([]byte(nil), body...)
	result, err := (apkProfile{}).Transform(context.Background(), packageprofile.TransformRequest{
		Body:        body,
		ContentType: "application/vnd.alpine.apk",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(result.Body, original) || !bytes.Equal(body, original) {
		t.Fatalf("Transform changed signed bytes: result %x, input %x, want %x", result.Body, body, original)
	}
	if result.ContentType != "application/vnd.alpine.apk" {
		t.Fatalf("ContentType = %q", result.ContentType)
	}
}

func TestAPKProfileContract(t *testing.T) {
	profile := apkProfile{}
	description := profile.Describe()
	if description.Profile != upstream_entity.PackageProfileAPK || description.Name != "Alpine APK" {
		t.Fatalf("description = %+v", description)
	}
	if len(profile.Companions()) != 0 {
		t.Fatalf("companions = %+v", profile.Companions())
	}
	guidance := profile.Guidance()
	if len(guidance.Clients) != 1 || guidance.Clients[0] != "apk" || len(guidance.Configuration) != 1 {
		t.Fatalf("guidance = %+v", guidance)
	}
	registered, ok := packageprofile.Lookup(upstream_entity.PackageProfileAPK)
	if !ok || registered.Describe().Profile != upstream_entity.PackageProfileAPK {
		t.Fatalf("registered profile = %#v, %v", registered, ok)
	}
}
