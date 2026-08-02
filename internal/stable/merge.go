package stable

import (
	"net/netip"
	"strconv"
	"strings"
	"unicode/utf8"

	"domains.lst/sub-preprocessor/internal/ioutil"
	"domains.lst/sub-preprocessor/internal/preprocess"
	"domains.lst/sub-preprocessor/internal/subscription"
)

// SourceBody is one source's contribution to a cycle: the nodes its preprocess
// pass kept, still unannotated.
type SourceBody struct {
	Name  string
	Nodes []preprocess.NodeResult
}

// Entry is a merged node, still unannotated: the tags are built once at
// publication, after every probe and through-node filter has had its say. Raw
// carries the clean <source>-NNN name the prober converts and the publication
// annotates. IP is the address the preprocess IP-filters judged, carried
// forward so the tag can never describe a different address than the filter
// did — the worker never resolves a hostname twice. Country is filled at
// publication only, from the annotation's own verdict.
type Entry struct {
	Label   string
	Raw     string
	Addr    string // lowercased server:port, the dead-cache key
	IP      netip.Addr
	Country string
}

// Merge walks all sources in order, dedupes nodes by lowercased Server:Port
// (hostnames are case-insensitive; first source wins) and relabels each kept
// node to <source>-NNN so probe results map back to entries unambiguously. NNN
// counts kept nodes per source. Entry.Addr carries the lowercased key so
// mixed-case duplicates share one dead-cache entry; Raw keeps the original
// casing.
func Merge(bodies []SourceBody) []Entry {
	total := 0
	for _, src := range bodies {
		total += len(src.Nodes)
	}
	seen := make(map[string]struct{}, total)
	entries := make([]Entry, 0, total)
	var scratch []byte  // reused lowercased server:port key builder
	var labelBuf []byte // reused <source>-NNN label builder
	// Reused parse input. Safe because relabelNode always returns a freshly
	// built string, never a view into the line it was handed.
	var lineBuf []byte
	for _, src := range bodies {
		kept := 0
		for _, node := range src.Nodes {
			lineBuf = append(lineBuf[:0], node.Raw...)
			n, ok := parseOne(lineBuf)
			if !ok {
				continue
			}
			// Dedupe key: lowercased server:port in the reused scratch buffer.
			scratch = lowerServerPort(scratch, n.Server, n.Port)
			// Membership test on the scratch bytes allocates nothing; the real
			// string key is interned only when the node is actually kept.
			if _, dup := seen[string(scratch)]; dup {
				continue
			}
			labelBuf = labelBuf[:0]
			labelBuf = append(labelBuf, src.Name...)
			labelBuf = append(labelBuf, '-')
			labelBuf = appendPad3(labelBuf, kept+1)
			label := string(labelBuf)
			raw, relabeled := relabelNode(n, label)
			if !relabeled {
				continue
			}
			key := string(scratch)
			seen[key] = struct{}{}
			kept++
			entries = append(entries, Entry{Label: label, Raw: raw, Addr: key, IP: node.IP})
		}
	}
	return entries
}

// parseOne parses the one node line a NodeResult carries. preprocess yields
// exactly the lines it kept, one node each, so the first result is this node's
// and the walk stops there.
func parseOne(line []byte) (subscription.Node, bool) {
	var node subscription.Node
	ok := false
	subscription.Parse(line, func(n subscription.Node) bool {
		node, ok = n, true

		return false
	})

	return node, ok
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
