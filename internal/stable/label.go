package stable

import (
	"net"
	"strings"

	mihomo "github.com/metacubex/mihomo/constant"
)

// entryLabel maps a mihomo proxy back onto the Entry.Label it was built from.
func entryLabel(px mihomo.Proxy) string {
	return foldMieruName(px.Name(), px.Type() == mihomo.Mieru)
}

// proxyHost is the server half of a proxy's address, for outcome fields that
// name the probed endpoint.
func proxyHost(px mihomo.Proxy) string {
	host, _, err := net.SplitHostPort(px.Addr())
	if err != nil {
		return px.Addr()
	}
	return host
}

// mappingLabel is entryLabel for a mapping no adapter was built from, which is
// every node the pre-check condemns. mihomo copies both keys verbatim —
// Base.name is option.Name (adapter/outbound/base.go:61) and the mieru port
// suffix is the converter's own (common/convert/converter.go:662-696) — so this
// answers what entryLabel would have for the proxy the mapping parses into.
func mappingLabel(name, typ string) string {
	return foldMieruName(name, typ == "mieru")
}

// foldMieruName folds a proxy name back onto its Entry.Label.
//
// Every scheme but one names its single proxy exactly the label (the URI
// fragment Merge writes). mierus:// is the exception: mihomo expands ONE link
// into one proxy per configured port, named "<label>:<port>/<protocol>"
// (mihomo common/convert/converter.go:662-696). Without folding that back,
// every proxy-name-keyed map in this package misses on lookup by label, and a
// perfectly healthy mieru node is never selected, never checked, and booked
// into the dead cache.
//
// The suffix starts at the LAST ':' because neither a port ("2999",
// "9998-9999") nor a transport ("TCP"/"UDP") can contain one, while a source
// name can — "weird:src-001:2999/TCP" must fold to "weird:src-001". The
// colonless / leading-colon guard describes no shape mihomo emits (it builds
// every mieru name from that exact format, and uniqueName only appends
// "-%02d" to the tail); it is there so that a future naming change degrades
// into an unfolded name rather than collapsing every mieru proxy onto "".
func foldMieruName(name string, mieru bool) string {
	if !mieru {
		return name
	}
	i := strings.LastIndexByte(name, ':')
	if i <= 0 {
		return name
	}
	return name[:i]
}

// sourceOfLabel maps an Entry.Label back onto the source that produced it.
// Merge builds every label as "<source>-NNN", so the split is at the LAST '-':
// a source name may contain '-' and ':' while the pad-3 counter tail cannot.
//
// It returns a substring, never a copy, because countSourceStages calls it from
// both of its passes -- over tested and over the filtered subset of it -- so a
// published node is split twice and a dropped one once. Both passes walk the
// probe's survivors, not the merged pool: 572 calls on prod and 1441 on
// vassago, measured 2026-08-15.
//
// A label of another shape is returned unchanged rather than "" so a future
// naming change degrades into one unattributed row instead of collapsing
// every source onto one key.
func sourceOfLabel(label string) string {
	i := strings.LastIndexByte(label, '-')
	if i <= 0 || i == len(label)-1 {
		return label
	}
	for j := i + 1; j < len(label); j++ {
		if label[j] < '0' || label[j] > '9' {
			return label
		}
	}
	return label[:i]
}
