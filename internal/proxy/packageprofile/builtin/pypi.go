package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/url"
	"strings"

	"golang.org/x/net/html"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
)

const pyPIFilesHost = "files.pythonhosted.org"

var pyPIFilesCompanion = packageprofile.Companion{
	Host:      pyPIFilesHost,
	Profile:   upstream_entity.PackageProfilePyPI,
	Transport: upstream_entity.ProtocolStatic,
}

type pyPIProfile struct{}

func init() {
	packageprofile.MustRegister(pyPIProfile{})
}

func (pyPIProfile) Describe() packageprofile.Description {
	return packageprofile.Description{
		Profile:  upstream_entity.PackageProfilePyPI,
		Name:     "pypi",
		Variants: []string{"Accept"},
	}
}

func (pyPIProfile) Classify(request packageprofile.Request) packageprofile.Representation {
	path := strings.TrimSpace(request.Path)
	if pyPISimplePath(path) {
		return packageprofile.Representation{
			Class:     packageprofile.ClassMutable,
			Transform: true,
			Variants:  []string{"Accept"},
			MediaTypes: []string{
				"text/html",
				"application/vnd.pypi.simple.v1+html",
				"application/json",
				"application/vnd.pypi.simple.v1+json",
			},
		}
	}
	if pyPIArtifactPath(path) {
		return packageprofile.Representation{Class: packageprofile.ClassImmutable}
	}
	return packageprofile.Representation{}
}

func (pyPIProfile) Transform(ctx context.Context, request packageprofile.TransformRequest) (*packageprofile.TransformResult, error) {
	if strings.TrimSpace(request.SiteBaseURL) == "" || request.RewriteURL == nil {
		return nil, packageprofile.ErrUnavailable
	}
	if _, err := absoluteHTTPURL(request.SiteBaseURL); err != nil {
		return nil, packageprofile.ErrUnavailable
	}

	mediaType, _, err := mime.ParseMediaType(request.ContentType)
	if err != nil {
		return nil, packageprofile.ErrInvalidMetadata
	}
	var body []byte
	switch strings.ToLower(mediaType) {
	case "text/html", "application/vnd.pypi.simple.v1+html":
		body, err = rewritePyPIHTML(ctx, request)
	case "application/json", "application/vnd.pypi.simple.v1+json":
		body, err = rewritePyPIJSON(ctx, request)
	default:
		return nil, packageprofile.ErrInvalidMetadata
	}
	if err != nil {
		return nil, err
	}
	return &packageprofile.TransformResult{Body: body, ContentType: request.ContentType}, nil
}

func (pyPIProfile) Companions() []packageprofile.Companion {
	return []packageprofile.Companion{pyPIFilesCompanion}
}

func (pyPIProfile) Guidance() packageprofile.Guidance {
	return packageprofile.Guidance{
		Client: "pip",
		Configuration: []string{
			"pip: --index-url https://<katch>/pypi.org/simple/",
			"uv: --index-url https://<katch>/pypi.org/simple/",
			"poetry: source add --priority=primary katch https://<katch>/pypi.org/simple/",
		},
	}
}

func rewritePyPIHTML(ctx context.Context, request packageprofile.TransformRequest) ([]byte, error) {
	tokenizer := html.NewTokenizer(bytes.NewReader(request.Body))
	var output bytes.Buffer
	for {
		tokenType := tokenizer.Next()
		switch tokenType {
		case html.ErrorToken:
			if errors.Is(tokenizer.Err(), io.EOF) {
				return output.Bytes(), nil
			}
			return nil, packageprofile.ErrInvalidMetadata
		case html.StartTagToken, html.SelfClosingTagToken:
			token := tokenizer.Token()
			if !strings.EqualFold(token.Data, "a") {
				_, _ = output.Write(tokenizer.Raw())
				continue
			}
			changed, err := rewritePyPIHTMLLink(ctx, request, &token)
			if err != nil {
				return nil, err
			}
			if changed {
				_, _ = output.WriteString(token.String())
			} else {
				_, _ = output.Write(tokenizer.Raw())
			}
		default:
			_, _ = output.Write(tokenizer.Raw())
		}
	}
}

func rewritePyPIHTMLLink(ctx context.Context, request packageprofile.TransformRequest, token *html.Token) (bool, error) {
	for index := range token.Attr {
		attribute := &token.Attr[index]
		if !strings.EqualFold(attribute.Key, "href") {
			continue
		}
		target, err := resolvePyPIURL(request.Source, attribute.Val)
		if err != nil {
			return false, err
		}
		if !pyPIPotentialArtifactPath(target.Path) {
			return false, nil
		}
		rewritten, err := rewritePyPIArtifactURL(ctx, request, target)
		if err != nil {
			return false, err
		}
		attribute.Val = rewritten.String()
		return true, nil
	}
	return false, nil
}

