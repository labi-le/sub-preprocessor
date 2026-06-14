package rewrite

import (
	"net/netip"
	"strings"

	"domains.lst/sub-preprocessor/internal/subscription"
)

const (
	decimalBase = 10
	hundred     = 100
)

func NodeName(b *strings.Builder, node subscription.Node, country string, ip netip.Addr) {
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
	b.WriteString(country)
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

func writeOctet(b *strings.Builder, n byte) {
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
	s = strings.TrimSpace(s)
	for {
		if !strings.HasPrefix(s, "[") {
			return strings.TrimSpace(s)
		}
		end := strings.Index(s, "]")
		if end < 0 {
			return strings.TrimSpace(s)
		}
		tag := s[1:end]
		if strings.HasPrefix(tag, "GEO:") || strings.HasPrefix(tag, "IP:") || strings.HasPrefix(tag, "JUR:") || tag == "OK" || tag == "BAD" {
			s = strings.TrimSpace(s[end+1:])
			continue
		}
		return strings.TrimSpace(s)
	}
}
