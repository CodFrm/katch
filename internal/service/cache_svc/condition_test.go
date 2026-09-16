package cache_svc

import (
	"net/http"
	"testing"

	"github.com/smartystreets/goconvey/convey"
)

// TestParseETag 单个 entity-tag 的语法判定。
//
// 判定必须严格按 RFC 9110 来：读不懂的串不能被当成一个「差不多」的校验符拿去比较，
// 那正是「无效语法按未提供处理」这条规则的反面——一个猜出来的结论会替上游背书。
func TestParseETag(t *testing.T) {
	convey.Convey("单个 entity-tag 的解析与弱标识剥离", t, func() {
		cases := []struct {
			in     string
			opaque string
			ok     bool
		}{
			{`"abc"`, "abc", true},
			{`W/"abc"`, "abc", true},
			{`""`, "", true},
			{`"sha256:2e752c"`, "sha256:2e752c", true},
			{" ", "", false},
			{"abc", "", false},
			{`w/"abc"`, "", false},
			{`"abc`, "", false},
			{`abc"`, "", false},
			{`"a"b"`, "", false},
			{`"abc"extra`, "", false},
			{"\"a\tb\"", "", false},
		}
		for _, c := range cases {
			opaque, ok := parseETag(c.in)
			convey.So(ok, convey.ShouldEqual, c.ok)
			convey.So(opaque, convey.ShouldEqual, c.opaque)
		}
	})
}

// TestParseIfNoneMatch If-None-Match 的列表与星号语义。
//
// 列表里任何一项读不懂，整个头就按未提供处理：只认得出一半就下结论，等于把
// 客户端的意图猜了一半，另一半猜错时客户端会拿到一份它已经有的内容。
func TestParseIfNoneMatch(t *testing.T) {
	convey.Convey("If-None-Match 列表、星号与无效语法", t, func() {
		cases := []struct {
			name     string
			in       string
			tags     []string
			wildcard bool
			ok       bool
		}{
			{"星号", "*", nil, true, true},
			{"单个强 tag", `"a"`, []string{"a"}, false, true},
			{"弱比较的弱 tag", `W/"a"`, []string{"a"}, false, true},
			{"逗号列表", `"a", W/"b"`, []string{"a", "b"}, false, true},
			{"列表带空格", ` "a" ,"b" `, []string{"a", "b"}, false, true},
			{"tag 里含逗号", `"a,b"`, []string{"a,b"}, false, true},
			{"tag 里含逗号且后面还有列表", `"a,b", "c"`, []string{"a,b", "c"}, false, true},
			{"容忍空元素", `"a", , "b"`, []string{"a", "b"}, false, true},
			{"只有逗号", ",", nil, false, false},
			{"缺少引号", "a", nil, false, false},
			{"列表里混进星号", `"a", *`, nil, false, false},
			{"两个 tag 之间没有逗号", `"a" "b"`, nil, false, false},
			{"只由空白组成", "   ", nil, false, false},
		}
		for _, c := range cases {
			convey.Convey(c.name, func() {
				tags, wildcard, ok := parseIfNoneMatch(c.in)
				convey.So(ok, convey.ShouldEqual, c.ok)
				convey.So(wildcard, convey.ShouldEqual, c.wildcard)
				convey.So(tags, convey.ShouldResemble, c.tags)
			})
		}
	})
}

// TestEvaluateConditional 本地条件求值的优先级与三种结果。
//
// 三种结果各自对应一个可观察的行为：无（按缓存的 200 答）、命中（304）、
// 求不了（回源）。把它们压成两个布尔值，就会分不清「不匹配」与 «没有 validator»
// ——前者该由本地答 200，后者必须把判断交回上游。
func TestEvaluateConditional(t *testing.T) {
	const lastModified = "Wed, 21 Oct 2015 07:28:00 GMT"
	header := func(kv ...string) http.Header {
		h := http.Header{}
		for i := 0; i+1 < len(kv); i += 2 {
			h.Set(kv[i], kv[i+1])
		}
		return h
	}
	// multiHeader 按 Add 构造：多行同名字段与一行逗号列表等价。
	multiHeader := func(kv ...string) http.Header {
		h := http.Header{}
		for i := 0; i+1 < len(kv); i += 2 {
			h.Add(kv[i], kv[i+1])
		}
		return h
	}

	convey.Convey("本地条件求值", t, func() {
		cases := []struct {
			name   string
			header http.Header
			etag   string
			mod    string
			want   conditionOutcome
		}{
			{"没有条件头", header(), `"v1"`, lastModified, conditionNone},
			{"ETag 强匹配", header("If-None-Match", `"v1"`), `"v1"`, "", conditionNotModified},
			{"ETag 里含逗号也按一个 tag 认", header("If-None-Match", `"a,b"`), `"a,b"`, "", conditionNotModified},
			{"ETag 弱比较：副本弱、请求强", header("If-None-Match", `"v1"`), `W/"v1"`, "", conditionNotModified},
			{"ETag 弱比较：副本强、请求弱", header("If-None-Match", `W/"v1"`), `"v1"`, "", conditionNotModified},
			{"多行 If-None-Match 等价于逗号列表",
				multiHeader("If-None-Match", `"a"`, "If-None-Match", `"v1"`), `"v1"`, "", conditionNotModified},
			{"列表中任一项命中", header("If-None-Match", `"a", "v1", "b"`), `"v1"`, "", conditionNotModified},
			{"星号命中任何现有表示", header("If-None-Match", "*"), "", "", conditionNotModified},
			{"列表均不匹配", header("If-None-Match", `"a", "b"`), `"v1"`, "", conditionNone},
			{"有效条件但副本没有 ETag", header("If-None-Match", `"v1"`), "", "", conditionUnresolved},
			{"有效条件但副本 ETag 读不懂", header("If-None-Match", `"v1"`), "v1", "", conditionUnresolved},
			{"无效语法按未提供处理", header("If-None-Match", "v1"), "", "", conditionNone},
			{"无效语法退回 If-Modified-Since", header(
				"If-None-Match", "v1", "If-Modified-Since", lastModified), `"v1"`, lastModified, conditionNotModified},
			{"If-None-Match 不匹配时不再看 If-Modified-Since", header(
				"If-None-Match", `"other"`, "If-Modified-Since", lastModified), `"v1"`, lastModified, conditionNone},
			{"资源未晚于请求时间", header("If-Modified-Since", lastModified), "", lastModified, conditionNotModified},
			{"请求日期晚于资源", header("If-Modified-Since", "Thu, 22 Oct 2015 07:28:00 GMT"), "", lastModified, conditionNotModified},
			{"资源较新", header("If-Modified-Since", "Tue, 20 Oct 2015 07:28:00 GMT"), "", lastModified, conditionNone},
			{"请求日期无效按未提供处理", header("If-Modified-Since", "not a date"), "", "", conditionNone},
			{"If-Modified-Since 但副本没有 Last-Modified", header("If-Modified-Since", lastModified), "", "", conditionUnresolved},
			{"If-Modified-Since 但副本日期读不懂", header("If-Modified-Since", lastModified), "", "not a date", conditionUnresolved},
		}
		for _, c := range cases {
			convey.Convey(c.name, func() {
				convey.So(evaluateConditional(c.header, c.etag, c.mod), convey.ShouldEqual, c.want)
			})
		}
	})
}
