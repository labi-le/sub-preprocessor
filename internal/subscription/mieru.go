package subscription

import "strings"

// mieruPort returns the first non-empty "port" query value of a mierus:// link,
// verbatim: it may be a range such as "9998-9999" (an accepted mieru form), and
// this service only ever uses it as a dedupe and dead-cache key, never dials it.
//
// tail is everything after the authority, so it still carries the '/' or '?'
// that ended it, plus any fragment.
//
// It reports false when there is no "port", when every "port" carries no value,
// or when the number of "port" values differs from the number of "protocol"
// values. mihomo expands one mierus:// link into one proxy per port paired with
// the protocol at the same index and drops the WHOLE link when the two lists do
// not line up (convert/converter.go:656-660). A valueless port, by contrast,
// only loses its own pair: strconv.Atoi's failure continues the inner per-port
// loop (:678-681), so "?port=&port=3000&protocol=TCP&protocol=UDP" still
// converts to one working proxy on 3000. Hence the first port with a value, not
// the first port. Only when no port has one is there nothing to keep, and
// keeping it would burn probe budget under a fabricated server:port booked into
// the dead cache and the dedupe map.
func mieruPort(tail string) (string, bool) {
	// The fragment is stripped first: "#?port=…" is a label, not a query.
	if i := strings.IndexByte(tail, '#'); i >= 0 {
		tail = tail[:i]
	}
	_, query, found := strings.Cut(tail, "?")
	if !found {
		return "", false
	}

	port := ""
	ports, protocols := 0, 0
	for query != "" {
		var pair string
		pair, query, _ = strings.Cut(query, "&")
		switch key, value, _ := strings.Cut(pair, "="); key {
		case "port":
			ports++
			if port == "" {
				port = value
			}
		case "protocol":
			protocols++
		}
	}
	if ports == 0 || ports != protocols || port == "" {
		return "", false
	}
	return port, true
}
