package subscription

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// escapeCorpus is what a display name and a base64 query value can carry: the
// reserved bytes each escaping mode treats differently, a space, non-ASCII and
// the tag shape the annotator writes.
var escapeCorpus = []string{
	"", "plain", "Tokyo Node", "[GEO:FI][IP:192.0.2.1] mifa-001",
	"a+b/c=d", "?query#frag", "a;b,c/d", "$&+,/:;=?@", "~-._",
	"Ünïtéd ÿÿÿ", "emoji 🇩🇪", "100%", "a\x00b", "\t\n\r",
	"obfs.example.com", "auth-token", "UO3EObgU3xUrhIGEE0gfCn5ZOz8YxNcwwW6ZaYzD3SA",
}

// TestEscapersMatchNetURL is the equivalence proof for the append escapers that
// replaced url.QueryEscape and url.PathEscape on the share-link paths: a byte
// either escaper spelled differently would publish a link mihomo reads as a
// different node.
func TestEscapersMatchNetURL(t *testing.T) {
	t.Parallel()

	cases := make([]string, 0, len(escapeCorpus)+256)
	cases = append(cases, escapeCorpus...)
	for b := range 256 {
		cases = append(cases, string([]byte{byte(b)}))
	}

	for _, s := range cases {
		if got, want := string(appendQueryEscape(nil, s)), url.QueryEscape(s); got != want {
			t.Errorf("appendQueryEscape(%q) = %q, url.QueryEscape = %q", s, got, want)
		}
		if got, want := string(appendPathEscape(nil, s)), url.PathEscape(s); got != want {
			t.Errorf("appendPathEscape(%q) = %q, url.PathEscape = %q", s, got, want)
		}
	}
}

func TestAppendEscapersKeepPrefix(t *testing.T) {
	t.Parallel()

	const prefix = "vless://id@host:443?"
	if got := string(appendQueryEscape([]byte(prefix), "a b")); got != prefix+"a+b" {
		t.Errorf("appendQueryEscape overwrote its destination: %q", got)
	}
	if got := string(appendPathEscape([]byte(prefix), "a b")); got != prefix+"a%20b" {
		t.Errorf("appendPathEscape overwrote its destination: %q", got)
	}
}

// queryCorpus is every shape an ssr query has to agree with url.ParseQuery on,
// including the two it must refuse: a ';' separator and a broken escape.
var queryCorpus = []string{
	"",
	"remarks=VG9reW8",
	"obfsparam=b2Jmcw&protoparam=YXV0aA&remarks=VG9reW8&group=Z3Jw&udpport=0&uot=1",
	"remarks=",
	"remarks",
	"=value",
	"&&remarks=x&&",
	"remarks=a&remarks=b&remarks=c",
	"remarks=a+b",
	"remarks=a%20b",
	"remarks=%zz",
	"%zz=x",
	"remarks=x;group=y",
	"a=1&b=2&c=3&d=4&e=5&f=6&g=7&h=8&i=9&j=10&k=11",
	"z=last&a=first",
	"key=val=ue",
	"remarks=%F0%9F%87%A9%F0%9F%87%AA",
}

// TestQueryListMatchesURLValues pins the parse/set/encode round trip against
// net/url for both the accept set and the emitted bytes: the ssr rewriter's
// output has to stay a link mihomo's own url.ParseQuery reads back.
func TestQueryListMatchesURLValues(t *testing.T) {
	t.Parallel()

	for _, raw := range queryCorpus {
		var scratch [ssrQueryHint]queryPair
		list, gotOK := queryList(scratch[:0]).parse(raw)

		values, err := url.ParseQuery(raw)
		if wantOK := err == nil; gotOK != wantOK {
			t.Errorf("%q: parse ok = %v, url.ParseQuery err = %v", raw, gotOK, err)
			continue
		}
		if !gotOK {
			continue
		}

		if got, want := string(list.appendEncoded(nil)), values.Encode(); got != want {
			t.Errorf("%q: encoded %q, url.Values.Encode %q", raw, got, want)
		}
		if got, want := list.get("remarks"), values.Get("remarks"); got != want {
			t.Errorf("%q: get(remarks) = %q, url.Values.Get = %q", raw, got, want)
		}

		const newName = "[GEO:JP] Tokyo Node"
		list = list.set("remarks", newName)
		values.Set("remarks", newName)
		if got, want := string(list.appendEncoded(nil)), values.Encode(); got != want {
			t.Errorf("%q: after set encoded %q, url.Values.Encode %q", raw, got, want)
		}
	}
}

// TestQueryListAllocatesNothing pins the reason the type exists: the ssr parse
// path holds its query in a caller-owned array, so no node pays a map.
func TestQueryListAllocatesNothing(t *testing.T) {
	const raw = "obfsparam=b2Jmcw&protoparam=YXV0aA&remarks=VG9reW8&group=Z3Jw&udpport=0&uot=1"

	dst := make([]byte, 0, 256)
	if allocs := testing.AllocsPerRun(100, func() {
		var scratch [ssrQueryHint]queryPair
		list, ok := queryList(scratch[:0]).parse(raw)
		if !ok || list.get("remarks") == "" {
			t.Fatal("fixture must parse")
		}
		dst = list.appendEncoded(dst[:0])
	}); allocs != 0 {
		t.Fatalf("query round trip allocated %.0f times per call, want 0", allocs)
	}
	if !strings.Contains(string(dst), "remarks=VG9reW8") {
		t.Fatalf("encoded query lost its remarks: %q", dst)
	}
}

// tenKeyDescending is queryHint keys in the order that costs a sort the most.
var tenKeyDescending = [queryHint]string{"j", "i", "h", "g", "f", "e", "d", "c", "b", "a"}

