package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
)

func TestPyPIProfileRegistersCompanionAndClassifies(t *testing.T) {
	profile, ok := packageprofile.Lookup(upstream_entity.PackageProfilePyPI)
	if !ok {
		t.Fatal("pypi profile is not registered")
	}
	if got := profile.Describe(); got.Profile != upstream_entity.PackageProfilePyPI || got.Name != "pypi" {
		t.Fatalf("description = %+v", got)
	}

	for _, path := range []string{"/simple/", "/simple/demo-pkg/"} {
		got := profile.Classify(packageprofile.Request{Path: path, Header: http.Header{"Accept": {"application/vnd.pypi.simple.v1+json"}}})
		if got.Class != packageprofile.ClassMutable || !got.Transform || !containsPyPIString(got.Variants, "Accept") || !containsPyPIString(got.MediaTypes, "application/vnd.pypi.simple.v1+json") || !containsPyPIString(got.MediaTypes, "text/html") {
			t.Errorf("Classify(%q) = %+v", path, got)
		}
	}

	immutable := []string{
		"/packages/ab/cd/0123456789abcdef0123456789abcdef0123456789abcdef0123456789ab/demo_pkg-1.0-py3-none-any.whl",
		"/packages/ab/cd/0123456789abcdef0123456789abcdef0123456789abcdef0123456789ab/demo_pkg-1.0-py3-none-any.whl.metadata",
		"/packages/fe/dc/fedcba9876543210fedcba9876543210fedcba9876543210fedcba987654/demo_pkg-1.0.tar.gz",
	}
	for _, path := range immutable {
		if got := profile.Classify(packageprofile.Request{Path: path}); got.Class != packageprofile.ClassImmutable || got.Transform {
			t.Errorf("Classify(%q) = %+v", path, got)
		}
	}
	unknown := []string{
		"/project/demo-pkg",
		"/packages/demo_pkg-1.0.whl",
		"/simple/demo-pkg/files",
		"/packages/ab/cd/0123456789abcdef0123456789abcdef0123456789abcdef012345678/demo.whl",
		"/packages/ab/cd/0123456789abcdef0123456789abcdef0123456789abcdef0123456789abc/demo.whl",
		"/packages/ab/cd/0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef/demo.whl",
		"/packages/ab/cd/0123456789abcdef0123456789abcdef0123456789abcdef0123456789ag/demo.whl",
		"/packages/ab/cd/0123456789abcdef0123456789abcdef0123456789abcdef0123456789ab/demo.exe",
	}
	for _, path := range unknown {
		if got := profile.Classify(packageprofile.Request{Path: path}); got.Recognized() {
			t.Errorf("Classify(%q) = %+v, want unknown", path, got)
		}
	}

	companions := profile.Companions()
	if len(companions) != 1 || companions[0].Host != "files.pythonhosted.org" || companions[0].Profile != upstream_entity.PackageProfilePyPI || companions[0].Transport != upstream_entity.ProtocolStatic {
		t.Fatalf("companions = %+v", companions)
	}
	if guidance := profile.Guidance(); guidance.Client != "pip" || len(guidance.Configuration) == 0 {
		t.Fatalf("guidance = %+v", guidance)
	}
}

func TestPyPITransformAcceptsWarehouseArtifactPath(t *testing.T) {
	profile := mustPyPIProfile(t)
	body := []byte(`{"meta":{"api-version":"1.1"},"name":"idna","files":[{"filename":"idna-0.2.tar.gz","url":"https://files.pythonhosted.org/packages/22/35/04dedec60e9366ba19ac7c147cd715c88a7e87d43cda47a75802190c0950/idna-0.2.tar.gz"}]}`)

	result, err := profile.Transform(context.Background(), packageprofile.TransformRequest{
		Body: body, ContentType: "application/vnd.pypi.simple.v1+json",
		Source: mustPyPIURL(t, "https://pypi.org/simple/idna/"), SiteBaseURL: "https://katch.example.com",
		RewriteURL: successfulPyPIRewrite,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Body), "https://katch.example.com/files.pythonhosted.org/packages/22/35/04dedec60e9366ba19ac7c147cd715c88a7e87d43cda47a75802190c0950/idna-0.2.tar.gz") {
		t.Fatalf("body = %s", result.Body)
	}
}

