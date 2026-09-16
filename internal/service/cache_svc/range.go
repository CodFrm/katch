package cache_svc

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"

	"github.com/CodFrm/katch/internal/service/proxy_svc"
)

type byteRange struct {
	start  int64
	length int64
}

type rangeParseResult int

const (
	rangeIgnored rangeParseResult = iota
	rangeSatisfiable
	rangeUnsatisfiable
)

type readerWithCloser struct {
	io.Reader
	io.Closer
}

// representationResponse applies the already-evaluated preconditions, then selects a GET range.
// readerAt lets range bodies stream from disk; owner keeps the underlying descriptor alive and is
// closed on every bodyless branch.
func representationResponse(target *proxy_svc.Target, owner io.ReadCloser, readerAt io.ReaderAt,
	header http.Header, size int64, outcome conditionOutcome,
) (io.ReadCloser, *proxy_svc.Meta) {
	switch outcome {
	case conditionPreconditionFailed:
		_ = owner.Close()
		return http.NoBody, preconditionFailedMeta(header)
	case conditionNotModified:
		_ = owner.Close()
		return http.NoBody, notModifiedMeta(header)
	}
	if target.Method == http.MethodHead {
		_ = owner.Close()
		return http.NoBody, fullRepresentationMeta(header, size)
	}

	ranges, result := parseRangeHeader(target.Header.Values("Range"), size)
	if result == rangeIgnored || !ifRangeMatches(target.Header.Values("If-Range"), header) {
		return owner, fullRepresentationMeta(header, size)
	}
	if result == rangeUnsatisfiable {
		_ = owner.Close()
		return http.NoBody, rangeNotSatisfiableMeta(header, size)
	}
	if len(ranges) == 1 {
		r := ranges[0]
		h := header.Clone()
		h.Set("Content-Range", contentRange(r, size))
		h.Set("Content-Length", strconv.FormatInt(r.length, 10))
		return &readerWithCloser{Reader: io.NewSectionReader(readerAt, r.start, r.length), Closer: owner},
			&proxy_svc.Meta{StatusCode: http.StatusPartialContent, Header: h, ContentLength: r.length}
	}

	body, contentType, length, ok := multipartRangeBody(owner, readerAt, ranges, size, header.Get("Content-Type"))
	if !ok {
		return owner, fullRepresentationMeta(header, size)
	}
	h := header.Clone()
	h.Del("Content-Range")
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", strconv.FormatInt(length, 10))
	return body, &proxy_svc.Meta{StatusCode: http.StatusPartialContent, Header: h, ContentLength: length}
}

func fullRepresentationMeta(header http.Header, size int64) *proxy_svc.Meta {
	h := header.Clone()
	h.Del("Content-Range")
	h.Set("Content-Length", strconv.FormatInt(size, 10))
	return &proxy_svc.Meta{StatusCode: http.StatusOK, Header: h, ContentLength: size}
}

func preconditionFailedMeta(header http.Header) *proxy_svc.Meta {
	h := header.Clone()
	h.Del("Content-Length")
	h.Del("Content-Type")
	h.Del("Content-Range")
	return &proxy_svc.Meta{StatusCode: http.StatusPreconditionFailed, Header: h}
}

func rangeNotSatisfiableMeta(header http.Header, size int64) *proxy_svc.Meta {
	h := header.Clone()
	h.Del("Content-Length")
	h.Del("Content-Type")
	h.Set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
	return &proxy_svc.Meta{StatusCode: http.StatusRequestedRangeNotSatisfiable, Header: h}
}

