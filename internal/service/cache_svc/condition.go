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
	// conditionPreconditionFailed If-Match 或 If-Unmodified-Since 失败：返回本地 412。
	conditionPreconditionFailed
	// conditionNotModified If-None-Match 或 If-Modified-Since 命中：返回本地 304。
	conditionNotModified
	// conditionUnresolved 有效条件存在，但副本缺少能求值的 validator：必须回源。
	conditionUnresolved
)

// evaluateConditional 拿请求的条件头与副本的两个 validator 求值。
//
// 优先级按 RFC 9110 §13.2.2：If-Match 先于 If-Unmodified-Since，If-None-Match
// 先于 If-Modified-Since；有效的 entity-tag 条件会压过同组的日期条件。语法无效的
// 条件按未提供处理，于是它会正常地落到下一个条件或普通命中。
func evaluateConditional(header http.Header, etag, lastModified string) conditionOutcome {
	ifMatchPresent := false
	if values := header.Values("If-Match"); len(values) > 0 {
		value := strings.TrimSpace(strings.Join(values, ","))
		if value != "" {
			matched, ok := evaluateIfMatch(value, etag)
			if ok {
				if !matched {
					if strings.TrimSpace(etag) == "" {
						// 条件有效，但这份副本没有任何可参与强比较的 ETag：判不了
						// 成败，交回源去问——与下面 If-None-Match 的同名分支同一
						// 策略。副本上有 ETag（哪怕只是弱的）时照常按强比较判 412。
						return conditionUnresolved
					}
					return conditionPreconditionFailed
				}
				ifMatchPresent = true
			}
		}
	}
	if !ifMatchPresent {
		if value := strings.TrimSpace(header.Get("If-Unmodified-Since")); value != "" {
			requested, err := http.ParseTime(value)
			if err == nil {
				modified, modifiedErr := http.ParseTime(lastModified)
				if modifiedErr != nil {
					return conditionUnresolved
				}
				if modified.After(requested) {
					return conditionPreconditionFailed
				}
			}
		}
	}

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

// evaluateIfMatch applies the strong comparison function to the list.
// A wildcard succeeds because this function is only called for a selected local representation.
func evaluateIfMatch(value, storedValue string) (matched, valid bool) {
	if strings.TrimSpace(value) == "*" {
		return true, true
	}
	stored, storedStrong := parseStrongETag(storedValue)
	tags, weaks, ok := parseETagList(value)
	if !ok {
		return false, false
	}
	// 整张表都读完才给结论：`"a", 碎片` 这样的后半段坏列表按 RFC 得整条忽略，
	// 不能因为前一项已经比中就提前放行。
	for index, tag := range tags {
		if !weaks[index] && storedStrong && tag == stored {
			matched = true
		}
	}
	return matched, true
}

func parseStrongETag(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "W/") {
		return "", false
	}
	return parseETag(value)
}

// parseIfNoneMatch 解析 If-None-Match 的取值，返回表里的 opaque tag、是否星号、
// 以及语法是否有效。
func parseIfNoneMatch(value string) (tags []string, wildcard bool, ok bool) {
	if strings.TrimSpace(value) == "*" {
		return nil, true, true
	}
	tags, _, ok = parseETagList(value)
	return tags, false, ok
}

// parseETagList 解析一个 entity-tag 列表（RFC 9110 §5.6.1.2 的 #entity-tag），
// 返回逐项的 opaque tag 与弱标识。If-Match 的强比较要知道每一项带不带 W/，
// If-None-Match 的弱比较不用，共用这一个扫描器。
//
// 逗号只在引号之外才分隔两个 tag：opaque tag 自己可以含逗号（RFC 9110 §8.8.1 的
// etagc 包含 `,`），按逗号直接切会把一个合法的校验符切成两个读不懂的碎片。空元素
// 按 §5.6.1.2 容忍并忽略；但整张表一个 tag 都没有就是无效，得回源。星号是整条
// 头的语义而不是列表元素，由两个调用方各自先行处理。
func parseETagList(value string) (tags []string, weaks []bool, ok bool) {
	s := value
	for {
		s = strings.TrimLeft(s, " \t")
		if s == "" {
			break
		}
		if s[0] == ',' {
			s = s[1:]
			continue
		}
		weak := strings.HasPrefix(s, "W/")
		tag, rest, valid := scanETag(s)
		if !valid {
			return nil, nil, false
		}
		tags = append(tags, tag)
		weaks = append(weaks, weak)
		s = strings.TrimLeft(rest, " \t")
		if s == "" {
			break
		}
		if s[0] != ',' {
			return nil, nil, false
		}
		s = s[1:]
	}
	if len(tags) == 0 {
		return nil, nil, false
	}
	return tags, weaks, true
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
