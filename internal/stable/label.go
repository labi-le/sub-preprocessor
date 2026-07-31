package stable

import (
	"strings"

	mihomo "github.com/metacubex/mihomo/constant"
)

// entryLabel maps a mihomo proxy back onto the Entry.Label it was built from.
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
func entryLabel(px mihomo.Proxy) string {
	name := px.Name()
	if px.Type() != mihomo.Mieru {
		return name
	}
	i := strings.LastIndexByte(name, ':')
	if i <= 0 {
		return name
	}
	return name[:i]
}