func parseRangeHeader(values []string, size int64) ([]byteRange, rangeParseResult) {
	if len(values) == 0 {
		return nil, rangeIgnored
	}
	value := strings.TrimSpace(strings.Join(values, ","))
	unit, set, found := strings.Cut(value, "=")
	if !found || !strings.EqualFold(strings.TrimSpace(unit), "bytes") || strings.TrimSpace(set) == "" {
		return nil, rangeIgnored
	}

	parts := strings.Split(set, ",")
	ranges := make([]byteRange, 0, len(parts))
	var selected int64
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if strings.ContainsAny(part, " \t") {
			return nil, rangeIgnored
		}
		startText, endText, found := strings.Cut(part, "-")
		if !found || strings.Contains(endText, "-") {
			return nil, rangeIgnored
		}
		startText = strings.TrimSpace(startText)
		endText = strings.TrimSpace(endText)
		if startText == "" {
			suffix, err := parseRangeNumber(endText)
			if err != nil {
				return nil, rangeIgnored
			}
			if suffix == 0 || size == 0 {
				continue
			}
			if suffix > size {
				suffix = size
			}
			r := byteRange{start: size - suffix, length: suffix}
			var ok bool
			selected, ok = checkedAdd(selected, r.length)
			if !ok || selected > size {
				return nil, rangeIgnored
			}
			ranges = append(ranges, r)
			continue
		}

		start, err := parseRangeNumber(startText)
		if err != nil {
			return nil, rangeIgnored
		}
		end := size - 1
		if endText != "" {
			end, err = parseRangeNumber(endText)
			if err != nil || end < start {
				return nil, rangeIgnored
			}
		}
		if start >= size {
			// A syntactically valid member can be unsatisfiable while another member is usable.
			continue
		}
		if end >= size {
			end = size - 1
		}
		r := byteRange{start: start, length: end - start + 1}
		var ok bool
		selected, ok = checkedAdd(selected, r.length)
		if !ok || selected > size {
			// Ignore range sets that would amplify one representation into more selected bytes.
			return nil, rangeIgnored
		}
		ranges = append(ranges, r)
	}
	if len(ranges) == 0 {
		return nil, rangeUnsatisfiable
	}
	return ranges, rangeSatisfiable
}

func parseRangeNumber(value string) (int64, error) {
	if value == "" || strings.HasPrefix(value, "+") || strings.HasPrefix(value, "-") {
		return 0, strconv.ErrSyntax
	}
	return strconv.ParseInt(value, 10, 64)
}

func ifRangeMatches(values []string, header http.Header) bool {
	if len(values) == 0 {
		return true
	}
	if len(values) != 1 {
		return false
	}
	value := strings.TrimSpace(values[0])
	if value == "" {
		return false
	}
	if strings.HasPrefix(value, `"`) || strings.HasPrefix(value, "W/") {
		requested, requestedStrong := parseStrongETag(value)
		stored, storedStrong := parseStrongETag(header.Get("Etag"))
		return requestedStrong && storedStrong && requested == stored
	}
	requested, err := http.ParseTime(value)
	if err != nil {
		return false
	}
	modified, err := http.ParseTime(header.Get("Last-Modified"))
	if err != nil {
		return false
	}
	return requested.Unix() == modified.Unix()
}

func contentRange(r byteRange, size int64) string {
	return fmt.Sprintf("bytes %d-%d/%d", r.start, r.start+r.length-1, size)
}

func multipartRangeBody(owner io.ReadCloser, readerAt io.ReaderAt, ranges []byteRange, size int64,
	contentType string,
) (io.ReadCloser, string, int64, bool) {
	boundary := multipart.NewWriter(io.Discard).Boundary()
	partType := safeMultipartContentType(contentType)
	readers := make([]io.Reader, 0, len(ranges)*2+1)
	var total int64
	for i, r := range ranges {
		var prefix strings.Builder
		if i > 0 {
			prefix.WriteString("\r\n")
		}
		_, _ = fmt.Fprintf(&prefix, "--%s\r\nContent-Range: %s\r\n", boundary, contentRange(r, size))
		if partType != "" {
			_, _ = fmt.Fprintf(&prefix, "Content-Type: %s\r\n", partType)
		}
		prefix.WriteString("\r\n")
		framing := []byte(prefix.String())
		var ok bool
		total, ok = checkedAdd(total, int64(len(framing)))
		if !ok {
			return nil, "", 0, false
		}
		total, ok = checkedAdd(total, r.length)
		if !ok {
			return nil, "", 0, false
		}
		readers = append(readers, bytes.NewReader(framing), io.NewSectionReader(readerAt, r.start, r.length))
	}
	suffix := []byte("\r\n--" + boundary + "--\r\n")
	var ok bool
	total, ok = checkedAdd(total, int64(len(suffix)))
	if !ok {
		return nil, "", 0, false
	}
	readers = append(readers, bytes.NewReader(suffix))
	responseType := mime.FormatMediaType("multipart/byteranges", map[string]string{"boundary": boundary})
	return &readerWithCloser{Reader: io.MultiReader(readers...), Closer: owner}, responseType, total, true
}

func safeMultipartContentType(value string) string {
	mediaType, params, err := mime.ParseMediaType(value)
	if err != nil || strings.ContainsAny(mediaType, "\r\n") {
		return ""
	}
	return mime.FormatMediaType(mediaType, params)
}

func checkedAdd(a, b int64) (int64, bool) {
	if a < 0 || b < 0 || a > math.MaxInt64-b {
		return 0, false
	}
	return a + b, true
}
