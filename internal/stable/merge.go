package stable

import (
	"bytes"
	"strconv"
	"strings"
	"unicode/utf8"

	"domains.lst/sub-preprocessor/internal/ioutil"
	"domains.lst/sub-preprocessor/internal/rewrite"
	"domains.lst/sub-preprocessor/internal/subscription"
)

// SourceBody is a fetched (and geo-filtered) subscription payload.
type SourceBody struct {
	Name string
	Body []byte
}

// Entry is a merged node. Raw carries the clean <source>-NNN name used for
// probing; Tagged carries the published name (the [GEO][IP] annotation from the
// filter pass, when present, plus the same unique label). Country mirrors the
// carried [GEO:xx] tag: two ASCII letters, "??" when the annotation chain
// resolved nothing, "" when annotation is off (no tag to judge) or the tag
// carried something that is not a country code — see tagCountry.
type Entry struct {
	Label   string
	Raw     string
	Tagged  string
	Addr    string // server:port, the dead-cache key
	Country string
}

// Merge parses all source bodies in order, dedupes nodes by lowercased
// Server:Port (hostnames are case-insensitive; first source wins) and relabels
// each kept node to <source>-NNN so probe results map back to entries
// unambiguously. NNN counts kept nodes per source. Entry.Addr carries the
// lowercased key so mixed-case duplicates share one dead-cache entry; Raw and
// Tagged keep the original casing.
func Merge(bodies []SourceBody) []Entry {
	// Estimate total nodes cheaply (one line per node) to pre-size collections.
	total := 0
	for _, src := range bodies {
		total += bytes.Count(src.Body, []byte{'\n'}) + 1
	}
	seen := make(map[string]struct{}, total)
	entries := make([]Entry, 0, total)
	var scratch []byte  // reused lowercased server:port key builder
	var labelBuf []byte // reused <source>-NNN label builder
	for _, src := range bodies {
		kept := 0
		subscription.Parse(src.Body, func(n subscription.Node) bool {
			// Dedupe key: lowercased server:port in the reused scratch buffer.
			scratch = lowerServerPort(scratch, n.Server, n.Port)
			// Membership test on the scratch bytes allocates nothing; the real
			// string key is interned only when the node is actually kept.
			if _, dup := seen[string(scratch)]; dup {
				return true
			}
			labelBuf = labelBuf[:0]
			labelBuf = append(labelBuf, src.Name...)
			labelBuf = append(labelBuf, '-')
			labelBuf = appendPad3(labelBuf, kept+1)
			label := string(labelBuf)
			raw, ok := relabelNode(n, label)
			if !ok {
				return true
			}
			tags := rewrite.LeadingTags(n.Name)
			tagged := taggedName(n, raw, label, tags)
			key := string(scratch)
			seen[key] = struct{}{}
			kept++
			entries = append(entries, Entry{Label: label, Raw: raw, Tagged: tagged, Addr: key, Country: tagCountry(tags)})
			return true
		})
	}
	return entries
}

// lowerServerPort appends the lowercased "server:port" dedupe key into dst[:0]
// and returns it. Node servers are virtually always ASCII (bare IPs, punycode
// domains), so the byte-wise fast path handles them zero-alloc; a rare non-ASCII
// server falls back to strings.ToLower for exact parity with the prior key.
func lowerServerPort(dst []byte, server, port string) []byte {
	dst = dst[:0]
	for i := range len(server) {
		c := server[i]
		if c >= utf8.RuneSelf {
			dst = append(dst[:0], strings.ToLower(server)...)
			break
		}
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		dst = append(dst, c)
	}
	dst = append(dst, ':')
	return append(dst, port...)
}

// countryUnknown is the annotator's marker for "no provider resolved a
// country" ([GEO:??]). Returning the constant keeps the common miss alloc-free
// and body-free.
const countryUnknown = "??"

