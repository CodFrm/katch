package packageprofile

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
)

var errInvalidRegistration = errors.New("invalid package profile registration")

// Registry holds independently registered package profiles.
type Registry struct {
	mu       sync.RWMutex
	profiles map[upstream_entity.PackageProfile]Profile
}

// NewRegistry returns an empty profile registry.
func NewRegistry() *Registry {
	return &Registry{profiles: make(map[upstream_entity.PackageProfile]Profile)}
}

// Register adds one profile and rejects invalid or duplicate registrations.
func (r *Registry) Register(profile Profile) error {
	if profile == nil {
		return errInvalidRegistration
	}
	description := profile.Describe()
	name := upstream_entity.NormalizePackageProfile(description.Profile)
	if name == upstream_entity.PackageProfileNone || !name.Valid() || description.Name == "" {
		return errInvalidRegistration
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.profiles[name]; exists {
		return fmt.Errorf("%w: %s", errInvalidRegistration, name)
	}
	r.profiles[name] = profile
	return nil
}

// Lookup returns the implementation registered for name.
func (r *Registry) Lookup(name upstream_entity.PackageProfile) (Profile, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	profile, ok := r.profiles[upstream_entity.NormalizePackageProfile(name)]
	return profile, ok
}

// Descriptions returns sorted copies suitable for APIs and user interfaces.
func (r *Registry) Descriptions() []Description {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Description, 0, len(r.profiles))
	for _, profile := range r.profiles {
		description := profile.Describe()
		description.Variants = append([]string(nil), description.Variants...)
		out = append(out, description)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Profile < out[j].Profile })
	return out
}

var defaultRegistry = NewRegistry()

// Register adds a built-in profile to the process registry.
func Register(profile Profile) error { return defaultRegistry.Register(profile) }

// MustRegister adds a built-in profile and panics on an invalid registration.
func MustRegister(profile Profile) {
	if err := Register(profile); err != nil {
		panic(err)
	}
}

// Lookup returns a built-in profile by configured name.
func Lookup(name upstream_entity.PackageProfile) (Profile, bool) {
	return defaultRegistry.Lookup(name)
}

// Descriptions returns all built-in profile descriptions.
func Descriptions() []Description { return defaultRegistry.Descriptions() }
