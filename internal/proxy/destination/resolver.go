// Package destination validates and pins outbound HTTP destinations.
package destination

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/cago-frame/cago/pkg/logger"
	"go.uber.org/zap"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
)

// ErrDestinationNotAllowed deliberately hides the failed safety check.
var ErrDestinationNotAllowed = errors.New("目标不可用")

// AddressPolicy controls whether resolved private addresses are permitted.
type AddressPolicy uint8

const (
	// PublicAddressesOnly is the secure default for package and dynamically discovered destinations.
	PublicAddressesOnly AddressPolicy = iota
	// AllowPrivateAddresses is explicit opt-in for an administrator-configured generic origin.
	AllowPrivateAddresses
)

// DestinationRequirement describes the allowlist and address policy for one destination.
type DestinationRequirement struct {
	RequireRegistered bool
	Transport         string
	Profile           upstream_entity.PackageProfile
	AddressPolicy     AddressPolicy
}

// RewriteUpstream is the destination-relevant part of one enabled upstream.
type RewriteUpstream struct {
	Profile    upstream_entity.PackageProfile
	Transports upstream_entity.ProtocolSet
}

// RewriteSnapshot is one coherent enabled-upstream configuration.
type RewriteSnapshot struct {
	Upstreams map[string]RewriteUpstream
}

// RewriteConfigSource supplies coherent enabled-upstream configuration.
type RewriteConfigSource interface {
	Snapshot(context.Context) (*RewriteSnapshot, error)
}

// ResolvedTarget keeps the HTTP authority and TLS name coupled to the validated dial address.
type ResolvedTarget struct {
	URL         *url.URL
	Authority   string
	Host        string
	ServerName  string
	DialAddress string
}

// DestinationResolver validates and pins an outbound destination.
type DestinationResolver interface {
	Resolve(context.Context, *url.URL, DestinationRequirement) (*ResolvedTarget, error)
}

// LookupNetIP is injectable so DNS rebinding behavior can be tested deterministically.
type LookupNetIP func(context.Context, string) ([]netip.Addr, error)

// Options configures a Resolver.
type Options struct {
	Source      RewriteConfigSource
	LookupNetIP LookupNetIP
}

type resolver struct {
	source RewriteConfigSource
	lookup LookupNetIP
}

// New constructs a destination resolver.
func New(opt Options) DestinationResolver {
	if opt.LookupNetIP == nil {
		opt.LookupNetIP = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	return &resolver{source: opt.Source, lookup: opt.LookupNetIP}
}

// Resolve validates target and returns a URL/Host/SNI/dial tuple from the same DNS decision.
func (r *resolver) Resolve(
	ctx context.Context, target *url.URL, requirement DestinationRequirement,
) (*ResolvedTarget, error) {
	if target == nil || target.User != nil ||
		(target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		return nil, deny(ctx, target, "invalid URL", nil)
	}
	host := strings.ToLower(strings.TrimSuffix(target.Hostname(), "."))
	if host == "" {
		return nil, deny(ctx, target, "missing hostname", nil)
	}
	port, err := targetPort(target)
	if err != nil {
		return nil, deny(ctx, target, "invalid port", err)
	}
	if requirement.RequireRegistered {
		if err := r.checkRegistered(ctx, host, requirement); err != nil {
			return nil, deny(ctx, target, "registration mismatch", err)
		}
	}

	addresses, err := r.resolveAddresses(ctx, host)
	if err != nil || len(addresses) == 0 {
		return nil, deny(ctx, target, "DNS resolution failed", err)
	}
	for _, address := range addresses {
		if requirement.AddressPolicy == PublicAddressesOnly && unsafeAddress(address) {
			return nil, deny(ctx, target, "address policy rejected destination", nil)
		}
	}
	address := addresses[0].Unmap()
	cloned := *target
	return &ResolvedTarget{
		URL:         &cloned,
		Authority:   target.Host,
		Host:        target.Host,
		ServerName:  host,
		DialAddress: net.JoinHostPort(address.String(), port),
	}, nil
}

func (r *resolver) checkRegistered(
	ctx context.Context, host string, requirement DestinationRequirement,
) error {
	if r.source == nil {
		return ErrDestinationNotAllowed
	}
	snapshot, err := r.source.Snapshot(ctx)
	if err != nil || snapshot == nil {
		return ErrDestinationNotAllowed
	}
	upstream, ok := snapshot.Upstreams[host]
	if !ok {
		return ErrDestinationNotAllowed
	}
	if requirement.Transport != "" && !upstream.Transports.Has(requirement.Transport) {
		return ErrDestinationNotAllowed
	}
	if requirement.Profile != "" &&
		upstream_entity.NormalizePackageProfile(upstream.Profile) != requirement.Profile {
		return ErrDestinationNotAllowed
	}
	return nil
}

func (r *resolver) resolveAddresses(ctx context.Context, host string) ([]netip.Addr, error) {
	if address, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{address}, nil
	}
	return r.lookup(ctx, host)
}

func targetPort(target *url.URL) (string, error) {
	port := target.Port()
	if port == "" {
		if target.Scheme == "https" {
			return "443", nil
		}
		return "80", nil
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", ErrDestinationNotAllowed
	}
	return port, nil
}

func unsafeAddress(address netip.Addr) bool {
	address = address.Unmap()
	return !address.IsValid() || address.IsUnspecified() || address.IsLoopback() ||
		address.IsPrivate() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() ||
		address.IsMulticast()
}

func deny(ctx context.Context, target *url.URL, reason string, err error) error {
	host := ""
	if target != nil {
		host = target.Hostname()
	}
	fields := []zap.Field{zap.String("host", host), zap.String("reason", reason)}
	if err != nil {
		fields = append(fields, zap.Error(err))
	}
	logger.Ctx(ctx).Warn("拒绝回源目标", fields...)
	return ErrDestinationNotAllowed
}
