package subscription

import "strings"

// mieruPort returns the first "port" query value of a mierus:// link, verbatim:
// it may be a range such as "9998-9999" (an accepted mieru form), and this
// service only ever uses it as a dedupe and dead-cache key, never dials it.
//
// tail is everything after the authority, so it still carries the '/' or '?'
// that ended it, plus any fragment.
//
// It reports false when there is no "port", when the first "port" carries no
// value, or when the number of "port" values differs from the number of
// "protocol" values. mihomo expands one mierus:// link into one proxy per port
// paired with the protocol at the same index, drops the link outright when the
// lists do not line up (convert/converter.go:656-660) and strconv.Atoi's an
// empty port into an error (:679-684), so any of the three yields zero proxies
// there. Keeping such a node only burns probe budget and books a fabricated
// server:port into the dead cache and the dedupe map.
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
	if ports == 0 || ports != protocols || first == "" {
		return "", false
	}
	return first, true
}
