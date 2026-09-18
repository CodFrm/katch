package builtin

import (
	"testing"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
)

func TestRegisteredProfilesDeclareCompleteRuntimeGuidance(t *testing.T) {
	runtimeSupported := map[upstream_entity.PackageProfile]bool{
		upstream_entity.PackageProfileNPM:      true,
		upstream_entity.PackageProfilePyPI:     true,
		upstream_entity.PackageProfileGoProxy:  true,
		upstream_entity.PackageProfileMaven:    true,
		upstream_entity.PackageProfileCargo:    true,
		upstream_entity.PackageProfileNuGet:    true,
		upstream_entity.PackageProfileRubyGems: true,
		upstream_entity.PackageProfileAPT:      true,
		upstream_entity.PackageProfileRPM:      true,
		upstream_entity.PackageProfileAPK:      true,
		upstream_entity.PackageProfileComposer: true,
		upstream_entity.PackageProfileHomebrew: true,
	}

	descriptions := packageprofile.Descriptions()
	if len(descriptions) != len(runtimeSupported) {
		t.Fatalf("registered profiles = %d, runtime-supported profiles = %d", len(descriptions), len(runtimeSupported))
	}
	seen := make(map[upstream_entity.PackageProfile]bool, len(descriptions))
	for _, description := range descriptions {
		seen[description.Profile] = true
		profile, ok := packageprofile.Lookup(description.Profile)
		if !ok {
			t.Fatalf("registered profile %q cannot be looked up", description.Profile)
		}
		guidance := profile.Guidance()
		if len(guidance.Clients) == 0 {
			t.Errorf("profile %q has no guidance clients", description.Profile)
		}
		if len(guidance.Configuration) == 0 {
			t.Errorf("profile %q has no guidance configuration", description.Profile)
		}
		if len(guidance.Constraints) == 0 {
			t.Errorf("profile %q has no guidance constraints", description.Profile)
		}
		if runtimeSupported[description.Profile] != guidance.RuntimeVerified {
			t.Errorf("profile %q runtime_verified = %t, want %t",
				description.Profile, guidance.RuntimeVerified, runtimeSupported[description.Profile])
		}
	}
	for profile := range runtimeSupported {
		if !seen[profile] {
			t.Errorf("runtime-supported profile %q is not registered", profile)
		}
	}
}