// TestQueryListEncodeAllocatesNothingAtTenKeys is the xray builders' width: the
// sort appendEncoded runs must stay scratch-free at the width they build.
func TestQueryListEncodeAllocatesNothingAtTenKeys(t *testing.T) {
	dst := make([]byte, 0, 256)
	if allocs := testing.AllocsPerRun(100, func() {
		var scratch [queryHint]queryPair
		list := queryList(scratch[:0])
		for _, key := range tenKeyDescending {
			list = list.add(key, "v")
		}
		dst = list.appendEncoded(dst[:0])
	}); allocs != 0 {
		t.Fatalf("ten-key encode allocated %.0f times per call, want 0", allocs)
	}
	if got, want := string(dst), "a=v&b=v&c=v&d=v&e=v&f=v&g=v&h=v&i=v&j=v"; got != want {
		t.Fatalf("ten-key encode = %q, want %q", got, want)
	}
}

// segmentsOf builds a raw query of n &-separated segments, each seg.
func segmentsOf(seg string, n int) string {
	if n == 0 {
		return ""
	}

	return strings.Repeat(seg+"&", n-1) + seg
}

// descendingKeys is n distinct fixed-width keys in reverse order, the worst
// input order a sort can be handed.
func descendingKeys(n int) string {
	var sb strings.Builder
	sb.Grow(n * len("k1000000=v&"))
	for i := n - 1; i >= 0; i-- {
		if sb.Len() > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString("k")
		sb.WriteString(strconv.Itoa(1_000_000 + i))
		sb.WriteString("=v")
	}

	return sb.String()
}

// TestQueryListMatchesURLValuesAtParamCeiling straddles the segment ceiling
// net/url refuses a query at. decodeSSR's accept set is that parser's because
// mihomo's converter runs it, so a payload we take past the ceiling is a node
// that probes and can never publish.
func TestQueryListMatchesURLValuesAtParamCeiling(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  string
	}{
		{"9999 repeated", segmentsOf("k=v", maxQueryParams-1)},
		{"10000 repeated", segmentsOf("k=v", maxQueryParams)},
		{"10001 repeated", segmentsOf("k=v", maxQueryParams+1)},
		{"10000 empty", strings.Repeat("&", maxQueryParams-1)},
		{"10001 empty", strings.Repeat("&", maxQueryParams)},
		{"10000 distinct descending", descendingKeys(maxQueryParams)},
		{"10001 distinct descending", descendingKeys(maxQueryParams + 1)},
	}
	for _, tc := range cases {
		list, gotOK := queryList(nil).parse(tc.raw)

		values, err := url.ParseQuery(tc.raw)
		if wantOK := err == nil; gotOK != wantOK {
			t.Errorf("%s: parse ok = %v, url.ParseQuery err = %v", tc.name, gotOK, err)
			continue
		}
		if !gotOK {
			continue
		}
		if got, want := string(list.appendEncoded(nil)), values.Encode(); got != want {
			t.Errorf("%s: encoded %d bytes, url.Values.Encode %d, first difference at %d",
				tc.name, len(got), len(want), firstDiff(got, want))
		}
	}
}

func firstDiff(a, b string) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}

	return min(len(a), len(b))
}

// TestQueryListEncodeIsStableAtScale pins the ordering repeated keys get from a
// query too wide for the sort to be stable by accident: url.Values holds them
// in one []string in arrival order, and the ssr rewriter has to emit that.
func TestQueryListEncodeIsStableAtScale(t *testing.T) {
	t.Parallel()

	const keys, repeats = 100, 100
	var sb strings.Builder
	for i := range keys * repeats {
		if i > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString("k")
		sb.WriteString(strconv.Itoa(100 + i%keys))
		sb.WriteString("=v")
		sb.WriteString(strconv.Itoa(i))
	}
	raw := sb.String()

	list, ok := queryList(nil).parse(raw)
	if !ok {
		t.Fatal("fixture must parse")
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("fixture must parse under net/url: %v", err)
	}
	if got, want := string(list.appendEncoded(nil)), values.Encode(); got != want {
		t.Errorf("encoded %d bytes, url.Values.Encode %d, first difference at %d",
			len(got), len(want), firstDiff(got, want))
	}
}

// TestQueryListSortIsNotQuadratic bounds the cost of the one caller whose width
// a source picks: RewriteSSRName sorts whatever the payload carried. Ratios
// rather than wall clock, so the bound holds on any machine — measured
// 2026-08-18, 8x the pairs is 8.5x the work here and 64x for the insertion sort
// this replaced.
func TestQueryListSortIsNotQuadratic(t *testing.T) {
	const small, large = 5_000, 40_000
	const budget = 30.0

	fast, slow := sortTime(t, small), sortTime(t, large)
	if ratio := float64(slow) / float64(fast); ratio > budget {
		t.Fatalf("sorting %d pairs took %v, %.0fx the %v of %d pairs, over the %.0fx budget: the sort is not n log n",
			large, slow, ratio, fast, small, budget)
	}
}

// sortTime is the shortest of five sorts of pairs descending keys; the shortest
// because a scheduler can only ever add time to one.
func sortTime(t *testing.T, pairs int) time.Duration {
	t.Helper()

	best := time.Duration(0)
	for range 5 {
		q := make(queryList, 0, pairs)
		for i := pairs - 1; i >= 0; i-- {
			q = q.add("k"+strconv.Itoa(1_000_000+i), "v")
		}
		start := time.Now()
		q.sortByKey()
		elapsed := time.Since(start)
		if q[0].key != "k1000000" {
			t.Fatalf("%d pairs: sort left %q first", pairs, q[0].key)
		}
		if best == 0 || elapsed < best {
			best = elapsed
		}
	}

	return best
}
