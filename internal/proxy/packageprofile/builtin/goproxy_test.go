package builtin

import (
	"context"
	"testing"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"
)

func TestGoProxyProfileRegistersAndClassifiesRepresentations(t *testing.T) {
	profile, ok := packageprofile.Lookup(upstream_entity.PackageProfileGoProxy)
	if !ok {
		t.Fatal("goproxy profile is not registered")
	}
	if got := profile.Describe(); got.Profile != upstream_entity.PackageProfileGoProxy || got.Name != "goproxy" {
		t.Fatalf("description = %+v", got)
	}

	tests := []struct {
		name  string
		host  string
		path  string
		class packageprofile.Class
	}{
		{name: "escaped uppercase info", host: "proxy.golang.org", path: "/github.com/!burnt!sushi/toml/@v/v1.4.0.info", class: packageprofile.ClassImmutable},
		{name: "version mod", host: "proxy.golang.org", path: "/example.com/mod/@v/v1.2.3.mod", class: packageprofile.ClassImmutable},
		{name: "version zip", host: "proxy.golang.org", path: "/example.com/mod/@v/v1.2.3.zip", class: packageprofile.ClassImmutable},
		{name: "version list", host: "proxy.golang.org", path: "/example.com/mod/@v/list", class: packageprofile.ClassMutable},
		{name: "module latest", host: "proxy.golang.org", path: "/example.com/mod/@latest", class: packageprofile.ClassMutable},
		{name: "bare version list", host: "proxy.golang.org", path: "/@v/list", class: packageprofile.ClassUnknown},
		{name: "bare latest", host: "proxy.golang.org", path: "/@latest", class: packageprofile.ClassUnknown},
		{name: "sumdb lookup", host: "sum.golang.org", path: "/lookup/example.com/mod@v1.2.3", class: packageprofile.ClassImmutable},
		{name: "complete tile", host: "sum.golang.org", path: "/tile/8/1/000", class: packageprofile.ClassImmutable},
		{name: "partial tile", host: "sum.golang.org", path: "/tile/8/1/000.p/16", class: packageprofile.ClassMutable},
		{name: "sumdb latest", host: "sum.golang.org", path: "/latest", class: packageprofile.ClassMutable},
		{name: "bare lookup", host: "sum.golang.org", path: "/lookup", class: packageprofile.ClassUnknown},
		{name: "partial tile without width", host: "sum.golang.org", path: "/tile/8/1/000.p/", class: packageprofile.ClassUnknown},
		// tlog 把 ≥1000 的 tile 索引按三位一组编码，除最后一组外都带 x 前缀。真实的
		// sum.golang.org 树很大，Go 客户端取的 level-0 tile 几乎全是这种形式。
		{name: "complete tile with grouped index", host: "sum.golang.org", path: "/tile/8/0/x251/154", class: packageprofile.ClassImmutable},
		{name: "partial tile with grouped index", host: "sum.golang.org", path: "/tile/8/0/x251/174.p/91", class: packageprofile.ClassMutable},
		{name: "complete data tile", host: "sum.golang.org", path: "/tile/8/data/x001/x234/567", class: packageprofile.ClassImmutable},
		{name: "partial data tile", host: "sum.golang.org", path: "/tile/8/data/x001/x234/567.p/3", class: packageprofile.ClassMutable},
		// 只有规范路径才由树上的位置唯一确定内容；Go 客户端也只发规范路径。
		{name: "grouped index without x prefix", host: "sum.golang.org", path: "/tile/8/0/251/154", class: packageprofile.ClassUnknown},
		{name: "index not zero padded", host: "sum.golang.org", path: "/tile/8/1/1", class: packageprofile.ClassUnknown},
		{name: "unknown module path", host: "proxy.golang.org", path: "/example.com/mod/@v/v1.2.3.txt", class: packageprofile.ClassUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := profile.Classify(packageprofile.Request{Host: tc.host, Path: tc.path})
			if got.Class != tc.class || got.Transform {
				t.Fatalf("Classify(%q, %q) = %+v, want class %v without transform", tc.host, tc.path, got, tc.class)
			}
		})
	}
}

func TestGoProxyProfileLeavesBodiesByteIdentical(t *testing.T) {
	profile, ok := packageprofile.Lookup(upstream_entity.PackageProfileGoProxy)
	if !ok {
		t.Fatal("goproxy profile is not registered")
	}
	body := []byte("signed or checksummed bytes\x00\xff")
	got, err := profile.Transform(context.Background(), packageprofile.TransformRequest{
		Body: body, ContentType: "application/octet-stream",
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Body) != string(body) || got.ContentType != "application/octet-stream" {
		t.Fatalf("Transform() = body %q, content type %q", got.Body, got.ContentType)
	}
}