func rewritePyPIJSON(ctx context.Context, request packageprofile.TransformRequest) ([]byte, error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(request.Body, &document); err != nil || document == nil {
		return nil, packageprofile.ErrInvalidMetadata
	}
	filesRaw, hasFiles := document["files"]
	if !hasFiles {
		projectsRaw, hasProjects := document["projects"]
		if !hasProjects {
			return nil, packageprofile.ErrInvalidMetadata
		}
		var projects []map[string]json.RawMessage
		if err := json.Unmarshal(projectsRaw, &projects); err != nil || projects == nil {
			return nil, packageprofile.ErrInvalidMetadata
		}
		return append([]byte(nil), request.Body...), nil
	}
	var files []map[string]json.RawMessage
	if err := json.Unmarshal(filesRaw, &files); err != nil || files == nil {
		return nil, packageprofile.ErrInvalidMetadata
	}
	for _, file := range files {
		var rawURL string
		if err := json.Unmarshal(file["url"], &rawURL); err != nil || strings.TrimSpace(rawURL) == "" {
			return nil, packageprofile.ErrInvalidMetadata
		}
		target, err := resolvePyPIURL(request.Source, rawURL)
		if err != nil {
			return nil, err
		}
		rewritten, err := rewritePyPIArtifactURL(ctx, request, target)
		if err != nil {
			return nil, err
		}
		file["url"], err = json.Marshal(rewritten.String())
		if err != nil {
			return nil, packageprofile.ErrInvalidMetadata
		}
	}
	rewrittenFiles, err := json.Marshal(files)
	if err != nil {
		return nil, packageprofile.ErrInvalidMetadata
	}
	document["files"] = rewrittenFiles
	body, err := json.Marshal(document)
	if err != nil {
		return nil, packageprofile.ErrInvalidMetadata
	}
	return body, nil
}

func rewritePyPIArtifactURL(ctx context.Context, request packageprofile.TransformRequest, target *url.URL) (*url.URL, error) {
	if err := validatePyPIArtifactURL(target); err != nil {
		return nil, err
	}
	fragment := target.Fragment
	resolverTarget := *target
	resolverTarget.Fragment = ""
	resolverTarget.RawFragment = ""
	mapped, err := request.RewriteURL(ctx, &resolverTarget, pyPIFilesCompanion)
	if err != nil {
		if errors.Is(err, packageprofile.ErrUnavailable) {
			return nil, packageprofile.ErrUnavailable
		}
		return nil, packageprofile.ErrInvalidMetadata
	}
	if mapped == nil {
		return nil, packageprofile.ErrUnavailable
	}
	if _, err := absoluteHTTPURL(mapped.String()); err != nil {
		return nil, packageprofile.ErrUnavailable
	}
	result := *mapped
	result.Fragment = fragment
	result.RawFragment = ""
	return &result, nil
}

func resolvePyPIURL(source *url.URL, raw string) (*url.URL, error) {
	target, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, packageprofile.ErrInvalidMetadata
	}
	if !target.IsAbs() {
		if source == nil {
			return nil, packageprofile.ErrInvalidMetadata
		}
		target = source.ResolveReference(target)
	}
	return target, nil
}

func validatePyPIArtifactURL(target *url.URL) error {
	if target == nil || target.User != nil || (target.Scheme != "https" && target.Scheme != "http") {
		return packageprofile.ErrInvalidMetadata
	}
	if strings.ToLower(strings.TrimSuffix(target.Hostname(), ".")) != pyPIFilesHost || !pyPIArtifactPath(target.EscapedPath()) {
		return packageprofile.ErrInvalidMetadata
	}
	return nil
}

func pyPISimplePath(path string) bool {
	parts := strings.Split(strings.Trim(strings.TrimSpace(path), "/"), "/")
	return len(parts) == 1 && parts[0] == "simple" || len(parts) == 2 && parts[0] == "simple" && parts[1] != ""
}

func pyPIArtifactPath(path string) bool {
	parts := strings.Split(strings.Trim(strings.TrimSpace(path), "/"), "/")
	if len(parts) != 5 || parts[0] != "packages" || len(parts[1]) != 2 || len(parts[2]) != 2 || len(parts[3]) != 60 {
		return false
	}
	if !pyPIHex(parts[1]) || !pyPIHex(parts[2]) || !pyPIHex(parts[3]) {
		return false
	}
	return pyPIPotentialArtifactPath(parts[4])
}

func pyPIPotentialArtifactPath(path string) bool {
	lower := strings.ToLower(path)
	lower = strings.TrimSuffix(lower, ".metadata")
	for _, extension := range []string{".whl", ".tar.gz", ".tar.bz2", ".tar.xz", ".zip", ".tgz"} {
		if strings.HasSuffix(lower, extension) {
			return true
		}
	}
	return false
}

func pyPIHex(value string) bool {
	for _, character := range value {
		if character < '0' || character > '9' {
			lower := character | 0x20
			if lower < 'a' || lower > 'f' {
				return false
			}
		}
	}
	return value != ""
}
