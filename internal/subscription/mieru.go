package subscription

import "strings"

// mieruPort returns the first "port" query value of a mierus:// link, verbatim:
// it may be a range such as "9998-9999" (an accepted mieru form), and this
// service only ever uses it as a dedupe and dead-cache key, never dials it.
//
// tail is everything after the authority, so it still carries the '/' or '?'
// that ended it, plus any fragment.
//
// It reports false when there is no "port", or when the number of "port" values
// differs from the number of "protocol" values: mihomo expands one mierus://
// link into one proxy per port paired with the protocol at the same index and
// drops the link outright when the lists do not line up
// (convert/converter.go:656-660), so keeping it only burns probe budget on a
// node that can never be selected.
func mieruPort(tail string) (string, bool) {
	// The fragment is stripped first: "#?port=…" is a label, not a query.
	if i := strings.IndexByte(tail, '#'); i >= 0 {
		tail = tail[:i]
	}
	_, query, found := strings.Cut(tail, "?")
	if !found {
		return "", false
	}

	first := ""
	ports, protocols := 0, 0
	for query != "" {
		var pair string
		pair, query, _ = strings.Cut(query, "&")
		switch key, value, _ := strings.Cut(pair, "="); key {
		case "port":
			ports++
			if ports == 1 {
				first = value
			}
		case "protocol":
			protocols++
		}
	}
	if ports == 0 || ports != protocols {
		return "", false
	}
	return first, true
}
