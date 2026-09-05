package crawl //nolint:testpackage // holds the unexported page scanners to their reference patterns

import (
	"html"
	"math/rand/v2"
	"regexp"
	"strings"
	"testing"
)

// The scanners in extract.go replaced these two patterns, so the patterns stay
// here as the specification they are held to: TestExtractorsMatchTheirRegexps
// runs both sides over the same corpus, and BenchmarkExtractURLs prices the
// difference in ONE binary rather than across two relinked trees. Editing a
// scanner without editing its pattern is what the test is for; editing both the
// same way is not, so a change to what may be harvested belongs in a fixture
// with a named case, not here.
var (
	urlReRef    = regexp.MustCompile(`https://[^\s"'<>\p{Z}]+`)
	inlineReRef = regexp.MustCompile(`\b(?:vless|vmess|ss|ssr|trojan|tuic|hysteria2|hysteria|hy2|anytls|mierus)://[^\s"'<>]+`)
)

// extractCorpusTokens are glued together at random to build pages. They are the
// boundaries the scanners can get wrong: every stop character of both classes,
// \v and NUL (which are NOT stops), unicode separators, invalid UTF-8, scheme
// substrings that must not match, and entity shapes html decodes by its own
// rules.
var extractCorpusTokens = []string{
	"https://", "http://", "HTTPS://", "https:/", "://",
	"ss://", "ssr://", "vless://", "vmess://", "hy2://", "hysteria://", "hysteria2://",
	"anytls://", "mierus://", "trojan://", "tuic://", "socks5://", "xss://", "_ss://", "2hy2://",
	"host.example", "/path", "?q=1", ":443", "#frag", "%20", "a", "Z", "9", "-", "_", "/",
	" ", "\t", "\n", "\r", "\f", "\v", "\x00", "\u00a0", "\u2028", "\u2029", "\u202f", "\u3000",
	"\"", "'", "<", ">", ".", ",", ";", ":", "!", "?", ")", "]", "}",
	"&amp;", "&AMP;", "&amp", "&ampx", "&lt;", "&gt;", "&quot;", "&apos;", "&nbsp;",
	"&#39;", "&#34;", "&#x41;", "&#38", "&#", "&#;", "&#xZZ;", "&notin;", "&not", "&", "&;",
	"&NotEqualTilde;", "&nGg;", "&#0;", "&#x110000;", "&#x85;", "&#128512;", "é", "\xff\xfe",
	"<pre>", "</pre>", "vless://uuid@192.0.2.1:443?type=tcp&amp;security=tls#n",
}

func randomExtractCorpus(t *testing.T) []string {
	t.Helper()
	// Fixed seed: a differential failure has to be reproducible from the test
	// name alone, and a fresh corpus every run would report a different one.
	rng := rand.New(rand.NewPCG(0x5eed, 0xc0ffee))
	const (
		pages     = 4000
		maxTokens = 24
	)
	out := make([]string, 0, pages)
	for range pages {
		var sb strings.Builder
		for range rng.IntN(maxTokens) + 1 {
			sb.WriteString(extractCorpusTokens[rng.IntN(len(extractCorpusTokens))])
		}
		out = append(out, sb.String())
	}
	return out
}

func extractCorpus(t *testing.T) []string {
	t.Helper()
	corpus := randomExtractCorpus(t)
	corpus = append(corpus, benchPages()...)
	corpus = append(corpus, benchInlinePages()...)
	return append(corpus, benchNoisePages()...)
}

func extractURLsRe(page string) []string {
	matches := urlReRef.FindAllString(page, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, strings.TrimRight(m, trimSet))
	}
	return out
}

func extractInlineNodesRe(page string) []string {
	matches := inlineReRef.FindAllString(page, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, strings.TrimRight(m, trimSet))
	}
	return out
}

