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
	// Label is a view into Merge's keyArena on the payload-rewriter path (vmess,
	// ssr), an owned string on the fragment path; Addr is always a view. The
	// bytes stay valid either way, but a retained view pins its whole 1 KiB
	// block, so anything outliving the cycle — a DeadCache above all — must copy
	// what it keeps.
	Label   string
	Raw     string
	Addr    string
	IP      netip.Addr
	Country string
}

// Merge walks all sources in order, dedupes nodes by lowercased Server:Port
// (hostnames are case-insensitive; first source wins) and relabels each kept
// node to <source>-NNN so probe results map back to entries unambiguously. NNN
// counts kept nodes per source. Entry.Addr carries the lowercased key so
// mixed-case duplicates share one dead-cache entry; Raw keeps the original
// casing.
//
// A placeholder node is dropped here rather than given a probe slot: a Nil-UUID
// credential authenticates nobody, and a server that names the dialling machine
// reaches no remote however good its credential. The second is worse than a
// dial failure — such a node on this service's own listener port passes the TCP
// precheck and buys a URLTest round against the worker itself. The pool does
// carry them: fetching the 98 configured source URLs with the worker's UA on
// 2026-08-14 yielded 100 nodes on a local address, 24 of them loopback and 4 of
// those on the shipped listener's own port. Dropping before the dedupe key is
// interned leaves a real node on the same server:port still admittable.
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
	var keys keyArena
	for _, src := range bodies {
		kept := 0
		for _, node := range src.Nodes {
			lineBuf = append(lineBuf[:0], node.Raw...)
			n, ok := parseOne(lineBuf)
			if !ok {
				continue
			}
			if subscription.PlaceholderNode(n.Raw, n.Server) {
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
			raw, label, relabeled := relabelNode(n, labelBuf, &keys)
			if !relabeled {
				continue
			}
			// One arena view serves both the dedupe key and Entry.Addr; scratch
			// is about to be overwritten by the next node.
			key := keys.intern(scratch)
			seen[key] = struct{}{}
			kept++
			entries = append(entries, Entry{Label: label, Raw: raw, Addr: key, IP: node.IP})
		}
	}
	return entries
}

// keyArena cuts each kept node's dedupe key, and the label a vmess or ssr node
// cannot view out of its own line, from a shared block instead of allocating
// each one: 574 fewer allocations on BenchmarkMerge, measured 2026-08-18.
//
// A full block is retired, never grown, so append can never move bytes an
// outstanding view points at. A retained view pins its whole block, which is
// why DeadCache.Block must copy the key it keeps.
type keyArena struct{ buf []byte }

// keyArenaBlock bounds both sides of the trade: one partly-filled block of
// waste per Merge against one allocation per 1KiB interned rather than one per
// string — ~745 blocks for the keys alone at production's 36342 merged nodes
// (2026-08-15, ~21 B a key), with the labels riding in the same blocks.
// Packing also beats the size class a short key rounds up to, so B/op falls: on
// BenchmarkMerge, medians of -count=5 on 2026-08-18, 203831 B/1338 allocs with
// no arena at all and 202575 B/764 here, against 209745 B/757 at 8KiB where the
// waste has outgrown what it bought.
const keyArenaBlock = 1 << 10

func (a *keyArena) intern(key []byte) string {
	if len(key) > cap(a.buf)-len(a.buf) {
		a.buf = make([]byte, 0, max(keyArenaBlock, len(key)))
	}
	start := len(a.buf)
	a.buf = append(a.buf, key...)

	return ioutil.UnsafeString(a.buf[start:])
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
// the entry, and the checker books server:port into the dead cache
// (deadcache.ttl, 3h shipped, jittered to [ttl, 1.5*ttl)), where Merge's
// first-wins dedupe lets it shadow a working node of another scheme.
//
// A payload neither rewriter can decode returns false, which drops the node:
// a node that cannot carry the label cannot be mapped back from a probe.
//
// The returned label is the string the Entry keeps: on the fragment path it is
// a view into the relabeled line's tail, so a kept node pays nothing for it.
// The payload rewriters re-encode the name and leave nothing to view, so their
// label is cut from the keyArena instead: neither rewriter retains what it is
// handed, and a kept Entry already pins a block of that arena through Addr.
// The allocation it saves costs time, so it is a trade: two interleaved
// -count=5 series, 2026-08-18, put BenchmarkMerge at 837 -> 764 allocs/op for
// +1.6% and +2.0% ns/op (209470 here) and BenchmarkMergeSSR at 997 -> 924 for
// +2.5% and +3.1% (151203). Merge runs once per cycle, so the allocations win.
func relabelNode(n subscription.Node, label []byte, keys *keyArena) (raw, lbl string, ok bool) {
	switch n.Scheme { //nolint:exhaustive // ss and mierus name their node in the URI fragment, i.e. the generic path below
	case subscription.SchemeVmess:
		lbl = keys.intern(label)
		raw, ok = subscription.RewriteVmessName(n.Raw, lbl)

		return raw, lbl, ok
	case subscription.SchemeSSR:
		lbl = keys.intern(label)
		raw, ok = subscription.RewriteSSRName(n.Raw, lbl)

		return raw, lbl, ok
	}
	uri := n.Raw
	if n.FragmentIdx >= 0 {
		uri = uri[:n.FragmentIdx]
	}
	// Single allocation for the joined "<uri>#<label>" string; the byte buffer
	// is not retained after conversion, so the zero-copy view is safe.
	buf := make([]byte, 0, len(uri)+1+len(label))
	buf = append(buf, uri...)
	buf = append(buf, '#')
	buf = append(buf, label...)
	raw = ioutil.UnsafeString(buf)

	return raw, raw[len(raw)-len(label):], true
}
