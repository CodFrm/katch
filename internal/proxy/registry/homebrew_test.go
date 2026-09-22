package registry

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/CodFrm/katch/internal/proxy/origin"
)

type homebrewBottleOrigin struct {
	request *origin.Request
}

func (o *homebrewBottleOrigin) Do(_ context.Context, request *origin.Request) (*origin.Response, error) {
	o.request = request
	return &origin.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":          {"application/vnd.oci.image.manifest.v1+json"},
			"Docker-Content-Digest": {"sha256:b07e1e"},
		},
		Body: io.NopCloser(strings.NewReader("bottle-manifest-bytes")),
	}, nil
}

func TestHomebrewBottleUsesGenericGHCRPathAndDigestResponse(t *testing.T) {
	upstream := &homebrewBottleOrigin{}
	adapter := New(Options{Origin: upstream})
	response, err := adapter.Do(context.Background(), &Request{
		Host:   "ghcr.io",
		Origin: "https://ghcr.io",
		Path:   "/homebrew/core/jq/manifests/sha256:b07e1e",
		Method: http.MethodGet,
		Header: http.Header{"Accept": {"application/vnd.oci.image.manifest.v1+json"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if upstream.request == nil {
		t.Fatal("registry origin was not called")
	}
	if upstream.request.Path != "/v2/homebrew/core/jq/manifests/sha256:b07e1e" {
		t.Fatalf("origin path = %q", upstream.request.Path)
	}
	if upstream.request.Header.Get("Accept") != "application/vnd.oci.image.manifest.v1+json" {
		t.Fatalf("origin Accept = %q", upstream.request.Header.Get("Accept"))
	}
	if response.Header.Get("Docker-Content-Digest") != "sha256:b07e1e" {
		t.Fatalf("Docker-Content-Digest = %q", response.Header.Get("Docker-Content-Digest"))
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if string(body) != "bottle-manifest-bytes" {
		t.Fatalf("body = %q", body)
	}

	for _, path := range []string{
		"/homebrew/core/jq/manifests/4.0.0",
		"/homebrew/core/jq/blobs/sha256:1a9e7f",
	} {
		gotPath, gotScope := upstreamPath(path, false)
		if gotPath != "/v2"+path || gotScope != "repository:homebrew/core/jq:pull" {
			t.Errorf("upstreamPath(%q) = %q, %q", path, gotPath, gotScope)
		}
	}
}