func TestExtractorsMatchTheirRegexps(t *testing.T) {
	for _, page := range extractCorpus(t) {
		if got, want := extractURLs(page), extractURLsRe(page); !equalStrings(got, want) {
			t.Fatalf("extractURLs(%q) = %q, urlReRef gives %q", page, got, want)
		}
		if got, want := extractInlineNodes(page), extractInlineNodesRe(page); !equalStrings(got, want) {
			t.Fatalf("extractInlineNodes(%q) = %q, inlineReRef gives %q", page, got, want)
		}
		// appendInlineNodes runs its own copy of the extractInlineNodes walk
		// (PerfCrawl#1: one exact buffer instead of a []string round-trip), so
		// the corpus binds the two walks to the same node set.
		if got, want := appendInlineNodes(nil, page), extractInlineNodes(page); !equalStrings(got, want) {
			t.Fatalf("appendInlineNodes(%q) = %q, extractInlineNodes gives %q", page, got, want)
		}
	}
}

// TestUnescapeIntoMatchesHTML holds the buffered unescape to html.UnescapeString
// over the same corpus, which is the whole of its correctness: unescapeInto owns
// only where an entity ENDS, and hands every reference it does not spell out
// back to html.
func TestUnescapeIntoMatchesHTML(t *testing.T) {
	var buf []byte
	for _, page := range extractCorpus(t) {
		got, next := unescapeInto(buf, page)
		buf = next
		if want := html.UnescapeString(page); got != want {
			t.Fatalf("unescapeInto(%q) = %q, html.UnescapeString gives %q", page, got, want)
		}
	}
}

// TestUnescapeIntoReusesOneBuffer pins the reuse the harvest depends on: a
// second page must not grow the buffer a first page of the same size sized.
func TestUnescapeIntoReusesOneBuffer(t *testing.T) {
	first := strings.Repeat("a&amp;b", 1000)
	second := strings.Repeat("c&lt;d", 1000)
	_, buf := unescapeInto(nil, first)
	grown := cap(buf)
	text, buf := unescapeInto(buf, second)
	if cap(buf) != grown {
		t.Fatalf("second page grew the buffer %d -> %d", grown, cap(buf))
	}
	if want := html.UnescapeString(second); text != want {
		t.Fatalf("reused buffer gave %q, want %q", text, want)
	}
}

// TestAppendInlineNodesCopiesOutOfTheScratch: the accumulator outlives the
// scratch text the nodes were scanned from, so a node that still aliased it
// would read the NEXT page after harvestPages moved on.
func TestAppendInlineNodesCopiesOutOfTheScratch(t *testing.T) {
	const (
		node = "vless://uuid@192.0.2.1:443#n"
		page = "<pre>" + node + "</pre>"
	)
	text, buf := unescapeInto(nil, "&amp;"+page)
	got := appendInlineNodes(nil, text)
	if len(got) != 1 || got[0] != node {
		t.Fatalf("appendInlineNodes = %q, want [%q]", got, node)
	}
	// The second page must decode INTO the buffer the first page grew: if it
	// allocated a fresh buffer, the scratch an aliased node would read is
	// never written to and the check below could not fail. The page is kept
	// smaller than the first buffer's cap and the reuse is asserted by the
	// capacity staying put, exactly as TestUnescapeIntoReusesOneBuffer does.
	second := strings.Repeat("&amp;z", 4)
	if _, next := unescapeInto(buf, second); cap(next) != cap(buf) {
		t.Fatalf("second page grew the buffer %d -> %d; the fixture never reused it", cap(buf), cap(next))
	}
	if got[0] != node {
		t.Fatalf("node became %q after the buffer was reused, want %q", got[0], node)
	}
}

