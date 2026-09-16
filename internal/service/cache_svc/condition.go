package cache_svc

import (
	"net/http"
	"strings"
)

// 本地条件求值：只用副本自己保存下来的两个 validator 回答客户端的条件请求。
//
// 传出去的一切都必须来自上游：副本上没有 ETag 时不能拿摘要替它说话，日期读不懂
// 时也不能按「大概没变」放行。求不了就回源——一次多余的传输，比替上游下一个它
// 从未声明过的结论便宜得多（决策 5）。

// conditionOutcome 一次本地条件求值的结果。
type conditionOutcome int

const (
	// conditionNone 没有可求值的条件，或者条件有效且不命中：按缓存的 200 回答。
	conditionNone conditionOutcome = iota
	// conditionNotModified 条件命中：返回本地 304。
	conditionNotModified
	// conditionUnresolved 有有效条件，但副本缺少能求值的 validator：必须回源。
	conditionUnresolved
)

// evaluateConditional 拿请求的条件头与副本的两个 validator 求值。
//
// 优先级按 RFC 9110 §13.2.2：有 If-None-Match 时不再看 If-Modified-Since——两个
// 条件同时给且结论相反时，日期说的是「可能没变」，ETag 说的是「就是这一份」，
// 后者更硬。语法无效的条件按未提供处理，于是它会正常地落到下一个条件或普通命中。
func evaluateConditional(header http.Header, etag, lastModified string) conditionOutcome {
	if values := header.Values("If-None-Match"); len(values) > 0 {
		// 多行 If-None-Match 与写成一行逗号列表等价（RFC 9110 §5.3）。
		value := strings.TrimSpace(strings.Join(values, ","))
		if value != "" {
			tags, wildcard, ok := parseIfNoneMatch(value)
			if ok {
				if wildcard {
					// 星号问的是「还有没有这份表示」：手上这份完整副本就是回答。
					return conditionNotModified
				}
				stored, storedOK := parseETag(etag)
				if !storedOK {
					// 条件有效却没有任何可比的 ETag：不猜，回源。
					return conditionUnresolved
				}
				for _, tag := range tags {
					if tag == stored {
						return conditionNotModified
					}
				}
				// 有效且明确不匹配：本地 200，不再看 If-Modified-Since。
				return conditionNone
			}
		}
	}
	if value := strings.TrimSpace(header.Get("If-Modified-Since")); value != "" {
		requested, err := http.ParseTime(value)
		if err != nil {
			// 语法无效按未提供处理；副本没有 Last-Modified 也不再是问题。
			return conditionNone
		}
		modified, err := http.ParseTime(lastModified)
		if err != nil {
			return conditionUnresolved
		}
		// 「未晚于请求时间」：HTTP 日期只到秒，相等同样算没变。
		if !modified.After(requested) {
			return conditionNotModified
		}
	}
	return conditionNone
}

// parseIfNoneMatch 解析 If-None-Match 的取值，返回表里的 opaque tag、是否星号、
// 以及语法是否有效。
//
// 逗号只在引号之外才分隔两个 tag：opaque tag 自己可以含逗号（RFC 9110 §8.8.1 的
// etagc 包含 `,`），按逗号直接切会把一个合法的校验符切成两个读不懂的碎片。空元素
// 按 §5.6.1.2 容忍并忽略；但整张表一个 tag 都没有就是无效，得回源。
func parseIfNoneMatch(value string) (tags []string, wildcard bool, ok bool) {
	s := strings.TrimSpace(value)
	if s == "*" {
		return nil, true, true
	}
	for {
		s = strings.TrimLeft(s, " \t")
		if s == "" {
			break
		}
		if s[0] == ',' {
			s = s[1:]
			continue
		}
		tag, rest, valid := scanETag(s)
		if !valid {
			return nil, false, false
		}
		tags = append(tags, tag)
		s = strings.TrimLeft(rest, " \t")
		if s == "" {
			break
		}
		if s[0] != ',' {
			return nil, false, false
		}
		s = s[1:]
	}
	if len(tags) == 0 {
		return nil, false, false
	}
	return tags, false, true
}

// parseETag 解析单个 entity-tag，返回去掉引号与弱标识的 opaque tag。
//
// 弱比较只比 opaque tag，不看 W/ 前缀：这正是 RFC 9110 §8.8.3.2 的弱比较函数。
// `W/` 按大小写敏感处理（RFC 里的字面量就是大写），小写的 w/ 是一个读不懂的串。
func parseETag(value string) (string, bool) {
	opaque, rest, ok := scanETag(strings.TrimSpace(value))
	if !ok || strings.TrimSpace(rest) != "" {
		return "", false
	}
	return opaque, true
}

// scanETag 从 s 的开头读一个 entity-tag，返回它的 opaque tag 与剩余串。
func scanETag(s string) (opaque, rest string, ok bool) {
	s = strings.TrimPrefix(s, "W/")
	if len(s) == 0 || s[0] != '"' {
		return "", "", false
	}
	for i := 1; i < len(s); i++ {
		switch ch := s[i]; {
		case ch == '"':
			return s[1:i], s[i+1:], true
		case ch < 0x21 || ch == 0x7f:
			// etagc = %x21 / %x23-7E / obs-text：控制字符与 DEL 不合法。
			return "", "", false
		}
	}
	return "", "", false
}
