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

func isKnownTag(tag string) bool {
	if len(tag) == 0 {
		return false
	}
	if len(tag) == 2 && (tag == "OK" || tag == "BAD") {
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
	return false
}
