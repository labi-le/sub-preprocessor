package rewrite

import (
	"bytes"
	"net/netip"
	"strings"

	"domains.lst/sub-preprocessor/internal/geofeed"
	"domains.lst/sub-preprocessor/internal/subscription"
)

const (
	decimalBase = 10
	hundred     = 100
)

func NodeName(b *bytes.Buffer, node subscription.Node, country geofeed.CountryCode, ip netip.Addr) {
	if !supportsFragmentRewrite(node) {
		b.WriteString(node.Raw)
		return
	}

	cleanName := StripKnownTags(node.Name)
	if cleanName == "" {
		cleanName = node.Server
	}

	// vmess carries its name in the base64 JSON "ps" field, not a URI
	// fragment, so the geo/IP tag is folded into the payload and re-encoded.
	if node.Scheme == subscription.SchemeVmess {
		var name bytes.Buffer
		name.WriteString("[GEO:")
		name.WriteByte(country[0])
		name.WriteByte(country[1])
		name.WriteString("][IP:")
		ip4 := ip.As4()
		writeOctet(&name, ip4[0])
		name.WriteByte('.')
		writeOctet(&name, ip4[1])
		name.WriteByte('.')
		writeOctet(&name, ip4[2])
		name.WriteByte('.')
		writeOctet(&name, ip4[3])
		name.WriteString("] ")
		name.WriteString(cleanName)
		if out, ok := subscription.RewriteVmessName(node.Raw, name.String()); ok {
			b.WriteString(out)
			return
		}
		b.WriteString(node.Raw)
		return
	}

	if node.FragmentIdx >= 0 {
		b.WriteString(node.Raw[:node.FragmentIdx])
	} else {
		b.WriteString(node.Raw)
	}
	b.WriteString("#[GEO:")
	b.WriteByte(country[0])
	b.WriteByte(country[1])
	b.WriteString("][IP:")
	ip4 := ip.As4()
	writeOctet(b, ip4[0])
	b.WriteByte('.')
	writeOctet(b, ip4[1])
	b.WriteByte('.')
	writeOctet(b, ip4[2])
	b.WriteByte('.')
	writeOctet(b, ip4[3])
	b.WriteString("] ")
	b.WriteString(cleanName)
}

func writeOctet(b *bytes.Buffer, n byte) {
	switch {
	case n >= hundred:
		b.WriteByte('0' + n/hundred)
		b.WriteByte('0' + (n/decimalBase)%decimalBase)
		b.WriteByte('0' + n%decimalBase)
	case n >= decimalBase:
		b.WriteByte('0' + n/decimalBase)
		b.WriteByte('0' + n%decimalBase)
	default:
		b.WriteByte('0' + n)
	}
}

func supportsFragmentRewrite(node subscription.Node) bool {
	return node.Scheme != ""
}

func StripKnownTags(s string) string {
	// Scan to find the end of all contiguous known tags without slicing.
	// Performs exactly one slice and one TrimSpace at the end.
	pos := 0
	for pos < len(s) {
		// Skip leading whitespace
		for pos < len(s) && (s[pos] == ' ' || s[pos] == '\t') {
			pos++
		}
		if pos >= len(s) || s[pos] != '[' {
			return strings.TrimSpace(s[pos:])
		}
		end := strings.IndexByte(s[pos:], ']')
		if end < 0 {
			return strings.TrimSpace(s[pos:])
		}
		tagStart := pos + 1
		tagEnd := pos + end
		tag := s[tagStart:tagEnd]
		if isKnownTag(tag) {
			pos = tagEnd + 1
			continue
		}
		return strings.TrimSpace(s[pos:])
	}
	return ""
}

// LeadingTags returns the contiguous run of known [KEY:VAL] tags at the start of
// s (e.g. "[GEO:FI][IP:1.2.3.4]"), or "" if none. Complement of StripKnownTags.
func LeadingTags(s string) string {
	pos, last := 0, 0
	for pos < len(s) {
		for pos < len(s) && (s[pos] == ' ' || s[pos] == '\t') {
			pos++
		}
		if pos >= len(s) || s[pos] != '[' {
			break
		}
		end := strings.IndexByte(s[pos:], ']')
		if end < 0 {
			break
		}
		if !isKnownTag(s[pos+1 : pos+end]) {
			break
		}
		pos += end + 1
		last = pos
	}
	return strings.TrimSpace(s[:last])
}

func isKnownTag(tag string) bool {
	if tag == "OK" || tag == "BAD" {
		return true
	}
	if len(tag) >= 4 && tag[:4] == "GEO:" {
		return true
	}
	if len(tag) >= 3 && tag[:3] == "IP:" {
		return true
	}
	if len(tag) >= 4 && tag[:4] == "JUR:" {
		return true
	}
	if len(tag) >= 4 && tag[:4] == "SPD:" {
		return true
	}
	return false
}
