package destination

import (
	"context"
	"errors"
	"net/netip"
	"net/url"
	"testing"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
)

type staticSource struct {
	snapshot *RewriteSnapshot
	err      error
}

func (s staticSource) Snapshot(context.Context) (*RewriteSnapshot, error) {
	return s.snapshot, s.err
}

func parseTarget(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func lookup(answers map[string][]netip.Addr) LookupNetIP {
	return func(_ context.Context, host string) ([]netip.Addr, error) {
		ips, ok := answers[host]
		if !ok {
			return nil, errors.New("not found")
		}
		return ips, nil
	}
}

func registeredSource() RewriteConfigSource {
	return staticSource{snapshot: &RewriteSnapshot{Upstreams: map[string]RewriteUpstream{
		"cdn.example.com": {
			Profile:    upstream_entity.PackageProfileNPM,
			Transports: upstream_entity.ProtocolSet{upstream_entity.ProtocolStatic},
		},
	}}}
}

func TestResolvePinsRegisteredDestination(t *testing.T) {
	resolver := New(Options{
		Source: registeredSource(),
		LookupNetIP: lookup(map[string][]netip.Addr{
			"cdn.example.com": {netip.MustParseAddr("203.0.113.8")},
		}),
	})
	targetURL := parseTarget(t, "https://cdn.example.com:8443/pkg.tgz?x=1")

	got, err := resolver.Resolve(context.Background(), targetURL, DestinationRequirement{
		RequireRegistered: true,
		Transport:         upstream_entity.ProtocolStatic,
		Profile:           upstream_entity.PackageProfileNPM,
		AddressPolicy:     PublicAddressesOnly,
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.URL.String() != targetURL.String() {
		t.Fatalf("URL = %q, want %q", got.URL, targetURL)
	}
	if got.Authority != "cdn.example.com:8443" || got.Host != "cdn.example.com:8443" {
		t.Fatalf("authority/Host = %q/%q", got.Authority, got.Host)
	}
	if got.ServerName != "cdn.example.com" {
		t.Fatalf("ServerName = %q", got.ServerName)
	}
	if got.DialAddress != "203.0.113.8:8443" {
		t.Fatalf("DialAddress = %q", got.DialAddress)
	}
}

func TestResolveRejectsUnsafeDestinationShapesUniformly(t *testing.T) {
	resolver := New(Options{
		Source: registeredSource(),
		LookupNetIP: lookup(map[string][]netip.Addr{
			"cdn.example.com": {netip.MustParseAddr("203.0.113.8")},
		}),
	})
	requirement := DestinationRequirement{
		RequireRegistered: true,
		Transport:         upstream_entity.ProtocolStatic,
		Profile:           upstream_entity.PackageProfileNPM,
		AddressPolicy:     PublicAddressesOnly,
	}
	cases := map[string]string{
		"userinfo":           "https://user:secret@cdn.example.com/pkg.tgz",
		"unsupported scheme": "file://cdn.example.com/pkg.tgz",
		"missing authority":  "https:///pkg.tgz",
		"unregistered host":  "https://absent.example.com/pkg.tgz",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := resolver.Resolve(context.Background(), parseTarget(t, raw), requirement)
			if !errors.Is(err, ErrDestinationNotAllowed) {
				t.Fatalf("error = %v, want ErrDestinationNotAllowed", err)
			}
			if err.Error() != ErrDestinationNotAllowed.Error() {
				t.Fatalf("error text = %q, want uniform %q", err, ErrDestinationNotAllowed)
			}
		})
	}

	for name, requirement := range map[string]DestinationRequirement{
		"wrong transport": {
			RequireRegistered: true,
			Transport:         upstream_entity.ProtocolRegistry,
			Profile:           upstream_entity.PackageProfileNPM,
		},
		"wrong profile": {
			RequireRegistered: true,
			Transport:         upstream_entity.ProtocolStatic,
			Profile:           upstream_entity.PackageProfilePyPI,
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := resolver.Resolve(context.Background(), parseTarget(t, "https://cdn.example.com/x"), requirement)
			if !errors.Is(err, ErrDestinationNotAllowed) || err.Error() != ErrDestinationNotAllowed.Error() {
				t.Fatalf("error = %v, want uniform ErrDestinationNotAllowed", err)
			}
		})
	}
}

func TestResolvePublicPolicyRejectsEveryUnsafeDNSAnswer(t *testing.T) {
	unsafe := map[string]netip.Addr{
		"loopback v4":     netip.MustParseAddr("127.0.0.1"),
		"loopback v6":     netip.MustParseAddr("::1"),
		"private v4":      netip.MustParseAddr("10.0.0.1"),
		"private v6":      netip.MustParseAddr("fd00::1"),
		"link-local v4":   netip.MustParseAddr("169.254.1.2"),
		"link-local v6":   netip.MustParseAddr("fe80::1"),
		"multicast v4":    netip.MustParseAddr("224.0.0.1"),
		"multicast v6":    netip.MustParseAddr("ff02::1"),
		"mapped loopback": netip.MustParseAddr("::ffff:127.0.0.1"),
	}
	for name, addr := range unsafe {
		t.Run(name, func(t *testing.T) {
			resolver := New(Options{LookupNetIP: lookup(map[string][]netip.Addr{"packages.example.com": {addr}})})
			_, err := resolver.Resolve(context.Background(), parseTarget(t, "https://packages.example.com/x"), DestinationRequirement{
				AddressPolicy: PublicAddressesOnly,
			})
			if !errors.Is(err, ErrDestinationNotAllowed) {
				t.Fatalf("error = %v, want ErrDestinationNotAllowed", err)
			}
		})
	}

	t.Run("mixed public and private answers fail closed", func(t *testing.T) {
		resolver := New(Options{LookupNetIP: lookup(map[string][]netip.Addr{
			"packages.example.com": {
				netip.MustParseAddr("203.0.113.8"),
				netip.MustParseAddr("10.0.0.8"),
			},
		})})
		_, err := resolver.Resolve(context.Background(), parseTarget(t, "https://packages.example.com/x"), DestinationRequirement{
			AddressPolicy: PublicAddressesOnly,
		})
		if !errors.Is(err, ErrDestinationNotAllowed) {
			t.Fatalf("error = %v, want ErrDestinationNotAllowed", err)
		}
	})
}

func TestResolveZeroRequirementRejectsPrivateAddress(t *testing.T) {
	resolver := New(Options{LookupNetIP: lookup(map[string][]netip.Addr{
		"internal.example": {netip.MustParseAddr("10.0.0.7")},
	})})
	_, err := resolver.Resolve(context.Background(), parseTarget(t, "https://internal.example/x"), DestinationRequirement{})
	if !errors.Is(err, ErrDestinationNotAllowed) {
		t.Fatalf("zero-value requirement error = %v, want ErrDestinationNotAllowed", err)
	}
}

func TestResolveExplicitGenericOriginAllowsPrivateAddress(t *testing.T) {
	resolver := New(Options{LookupNetIP: lookup(map[string][]netip.Addr{
		"apt.corp.example": {netip.MustParseAddr("10.20.30.40")},
	})})

	got, err := resolver.Resolve(context.Background(), parseTarget(t, "https://apt.corp.example/debian"), DestinationRequirement{
		AddressPolicy: AllowPrivateAddresses,
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.DialAddress != "10.20.30.40:443" {
		t.Fatalf("DialAddress = %q", got.DialAddress)
	}
}
