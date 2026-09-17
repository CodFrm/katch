package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
)

type rubyGemsPathCase struct {
	Path     string `json:"path"`
	RawQuery string `json:"raw_query"`
	Class    string `json:"class"`
}

func TestRubyGemsProfileRegistersAndClassifiesPaths(t *testing.T) {
	profile, ok := packageprofile.Lookup(upstream_entity.PackageProfileRubyGems)
	if !ok {
		t.Fatal("rubygems profile is not registered")
	}
	if got := profile.Describe(); got.Profile != upstream_entity.PackageProfileRubyGems || got.Name != "RubyGems" || len(got.Variants) != 0 {
		t.Fatalf("description = %+v", got)
	}

	body, err := os.ReadFile("testdata/rubygems/paths.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []rubyGemsPathCase
	if err := json.Unmarshal(body, &cases); err != nil {
		t.Fatal(err)
	}
	classes := map[string]packageprofile.Class{
		"unknown":   packageprofile.ClassUnknown,
		"mutable":   packageprofile.ClassMutable,
		"immutable": packageprofile.ClassImmutable,
	}
	for _, tc := range cases {
		t.Run(tc.Path, func(t *testing.T) {
			got := profile.Classify(packageprofile.Request{Path: tc.Path, RawQuery: tc.RawQuery})
			if got.Class != classes[tc.Class] || got.Transform || len(got.Variants) != 0 || len(got.MediaTypes) != 0 {
				t.Fatalf("Classify(%q) = %+v, want class %v with transparent representation", tc.Path, got, classes[tc.Class])
			}
		})
	}

	compact := profile.Classify(packageprofile.Request{Path: "/versions", Header: http.Header{
		"Range":         {"bytes=10-"},
		"If-None-Match": {`"compact-index-etag"`},
	}})
	if compact.Class != packageprofile.ClassMutable || compact.Transform || len(compact.Variants) != 0 {
		t.Fatalf("compact index cache semantics were not delegated: %+v", compact)
	}
}

func TestRubyGemsProfileIsByteTransparentAndSelfContained(t *testing.T) {
	profile, ok := packageprofile.Lookup(upstream_entity.PackageProfileRubyGems)
	if !ok {
		t.Fatal("rubygems profile is not registered")
	}
	body := []byte{0x04, 0x08, 0x00, 0xff, '\n'}
	result, err := profile.Transform(context.Background(), packageprofile.TransformRequest{
		Body: body, ContentType: "application/octet-stream",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(result.Body, body) || result.ContentType != "application/octet-stream" {
		t.Fatalf("transform changed representation: %+v", result)
	}
	if len(profile.Companions()) != 0 {
		t.Fatalf("companions = %+v, want none", profile.Companions())
	}
	guidance := profile.Guidance()
	if guidance.Client != "RubyGems / Bundler" || len(guidance.Configuration) != 2 {
		t.Fatalf("guidance = %+v", guidance)
	}
}