// TestNextMessageWalksDataPostBoundaries pins what counts as a boundary: the id
// shape is pageCursor's, and an attribute carrying none leaves its text with the
// message before it rather than opening one.
//
// The last two are the narrowing a numeric id buys. postID refuses a leading
// zero and a run too wide for uint64 rather than round either into an id, so
// the digit run stays injective onto its uint64: "chan/007" would otherwise
// mint the chan-7 that "chan/7" already owns, and a 25-digit run the name of
// whatever it wrapped to. Telegram emits neither; a hostile page can. The
// value-to-name mapping is deliberately not injective -- the channel post and
// forum rows below both pin 12 -- and mergeManaged's used set absorbs that.
func TestNextMessageWalksDataPostBoundaries(t *testing.T) {
	t.Parallel()
	const wide = "1234567890123456789012345"
	for _, tc := range []struct {
		name      string
		text      string
		seg, tail string
		id        uint64
	}{
		{"no attribute", "<div>a</div>", "<div>a</div>", "", 0},
		{"channel post", `a<div data-post="chan/12">b`, "a<div ", `chan/12">b`, 12},
		{"forum three segments", `a data-post="chat/7/12">b`, "a ", `chat/7/12">b`, 12},
		{"non-numeric tail", `a data-post="chan/x">b`, `a data-post="chan/x">b`, "", 0},
		{"no chat segment", `a data-post="/12">b`, `a data-post="/12">b`, "", 0},
		{"unterminated value", `a data-post="chan/12`, `a data-post="chan/12`, "", 0},
		{"past a bad attribute", `a data-post="chan/x" data-post="chan/9">b`, `a data-post="chan/x" `, `chan/9">b`, 9},
		{"leading zero", `a data-post="chan/007">b`, `a data-post="chan/007">b`, "", 0},
		{"signed", `a data-post="chan/+12">b`, `a data-post="chan/+12">b`, "", 0},
		{"wider than uint64", `a data-post="chan/` + wide + `">b`, `a data-post="chan/` + wide + `">b`, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			seg, id, tail := nextMessage(tc.text)
			if seg != tc.seg || id != tc.id || tail != tc.tail {
				t.Fatalf("nextMessage(%q) = %q %d %q, want %q %d %q",
					tc.text, seg, id, tail, tc.seg, tc.id, tc.tail)
			}
		})
	}
}

// TestHarvestAttributesThroughEscapedMarkup is the offset hazard: pageCursor
// reads the RAW page while the harvest scans the unescaped text, so an entity
// ahead of a boundary shifts every later offset, and an escaped attribute is a
// boundary in one string and not in the other. Attribution follows the scanner,
// which is why the last url here carries a post the cursor cannot see.
func TestHarvestAttributesThroughEscapedMarkup(t *testing.T) {
	t.Parallel()
	const (
		chrome = "https://sub.example/chrome"
		first  = "https://sub.example/first"
		second = "https://sub.example/second"
		third  = "https://sub.example/third"
	)
	page := `<a href="` + chrome + `">c</a><div>me &amp; you</div>` +
		`<div data-post="chan/100"><a href="` + first + `">a</a>` +
		`<a href="` + second + `">b</a></div>` +
		`<div data-post=&quot;chan/200&quot;><a href="` + third + `">d</a></div>`

	var inline []string
	cand := (&Crawler{}).harvestPages([]string{page}, &inline, nil, "chan")
	want := map[string]uint64{
		chrome: 0,
		first:  100,
		second: 100,
		third:  200,
	}
	if len(cand) != len(want) {
		t.Fatalf("harvested %v, want %d urls", cand, len(want))
	}
	for u, w := range want {
		if got, ok := cand[u]; !ok || got != w {
			t.Errorf("%s attributed to post %d (present %t), want %d", u, got, ok, w)
		}
	}
	if got := pageCursor(page); got != "100" {
		t.Fatalf("pageCursor = %q, want 100: the escaped boundary exists only in the unescaped text, so attribution computed on the raw page would misplace %s", got, third)
	}
}

