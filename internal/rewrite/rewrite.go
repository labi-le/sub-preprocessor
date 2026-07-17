package rewrite

import (
	"bytes"
	"strings"

	"domains.lst/sub-preprocessor/internal/subscription"
)

// NodeName writes node to b with the given already-formatted tag prefix folded
// into its published name, e.g. tags="[GEO:NL][IP:1.2.3.4]" produces
// "...#[GEO:NL][IP:1.2.3.4] Old Name". An empty tags string writes the node
// with its known-tag prefix stripped (annotation reduced to a clean relabel).
// Nodes that do not support fragment rewrites are written verbatim.
func NodeName(b *bytes.Buffer, node subscription.Node, tags string) {
	if !supportsFragmentRewrite(node) {
		b.WriteString(node.Raw)
		return
	}

	cleanName := StripKnownTags(node.Name)
	if cleanName == "" {
		cleanName = node.Server
	}

	name := cleanName
	if tags != "" {
		name = tags + " " + cleanName
	}

	// vmess carries its name in the base64 JSON "ps" field, not a URI
	// fragment, so the tag prefix is folded into the payload and re-encoded.
	if node.Scheme == subscription.SchemeVmess {
		if out, ok := subscription.RewriteVmessName(node.Raw, name); ok {
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
	b.WriteByte('#')
	b.WriteString(name)
}

func supportsFragmentRewrite(node subscription.Node) bool {
	return node.Scheme != ""
}

func StripKnownTags(s string) string {
	// Scan to find the end of all contiguous known tags without slicing.
	// Performs exactly one slice and one TrimSpace at the end.
	pos := 0
	for pos < len(s) {
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
	if len(tag) >= 4 && tag[:4] == "ASN:" {
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
