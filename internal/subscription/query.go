package subscription

import (
	"net/url"
	"slices"
	"strings"
)

// queryPair is one unescaped key/value of a share-link query.
type queryPair struct{ key, value string }

// queryList is url.Values over a caller-owned array. ssr and xray links carry a
// handful of keys each, and the map form charged a []string per key: measured
// 2026-08-18, net/url.parseQuery was 76% of BenchmarkParse_SSR's alloc_objects
// and url.Values.Set/Encode 38% of the xray path's.
//
// Every mutator returns the new list, as append does, and for the same reason:
// a pointer receiver storing into *q makes the caller's array escape.
type queryList []queryPair

func (q queryList) add(key, value string) queryList {
	return append(q, queryPair{key: key, value: value})
}

// set leaves the key holding exactly one value, as url.Values.Set does. The
// first occurrence keeps its position and the rest go, which encode cannot tell
// apart from the map form because it sorts by key anyway.
func (q queryList) set(key, value string) queryList {
	kept := q[:0]
	found := false
	for _, p := range q {
		switch {
		case p.key != key:
			kept = append(kept, p)
		case !found:
			p.value = value
			kept = append(kept, p)
			found = true
		}
	}
	if !found {
		return kept.add(key, value)
	}

	return kept
}

func (q queryList) get(key string) string {
	for i := range q {
		if q[i].key == key {
			return q[i].value
		}
	}

	return ""
}

// maxQueryParams is net/url's own ceiling (parseQuery -> urlParamsWithinMax,
// defaultMaxParams, go1.26.5), counting empty and repeated segments: mihomo's
// converter parses under it, so a payload past it can never publish.
const maxQueryParams = 10000

// parse mirrors net/url.parseQuery, including its segment ceiling and its
// rejection of a ';' separator and of an undecodable escape. It returns on the
// first of those where ParseQuery collects the rest, which no caller can
// observe: every one of them discards the values when ParseQuery hands back an
// error.
func (q queryList) parse(raw string) (queryList, bool) {
	if strings.Count(raw, "&")+1 > maxQueryParams {
		return q, false
	}
	for raw != "" {
		var segment string
		segment, raw, _ = strings.Cut(raw, "&")
		if strings.Contains(segment, ";") {
			return q, false
		}
		if segment == "" {
			continue
		}
		key, value, _ := strings.Cut(segment, "=")
		key, ok := unescapeQuery(key)
		if !ok {
			return q, false
		}
		value, ok = unescapeQuery(value)
		if !ok {
			return q, false
		}
		q = q.add(key, value)
	}

	return q, true
}

// appendEncoded writes the pairs as url.Values.Encode would.
func (q queryList) appendEncoded(dst []byte) []byte {
	q.sortByKey()
	for i := range q {
		if i > 0 {
			dst = append(dst, '&')
		}
		dst = appendQueryEscape(dst, q[i].key)
		dst = append(dst, '=')
		dst = appendQueryEscape(dst, q[i].value)
	}

	return dst
}

// rawLen is the encoded length before escaping: exact where nothing escapes,
// a floor otherwise.
func (q queryList) rawLen() int {
	n := 0
	for i := range q {
		n += len(q[i].key) + len(q[i].value) + len("=&")
	}
	if n > 0 {
		n -= len("&")
	}

	return n
}

// sortByKey orders the pairs as url.Values.Encode does, stable so repeated keys
// keep the order url.Values would have given their []string. n log n because
// RewriteSSRName hands it whatever the source's payload carried, not the ten
// keys the xray builders do.
func (q queryList) sortByKey() {
	slices.SortStableFunc(q, func(a, b queryPair) int { return strings.Compare(a.key, b.key) })
}

// unescapeQuery is url.QueryUnescape, skipping the call on the values that
// carry nothing to unescape — which is every base64 value an ssr query holds.
func unescapeQuery(s string) (string, bool) {
	if strings.IndexByte(s, '%') < 0 && strings.IndexByte(s, '+') < 0 {
		return s, true
	}
	v, err := url.QueryUnescape(s)

	return v, err == nil
}

const upperhex = "0123456789ABCDEF"

// unreserved reports whether c is an RFC 3986 §2.3 unreserved byte, the set
// every net/url escaping mode leaves alone.
func unreserved(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '-' || c == '_' || c == '.' || c == '~'
}

// appendQueryEscape is url.QueryEscape in append form; TestEscapersMatchNetURL
// pins the two against each other over every byte.
func appendQueryEscape(dst []byte, s string) []byte {
	for i := range len(s) {
		switch c := s[i]; {
		case unreserved(c):
			dst = append(dst, c)
		case c == ' ':
			dst = append(dst, '+')
		default:
			dst = append(dst, '%', upperhex[c>>4], upperhex[c&0xf])
		}
	}

	return dst
}

// appendPathEscape is url.PathEscape in append form.
func appendPathEscape(dst []byte, s string) []byte {
	for i := range len(s) {
		c := s[i]
		if unreserved(c) || pathSegmentSafe(c) {
			dst = append(dst, c)
			continue
		}
		dst = append(dst, '%', upperhex[c>>4], upperhex[c&0xf])
	}

	return dst
}

// pathSegmentSafe reports whether c is one of the reserved bytes net/url leaves
// unescaped inside a path segment: of "$&+,/:;=?@" it escapes only "/;,?".
func pathSegmentSafe(c byte) bool {
	switch c {
	case '$', '&', '+', ':', '=', '@':
		return true
	}

	return false
}
