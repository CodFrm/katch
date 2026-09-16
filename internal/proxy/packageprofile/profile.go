// Package packageprofile defines the extension contract for package metadata adapters.
package packageprofile

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
)

var (
	// ErrInvalidMetadata marks malformed, oversized or semantically incomplete metadata.
	ErrInvalidMetadata = errors.New("package metadata is invalid")
	// ErrUnavailable marks a local rewrite prerequisite that is not currently configured.
	ErrUnavailable = errors.New("package metadata rewrite is unavailable")
)

// Class is the protocol-owned retention class of a request path.
type Class uint8

const (
	ClassUnknown Class = iota
	ClassMutable
	ClassImmutable
)

// Request is the stable request surface exposed to profile classifiers.
type Request struct {
	Host     string
	Path     string
	RawQuery string
	Header   http.Header
}

// Representation describes one recognized protocol representation.
type Representation struct {
	Class      Class
	Transform  bool
	Variants   []string
	MediaTypes []string
}

// Recognized reports whether the profile owns the path's cache semantics.
func (r Representation) Recognized() bool { return r.Class != ClassUnknown }

// Description is registry-facing profile metadata.
type Description struct {
	Profile  upstream_entity.PackageProfile
	Name     string
	Variants []string
}

// Companion describes a host relationship required by a profile.
type Companion struct {
	Host      string
	Profile   upstream_entity.PackageProfile
	Transport string
}

// Guidance is structured client configuration guidance for presentation layers.
type Guidance struct {
	Client        string
	Configuration []string
}

// RewriteURL validates an upstream URL and maps it to the public katch URL space.
type RewriteURL func(context.Context, *url.URL, Companion) (*url.URL, error)

// TransformRequest contains one bounded, identity-encoded metadata representation.
type TransformRequest struct {
	Body        []byte
	ContentType string
	Source      *url.URL
	SiteBaseURL string
	RewriteURL  RewriteURL
}

// TransformResult is the canonical transformed representation.
type TransformResult struct {
	Body        []byte
	ContentType string
}

// Profile is implemented independently by every built-in package adapter.
type Profile interface {
	Describe() Description
	Classify(Request) Representation
	Transform(context.Context, TransformRequest) (*TransformResult, error)
	Companions() []Companion
	Guidance() Guidance
}
