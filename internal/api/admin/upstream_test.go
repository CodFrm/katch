package admin

import (
	"reflect"
	"strings"
	"testing"

	"github.com/CodFrm/katch/internal/model/entity/upstream_entity"
	"github.com/CodFrm/katch/internal/proxy/packageprofile"

	// 注册表要有人装过内置 profile 才有内容；显式引入，不依赖调用链上谁恰好引过。
	_ "github.com/CodFrm/katch/internal/proxy/packageprofile/builtin"
)

// TestPackageProfileBindingTagsMatchRegistry 三个请求结构里的 oneof 列表是 Go tag
// 字面量：注册表新增一个 profile 时编译器不会提醒同步，漂移发生后管理接口会用一条
// binding 报错把新 profile 挡在门外——界面上选得到、接口上存不进。这一条把 tag 与
// 注册表钉在同一个集合上。
func TestPackageProfileBindingTagsMatchRegistry(t *testing.T) {
	want := map[upstream_entity.PackageProfile]bool{upstream_entity.PackageProfileNone: true}
	for _, description := range packageprofile.Descriptions() {
		want[description.Profile] = true
	}

	requests := []any{
		ListUpstreamsRequest{}, SaveUpstreamRequest{}, UpdateUpstreamRequest{},
	}
	checked := 0
	for _, request := range requests {
		structType := reflect.TypeOf(request)
		field, ok := structType.FieldByName("PackageProfile")
		if !ok {
			field, ok = structType.FieldByName("PreviewPackageProfile")
		}
		if !ok {
			t.Fatalf("%s 上找不到 PackageProfile/PreviewPackageProfile 字段", structType.Name())
		}
		binding := field.Tag.Get("binding")
		value, ok := strings.CutPrefix(binding, "omitempty,oneof=")
		if !ok {
			t.Fatalf("%s.%s 的 binding tag 不是 omitempty,oneof=<列表>：%q",
				structType.Name(), field.Name, binding)
		}
		got := map[upstream_entity.PackageProfile]bool{}
		for _, name := range strings.Fields(value) {
			got[upstream_entity.PackageProfile(name)] = true
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s.%s 的 oneof 集合与注册表不一致：tag=%v，注册表=%v",
				structType.Name(), field.Name, got, want)
		}
		checked++
	}
	if checked != len(requests) {
		t.Fatalf("只检查了 %d 个结构，要的是 %d", checked, len(requests))
	}
}