// TestHarvestAttributesEachPageToItsOwnPost pins attribution across the page
// loop: every URL carries the id of the message it sits in, and a URL ahead of
// a page's first boundary carries none.
//
// It can no longer fail the way it was written to fail. origin.Post used to be
// a sub-slice of the one unescape scratch every page of a channel reuses, so
// page 0's id re-read as page 1's digits; a uint64 aliases nothing, and no
// fixture can reach a state the type forbids. Two things it does still catch,
// both checked by mutation: a segment attributed to the message it OPENS
// rather than the one it closes, and an id carried between pages over a shared
// buffer — this fixture on a tree whose Post is a sub-slice again reports 999
// for page 0. The equal lengths are what make that second one bite, so they
// are asserted rather than assumed.
//
// It does not catch an origin merely hoisted out of harvestPages' loop:
// harvestPage's walk ends every page on a boundary-less tail, which zeroes the
// post before the next page starts.
func TestHarvestAttributesEachPageToItsOwnPost(t *testing.T) {
	t.Parallel()
	page := func(id, path string) string {
		return `<a href="https://sub.example/` + path + `-chrome">c</a>` +
			`<div data-post="chan/` + id + `">me &amp; you` +
			`<a href="https://sub.example/` + path + `">a</a></div>`
	}
	pages := []string{page("100", "aaaa"), page("999", "bbbb")}
	if len(pages[0]) != len(pages[1]) {
		t.Fatalf("fixture pages are %d and %d bytes: page 1 only lands on page 0's digits when they match",
			len(pages[0]), len(pages[1]))
	}

	var inline []string
	cand := (&Crawler{}).harvestPages(pages, &inline, nil, "chan")
	want := map[string]uint64{
		"https://sub.example/aaaa-chrome": 0,
		"https://sub.example/aaaa":        100,
		"https://sub.example/bbbb-chrome": 0,
		"https://sub.example/bbbb":        999,
	}
	if len(cand) != len(want) {
		t.Fatalf("harvested %v, want %d urls", cand, len(want))
	}
	for u, w := range want {
		if got, ok := cand[u]; !ok || got != w {
			t.Errorf("%s attributed to post %d (present %t), want %d", u, got, ok, w)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// BenchmarkExtractURLs prices the scan against the pattern it replaced on one
// benchPages page: same input, same binary, one delta.
func BenchmarkExtractURLs(b *testing.B) {
	page := benchPages()[0]
	for _, tc := range []struct {
		name    string
		extract func(string) []string
	}{
		{"scan", extractURLs},
		{"regexp", extractURLsRe},
	} {
		if got := tc.extract(page); len(got) != benchLinkRepeats {
			b.Fatalf("%s yields %d urls, want benchLinkRepeats = %d", tc.name, len(got), benchLinkRepeats)
		}
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				benchURLSink = tc.extract(page)
			}
		})
	}
}

// BenchmarkExtractInlineNodes is BenchmarkExtractURLs for the node scan, on the
// entity-carrying inline page, already unescaped as the harvest hands it over.
func BenchmarkExtractInlineNodes(b *testing.B) {
	page := html.UnescapeString(benchInlinePages()[0])
	for _, tc := range []struct {
		name    string
		extract func(string) []string
	}{
		{"scan", extractInlineNodes},
		{"regexp", extractInlineNodesRe},
	} {
		if got := tc.extract(page); len(got) != benchInlineNodes {
			b.Fatalf("%s yields %d nodes, want benchInlineNodes = %d", tc.name, len(got), benchInlineNodes)
		}
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				benchURLSink = tc.extract(page)
			}
		})
	}
}

// BenchmarkUnescapePage prices one 52 KiB page carrying benchInlineNodes
// entities: buffered against html.UnescapeString, whose two copies of the page
// are what a harvest paid per page.
func BenchmarkUnescapePage(b *testing.B) {
	page := benchInlinePages()[0]
	if !strings.Contains(page, "&") {
		b.Fatal("fixture carries no entity, so neither side copies anything")
	}
	b.Run("buffered", func(b *testing.B) {
		var buf []byte
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			benchTextSink, buf = unescapeInto(buf, page)
		}
	})
	b.Run("html", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			benchTextSink = html.UnescapeString(page)
		}
	})
}
