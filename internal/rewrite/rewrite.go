package rewrite

import (
	"bytes"
	"strings"

	"domains.lst/sub-preprocessor/internal/subscription"
)

// NodeName writes node to b with the given already-formatted tag prefix folded
// into its published name, e.g. tags="[GEO:AE]" produces
// "...#[GEO:AE] Old Name". `[GEO:]` is the only prefix this service can emit
// from the annotate list -- preprocess.annotator has one rendering arm left,
// config.TagGEO, and it writes either an ISO-3166-1 alpha-2 code or "??" --
// and the stable worker prepends its own "[SPD:<n>M] " ahead of it. An empty
// tags string writes the node with its known-tag prefix stripped (annotation
// reduced to a clean relabel). Nodes that do not support fragment rewrites are
// written verbatim.
func NodeName(b *bytes.Buffer, node subscription.Node, tags string) {
	if !supportsFragmentRewrite(node) {
		b.WriteString(node.Raw)
		return
	}

	cleanName := StripKnownTags(node.Name)
	if cleanName == "" {
		cleanName = node.Server
	}

	// vmess and ssr carry their display name inside the base64 payload rather
	// than in a URI fragment, so the tag prefix is folded into the payload and
	// re-encoded. For ssr a fragment would be actively harmful: mihomo
	// base64-decodes everything after "ssr://", the "#name" included, so an
	// appended fragment turns the node into "format invalid". An undecodable
	// payload is published verbatim — unannotated beats mangled.
	switch node.Scheme { //nolint:exhaustive // ss and mierus name their node in the URI fragment, i.e. the generic path below
	case subscription.SchemeVmess:
		if out, ok := subscription.RewriteVmessName(node.Raw, displayName(tags, cleanName)); ok {
			b.WriteString(out)
			return
		}
		b.WriteString(node.Raw)
		return
	case subscription.SchemeSSR:
		if out, ok := subscription.RewriteSSRName(node.Raw, displayName(tags, cleanName)); ok {
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
	if tags != "" {
		b.WriteString(tags)
		b.WriteByte(' ')
	}
	b.WriteString(cleanName)
}

// displayName is called only by the two payload arms: they re-encode the name
// inside base64 and so need it contiguous. The fragment path writes the same
// bytes in three writes and pays no allocation for them -- ~300000 nodes a
// cycle, measured 2026-08-18 as 99.6% of alloc_objects in NodeName.
func displayName(tags, cleanName string) string {
	if tags == "" {
		return cleanName
	}

	return tags + " " + cleanName
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

// isKnownTag reports whether tag is one this service strips off an upstream
// name. The set is deliberately WIDER than the set we write: only `GEO:` (the
// one configured annotate tag) and `SPD:` (stable.speedPrefix) are ever
// authored here; `IP:`, `ASN:`, `JUR:`, `OK` and `BAD` are recognised so that
// an upstream-authored tag is removed on relabel instead of accumulating in
// front of ours.
//
// `IP:` and `ASN:` have no writer left -- both annotate tags were removed --
// and both must still stay, on the only path that strips anything: an
// ANNOTATING config. With `annotate: []` the annotator is nil, so nothing
// calls NodeName and this function never runs at all:
// preprocess.bufferSink.emit publishes node.Raw verbatim, while
// stable.BuildPayload's nil-annotator arm publishes Survivor.Raw -- the
// Entry.Raw that stable.Merge had already relabelled to <source>-NNN, so no
// upstream name reaches /stable.txt whatever this function recognises. On `/`
// every upstream tag survives.
//
// Where it does run, the scan consumes a CONTIGUOUS run and returns the
// remainder from the first tag it does not recognise, so dropping either arm
// costs more than the tag it drops -- and each arm needs a shape that CONTAINS
// its own tag to show it, because a shape without one is republished
// identically either way. Without `ASN:` an upstream
// `[GEO:RU][ASN:SOME-AS, RU] Moscow` loses only its GEO tag and we republish
// `[GEO:xx] [ASN:SOME-AS, RU] Moscow`. Without `IP:` an upstream
// `[GEO:RU][IP:1.2.3.4] Moscow` republishes as `[GEO:xx] [IP:1.2.3.4] Moscow`,
// leaking an address we never verified. Measured both ways: deleting the `IP:`
// arm leaves the ASN shape above stripping to `Moscow` unchanged, so testing
// that shape is what makes the arm look dead.
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