// tagCountry extracts the 2-char code from a "[GEO:xx]" tag anywhere in the
// leading-tags string ("" when absent — annotation off, no GEO tag, or a code
// that is not two ASCII letters).
//
// The result is CLONED rather than sliced. tags is the tail of a zero-copy
// chain out of the parser (subscription.Parse hands parseNode an
// ioutil.UnsafeString view, Node.Name and rewrite.LeadingTags both re-slice
// it) whose root is scheme-dependent: this cycle's whole source body for the
// generic path, a per-node base64 buffer for vmess/ss-legacy/ssr. Either way
// Entry.Country outlives the cycle inside the metrics snapshot, so a 2-byte
// sub-slice would pin its root until the next publication replaces it.
//
// The code is also validated, because node names are source-authored and
// Entry.Country becomes a Prometheus label value in the kept-country gauge:
// without annotation the upstream's own "[GEO:...]" survives verbatim into the
// merge, and one byte a label cannot carry would break the entire scrape.
func tagCountry(tags string) string {
	const marker = "[GEO:"
	i := strings.Index(tags, marker)
	if i < 0 {
		return ""
	}
	code := i + len(marker)
	if len(tags) < code+3 || tags[code+2] != ']' {
		return ""
	}
	c := tags[code : code+2]
	if c == countryUnknown {
		return countryUnknown
	}
	if !asciiLetter(c[0]) || !asciiLetter(c[1]) {
		return ""
	}
	return strings.Clone(c)
}

func asciiLetter(b byte) bool {
	const asciiCaseBit = 0x20 // the single bit by which ASCII letter cases differ
	lower := b | asciiCaseBit
	return lower >= 'a' && lower <= 'z'
}

// taggedName carries the leading [GEO][IP] tags from the source name onto the
// relabeled node, falling back to raw when there are none.
func taggedName(n subscription.Node, raw, label, tags string) string {
	if tags == "" {
		return raw
	}
	if t, ok := relabelNode(n, tags+" "+label); ok {
		return t
	}
	return raw
}

const (
	decimalBase   = 10
	labelPadWidth = 3
)

// appendPad3 appends v as a decimal, zero-padded to a minimum width of
// labelPadWidth (matching the %03d format), without fmt allocations/boxing.
func appendPad3(b []byte, v int) []byte {
	digits := 1
	for n := v; n >= decimalBase; n /= decimalBase {
		digits++
	}
	for ; digits < labelPadWidth; digits++ {
		b = append(b, '0')
	}
	return strconv.AppendInt(b, int64(v), decimalBase)
}

// relabelNode rewrites a node's display name to label so probe results map
// back to entries. vmess and ssr both keep their name inside the base64
// payload; every other scheme uses a URI #fragment.
//
// For ssr the fragment path is not merely unused but corrupting: mihomo
// base64-decodes EVERYTHING after "ssr://", an appended "#label" included
// (convert/converter.go:476-479), so the relabeled link yields no proxy at
// all. The label then misses in the probe-result map, SelectSurvivors drops
// the entry, and the checker books server:port into the 2h dead cache, where
// Merge's first-wins dedupe lets it shadow a working node of another scheme.
//
// A payload neither rewriter can decode returns false, which drops the node:
// a node that cannot carry the label cannot be mapped back from a probe.
func relabelNode(n subscription.Node, label string) (string, bool) {
	switch n.Scheme { //nolint:exhaustive // ss and mierus name their node in the URI fragment, i.e. the generic path below
	case subscription.SchemeVmess:
		return subscription.RewriteVmessName(n.Raw, label)
	case subscription.SchemeSSR:
		return subscription.RewriteSSRName(n.Raw, label)
	}
	raw := n.Raw
	if n.FragmentIdx >= 0 {
		raw = raw[:n.FragmentIdx]
	}
	// Single allocation for the joined "<raw>#<label>" string; the byte buffer
	// is not retained after conversion, so the zero-copy view is safe.
	buf := make([]byte, 0, len(raw)+1+len(label))
	buf = append(buf, raw...)
	buf = append(buf, '#')
	buf = append(buf, label...)
	return ioutil.UnsafeString(buf), true
}
