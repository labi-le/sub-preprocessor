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
// name can — "weird:src-001:2999/TCP" must fold to "weird:src-001". A name
// with no colon, or one starting with it, is returned unchanged rather than
// folded to "": mihomo's uniqueName can hand us shapes this pattern does not
// describe, and an empty label matches every entry that failed to parse.
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
