package packageprofile

import (
	"context"
	"net/http"
	"testing"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
)

type contractProfile struct {
	description Description
}

func (p contractProfile) Describe() Description { return p.description }
func (contractProfile) Classify(Request) Representation {
	return Representation{Class: ClassMutable, Transform: true, Variants: []string{"Accept"}, MediaTypes: []string{"application/json"}}
}
func (contractProfile) Transform(_ context.Context, in TransformRequest) (*TransformResult, error) {
	return &TransformResult{Body: append([]byte("rewritten:"), in.Body...), ContentType: "application/json"}, nil
}
func (contractProfile) Companions() []Companion {
	return []Companion{{Host: "cdn.example.com", Profile: upstream_entity.PackageProfileNPM, Transport: upstream_entity.ProtocolStatic}}
}
func (contractProfile) Guidance() Guidance {
	return Guidance{Client: "npm", Configuration: []string{"registry"}}
}

func TestProfileContract(t *testing.T) {
	profile := contractProfile{description: Description{
		Profile: upstream_entity.PackageProfileNPM,
		Name:    "npm",
	}}

	description := profile.Describe()
	if description.Profile != upstream_entity.PackageProfileNPM || description.Name != "npm" {
		t.Fatalf("description = %+v", description)
	}
	representation := profile.Classify(Request{Path: "/pkg", Header: http.Header{"Accept": {"application/json"}}})
	if !representation.Recognized() || !representation.Transform || representation.Class != ClassMutable {
		t.Fatalf("representation = %+v", representation)
	}
	result, err := profile.Transform(context.Background(), TransformRequest{Body: []byte("body")})
	if err != nil || string(result.Body) != "rewritten:body" {
		t.Fatalf("transform result = %+v, err = %v", result, err)
	}
	if len(profile.Companions()) != 1 || profile.Guidance().Client != "npm" {
		t.Fatal("companion and guidance contracts are not usable")
	}
}

func TestRegistryRegistrationAndDescriptionIsolation(t *testing.T) {
	registry := NewRegistry()
	profile := contractProfile{description: Description{
		Profile:  upstream_entity.PackageProfileNPM,
		Name:     "npm",
		Variants: []string{"Accept"},
	}}
	if err := registry.Register(profile); err != nil {
		t.Fatal(err)
	}
	if got, ok := registry.Lookup(upstream_entity.PackageProfileNPM); !ok || got.Describe().Name != "npm" {
		t.Fatalf("lookup = %#v, %v", got, ok)
	}

	descriptions := registry.Descriptions()
	if len(descriptions) != 1 || descriptions[0].Profile != upstream_entity.PackageProfileNPM {
		t.Fatalf("descriptions = %+v", descriptions)
	}
	descriptions[0].Variants[0] = "User-Agent"
	again := registry.Descriptions()
	if again[0].Variants[0] != "Accept" {
		t.Fatalf("registry description was mutated through caller: %+v", again[0])
	}

	if err := registry.Register(profile); err == nil {
		t.Fatal("duplicate profile registration succeeded")
	}
	invalid := contractProfile{description: Description{Profile: upstream_entity.PackageProfileNone, Name: "none"}}
	if err := registry.Register(invalid); err == nil {
		t.Fatal("none profile registration succeeded")
	}
}
