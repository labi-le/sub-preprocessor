package subscription

import "strings"

// mieruPort returns, verbatim, the first "port" query value of a mierus:// link
// that mihomo will actually serve on: a decimal port, or a range such as
// "9998-9999" (an accepted mieru form). This service never dials the value --
// it is the server:port dedupe key and the dead-cache key -- so what it must
// name is the port the node ends up published on, not the first one written
// down.
//
// tail is everything after the authority, so it still carries the '/' or '?'
// that ended it, plus any fragment.
//
// An unusable value costs only its own pair, not the link: mihomo walks the
// port list against the protocol list and, for a port it cannot turn into a
// number, continues the inner loop (convert/converter.go:662-683). So
// "?port=abc&port=3000&protocol=TCP&protocol=UDP" still converts, to a single
// proxy on 3000; taking "abc" would book that node under a port nothing answers
// on, splitting it from its duplicates and poisoning a dead-cache key no probe
// can ever clear. Hence the first usable port, not the first port.
//
// It reports false when there is no "port", when no "port" value is usable, or
// when the number of "port" values differs from the number of "protocol"
// values. mihomo pairs the two lists by index and drops the WHOLE link when
// they do not line up (:656-660); a link with no usable port has nothing left
// to keep, and keeping it would burn probe budget on a fabricated server:port.
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
			if port == "" && mieruUsablePort(value) {
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

// mieruUsablePort reports whether a raw "port" query value becomes a live mieru
// proxy. mihomo's converter is the looser of the two gates it has to pass: a
// value holding no '-' is strconv.Atoi'd, but one that holds a '-' is copied
// into "port-range" unchecked, so the converter alone happily emits an
// "abc-def" proxy. The bound is therefore the adapter's -- validateMieruOption
// wants port-range to scan as "%d-%d" with begin <= end and both ends in
// 1..65535, and a plain port in the same window (adapter/outbound/mieru.go:301-324)
// -- because adapter.ParseProxy is what the prober feeds the converted map to.
//
// The value is tested as written, before percent-decoding — deliberately
// CONSERVATIVE, never exact: mihomo compares the percent-DECODED value
// (url.Query() unescapes it, converter.go:656), so "port=%33000" reaches it as
// "3000" and converts while this gate refuses the raw "%33000" form. The
// asymmetry is one-directional — we can only be stricter, never looser — and
// the escaped form appears in no real payload, so the rawer test keeps the
// grammar this predicate already owns.
func mieruUsablePort(value string) bool {
	if begin, end, isRange := strings.Cut(value, "-"); isRange {
		b, bok := portNumber(begin)
		e, eok := portNumber(end)
		return bok && eok && b <= e
	}
	_, ok := portNumber(value)
	return ok
}