func TestPyPITransformPEP503PreservesLinkMetadata(t *testing.T) {
	profile := mustPyPIProfile(t)
	body, err := os.ReadFile("testdata/pypi/simple.html")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	result, err := profile.Transform(context.Background(), packageprofile.TransformRequest{
		Body: body, ContentType: "text/html; charset=UTF-8",
		Source: mustPyPIURL(t, "https://pypi.org/simple/demo-pkg/"), SiteBaseURL: "https://katch.example.com",
		RewriteURL: pyPIRewriteRecorder(t, &calls),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ContentType != "text/html; charset=UTF-8" || calls != 2 {
		t.Fatalf("content type = %q, rewrite calls = %d", result.ContentType, calls)
	}

	doc, err := html.Parse(strings.NewReader(string(result.Body)))
	if err != nil {
		t.Fatal(err)
	}
	links := pyPIHTMLLinks(doc)
	if len(links) != 2 {
		t.Fatalf("links = %+v", links)
	}
	wheel := links[0]
	if wheel["href"] != "https://katch.example.com/files.pythonhosted.org/packages/ab/cd/0123456789abcdef0123456789abcdef0123456789abcdef0123456789ab/demo_pkg-1.0-py3-none-any.whl?download=1#sha256=wheelhash" {
		t.Errorf("wheel href = %q", wheel["href"])
	}
	if wheel["data-requires-python"] != ">=3.8" || wheel["data-yanked"] != "broken build" || wheel["data-dist-info-metadata"] != "sha256=metadatahash" {
		t.Errorf("wheel metadata = %+v", wheel)
	}
	if links[1]["data-core-metadata"] != "sha256=corehash" || !strings.HasSuffix(links[1]["href"], "demo_pkg-1.0.tar.gz#sha256=sdisthash") {
		t.Errorf("sdist link = %+v", links[1])
	}
}

func TestPyPITransformPEP691PreservesFileMetadata(t *testing.T) {
	profile := mustPyPIProfile(t)
	body, err := os.ReadFile("testdata/pypi/simple.json")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	result, err := profile.Transform(context.Background(), packageprofile.TransformRequest{
		Body: body, ContentType: "application/vnd.pypi.simple.v1+json",
		Source: mustPyPIURL(t, "https://pypi.org/simple/demo-pkg/"), SiteBaseURL: "https://katch.example.com",
		RewriteURL: pyPIRewriteRecorder(t, &calls),
	})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Meta  map[string]any `json:"meta"`
		Name  string         `json:"name"`
		Files []struct {
			Filename         string            `json:"filename"`
			URL              string            `json:"url"`
			Hashes           map[string]string `json:"hashes"`
			RequiresPython   *string           `json:"requires-python"`
			Yanked           any               `json:"yanked"`
			DistInfoMetadata map[string]string `json:"dist-info-metadata"`
			CoreMetadata     map[string]string `json:"core-metadata"`
		} `json:"files"`
	}
	if err := json.Unmarshal(result.Body, &got); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || got.Name != "demo-pkg" || got.Meta["api-version"] != "1.1" || len(got.Files) != 2 {
		t.Fatalf("document changed: calls=%d document=%+v", calls, got)
	}
	wheel := got.Files[0]
	if !strings.HasSuffix(wheel.URL, ".whl#sha256=wheelhash") || !strings.HasPrefix(wheel.URL, "https://katch.example.com/files.pythonhosted.org/") || wheel.Hashes["sha256"] != "wheelhash" || wheel.RequiresPython == nil || *wheel.RequiresPython != ">=3.8" || wheel.Yanked != "broken build" || wheel.DistInfoMetadata["sha256"] != "metadatahash" {
		t.Errorf("wheel = %+v", wheel)
	}
	if got.Files[1].Yanked != false || got.Files[1].CoreMetadata["sha256"] != "corehash" {
		t.Errorf("sdist = %+v", got.Files[1])
	}
}

func TestPyPITransformPEP691RootPreservesProjects(t *testing.T) {
	profile := mustPyPIProfile(t)
	body := []byte(`{"meta":{"api-version":"1.1","_last-serial":42},"projects":[{"name":"demo-pkg","_last-serial":21}]}`)
	calls := 0
	result, err := profile.Transform(context.Background(), packageprofile.TransformRequest{
		Body: body, ContentType: "application/vnd.pypi.simple.v1+json",
		Source: mustPyPIURL(t, "https://pypi.org/simple/"), SiteBaseURL: "https://katch.example.com",
		RewriteURL: pyPIRewriteRecorder(t, &calls),
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 || string(result.Body) != string(body) {
		t.Fatalf("rewrite calls = %d, body = %s", calls, result.Body)
	}
}

func TestPyPITransformFailsClosed(t *testing.T) {
	profile := mustPyPIProfile(t)
	validJSON := []byte(`{"meta":{"api-version":"1.1"},"name":"demo","files":[{"filename":"demo.whl","url":"https://files.pythonhosted.org/packages/ab/cd/0123456789abcdef0123456789abcdef0123456789abcdef0123456789ab/demo.whl"}]}`)
	tests := []struct {
		name string
		in   packageprofile.TransformRequest
		want error
	}{
		{name: "missing site", in: packageprofile.TransformRequest{Body: validJSON, ContentType: "application/vnd.pypi.simple.v1+json", RewriteURL: successfulPyPIRewrite}, want: packageprofile.ErrUnavailable},
		{name: "missing rewriter", in: packageprofile.TransformRequest{Body: validJSON, ContentType: "application/vnd.pypi.simple.v1+json", SiteBaseURL: "https://katch.example.com"}, want: packageprofile.ErrUnavailable},
		{name: "missing companion", in: packageprofile.TransformRequest{Body: validJSON, ContentType: "application/vnd.pypi.simple.v1+json", SiteBaseURL: "https://katch.example.com", RewriteURL: unavailablePyPIRewrite}, want: packageprofile.ErrUnavailable},
		{name: "malformed json", in: packageprofile.TransformRequest{Body: []byte(`{`), ContentType: "application/vnd.pypi.simple.v1+json", SiteBaseURL: "https://katch.example.com", RewriteURL: successfulPyPIRewrite}, want: packageprofile.ErrInvalidMetadata},
		{name: "wrong companion host", in: packageprofile.TransformRequest{Body: []byte(`{"files":[{"filename":"demo.whl","url":"https://evil.example/packages/ab/cd/0123456789abcdef0123456789abcdef0123456789abcdef0123456789ab/demo.whl"}]}`), ContentType: "application/vnd.pypi.simple.v1+json", SiteBaseURL: "https://katch.example.com", RewriteURL: successfulPyPIRewrite}, want: packageprofile.ErrInvalidMetadata},
		{name: "wrong companion scheme", in: packageprofile.TransformRequest{Body: []byte(`{"files":[{"filename":"demo.whl","url":"ftp://files.pythonhosted.org/packages/ab/cd/0123456789abcdef0123456789abcdef0123456789abcdef0123456789ab/demo.whl"}]}`), ContentType: "application/vnd.pypi.simple.v1+json", SiteBaseURL: "https://katch.example.com", RewriteURL: successfulPyPIRewrite}, want: packageprofile.ErrInvalidMetadata},
		{name: "wrong HTML companion host", in: packageprofile.TransformRequest{Body: []byte(`<a href="https://evil.example/packages/ab/cd/0123456789abcdef0123456789abcdef0123456789abcdef0123456789ab/demo.whl">demo</a>`), ContentType: "text/html", SiteBaseURL: "https://katch.example.com", RewriteURL: successfulPyPIRewrite}, want: packageprofile.ErrInvalidMetadata},
		{name: "unsupported media type", in: packageprofile.TransformRequest{Body: validJSON, ContentType: "application/xml", SiteBaseURL: "https://katch.example.com", RewriteURL: successfulPyPIRewrite}, want: packageprofile.ErrInvalidMetadata},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := profile.Transform(context.Background(), tc.in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func mustPyPIProfile(t *testing.T) packageprofile.Profile {
	t.Helper()
	profile, ok := packageprofile.Lookup(upstream_entity.PackageProfilePyPI)
	if !ok {
		t.Fatal("pypi profile is not registered")
	}
	return profile
}

func pyPIRewriteRecorder(t *testing.T, calls *int) packageprofile.RewriteURL {
	t.Helper()
	return func(_ context.Context, source *url.URL, companion packageprofile.Companion) (*url.URL, error) {
		*calls++
		if companion.Host != "files.pythonhosted.org" || companion.Profile != upstream_entity.PackageProfilePyPI || companion.Transport != upstream_entity.ProtocolStatic {
			t.Fatalf("companion = %+v", companion)
		}
		if source.Fragment != "" {
			t.Fatalf("fragment was sent to resolver: %q", source.Fragment)
		}
		return url.Parse("https://katch.example.com/files.pythonhosted.org" + source.EscapedPath() + pyPIQuerySuffix(source))
	}
}

func mustPyPIURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func pyPIQuerySuffix(source *url.URL) string {
	if source.RawQuery == "" {
		return ""
	}
	return "?" + source.RawQuery
}

func successfulPyPIRewrite(_ context.Context, source *url.URL, _ packageprofile.Companion) (*url.URL, error) {
	return url.Parse("https://katch.example.com/files.pythonhosted.org" + source.EscapedPath())
}

func unavailablePyPIRewrite(context.Context, *url.URL, packageprofile.Companion) (*url.URL, error) {
	return nil, packageprofile.ErrUnavailable
}

func pyPIHTMLLinks(node *html.Node) []map[string]string {
	var links []map[string]string
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if current.Type == html.ElementNode && current.Data == "a" {
			attrs := make(map[string]string, len(current.Attr))
			for _, attr := range current.Attr {
				attrs[attr.Key] = attr.Val
			}
			links = append(links, attrs)
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return links
}

func containsPyPIString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
