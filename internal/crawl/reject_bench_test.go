package crawl //nolint:testpackage // benchmarks unexported crawl internals (candidate, harvestPages)

import (
	"errors"
	"fmt"
	"html"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/fetch"
)

// This file is the package's first benchmark, and it exists because of a
// process failure rather than a performance question. Three review rounds on
// the reject-log branch litigated the same allocation figures — the candidate
// accept path, the cost of its eager fmt.Errorf wraps, and the blind-vs-guarded
// clone in harvestPages — and every one of those figures came from a throwaway
// harness that was deleted after it ran, described in prose in a commit
// message. A message cannot be corrected without a rebase and cannot be
// re-derived from a checkout at all, so each round re-measured from scratch,
// got a slightly different number, and overturned its predecessor.
//
// The variants those harnesses built by hand are committed here instead:
// candidateUnwrapped is the alternative the eager-wrap finding proposed,
// candidatePreReason is 26c8fe2's candidate body verbatim, and harvestPagesBlind
// is the unguarded clone. Each pair compiles into THIS binary, so a delta is a
// difference between two benchmarks in one run rather than a comparison across
// relinked trees. Comments that cite a figure name the benchmark that produces
// it; nothing here needs a recipe written in prose.

// Sinks keep the compiler from eliding benchmarked work.
var (
	benchOKSink     bool
	benchReasonSink rejectReason
	errBenchSink    error
	benchCandSink   map[string]uint64
	benchURLSink    []string
	benchTextSink   string
)

const (
	// benchLinkBytes is the length of every synthetic subscription link below.
	// It is load-bearing for BenchmarkHarvestPages: the clone the harvest pays
	// is one allocation of the link's own length, so its B/op moves with this
	// constant and with nothing else in the page.
	benchLinkBytes = 64
	// benchPageBytes is one t.me/s page. The scraped cap is maxPageBytes
	// (8 MiB); a real page is this order, and page size matters here because
	// extractURLs scans the whole page and hands back sub-slices of it.
	benchPageBytes = 52 << 10
	// benchPageCount is one harvestPages call's worth of pages: main.go's
	// defaultCrawlPages, the full budget pagesFor gives a seed channel
	// (discovered channels get discoveredPages, 3).
	benchPageCount = 6
	// benchLinkRepeats is how many times one page reposts the same link. The
	// dedupe guard's whole subject: a blind clone copies per OCCURRENCE and a
	// guarded one per DISTINCT url, so one page pays this many copies against
	// one, and a call over benchPageCount pages differs by
	// benchPageCount*(benchLinkRepeats-1) clones. That is arithmetic; the
	// benchmark only prices it.
	benchLinkRepeats = 20
	// benchFirstPostID is the first id benchSegmentedPages stamps. Four digits
	// keeps every message one length, so a page's size stays the fixture's
	// arithmetic rather than the id's decimal width.
	benchFirstPostID = 3000
	// benchChannel is the harvest's channel argument AND the chat part of every
	// data-post value, so a boundary in a fixture is one this channel published.
	benchChannel = "benchchannel"
	// benchInlineNodes is how many pasted proxy URIs one inline fixture page
	// carries. Real pages run from zero to a few hundred (measured: 203 on the
	// densest of 24 channels' first pages, 0 on nine of them); this sits in the
	// working middle, where the regex scan and the match slice both count.
	benchInlineNodes = 20

	// benchPrivateIPLink takes candidate's validate gate:
	// ValidatePublicParsedHTTPSURL refuses a non-public target with a static
	// string, so the returned error is wrapped by the second fmt.Errorf and is
	// NOT a *url.Error. benchCandidateCase.check asserts that gate; this comment no
	// longer has to be believed.
	benchPrivateIPLink = "https://10.0.0.1/sub"
	// benchUnparseableLink takes candidate's parse gate: a DEL byte is a control
	// character, so url.Parse fails and hands back a *url.Error, which %w then
	// re-renders whole. Asserted by benchCandidateCase.check, not claimed.
	benchUnparseableLink = "https://ex\x7fample.com/sub"
)

// benchLink returns a distinct benchLinkBytes-long subscription link that
// passes every candidate gate.
func benchLink(i int) string {
	return "https://sub.example.com/api/v1/client/subscribe/" + fmt.Sprintf("%016x", i)
}

// benchNoiseLink returns a distinct benchLinkBytes-long t.me link, which
// candidate turns down at its noise-host gate with no error to wrap.
func benchNoiseLink(i int) string {
	return "https://t.me/somechannel/repost/archive/message/" + fmt.Sprintf("%016x", i)
}

// benchPages builds benchPageCount pages of exactly benchPageBytes, each
// carrying ONE distinct link reposted benchLinkRepeats times among filler that
// contains no URL and no HTML entity. The entity matters more than it looks: the
// unescape copies only when it finds an '&', so this fixture prices the scan with
// the copy designed out of it — BenchmarkUnescapePage prices the copy itself, and
// benchInlinePages is what pays for one.
func benchPages() []string {
	pages := make([]string, benchPageCount)
	for i := range pages {
		link := benchLink(i)
		var sb strings.Builder
		sb.Grow(benchPageBytes)
		filler := strings.Repeat("x", (benchPageBytes-benchLinkRepeats*(len(link)+2))/benchLinkRepeats)
		for range benchLinkRepeats {
			sb.WriteString(filler)
			sb.WriteByte(' ')
			sb.WriteString(link)
			sb.WriteByte(' ')
		}
		sb.WriteString(strings.Repeat("x", benchPageBytes-sb.Len()))
		pages[i] = sb.String()
	}
	return pages
}

// benchDistinctPages is benchPages with the repost factor as a parameter: every
// page still holds benchLinkRepeats occurrences of a benchLinkBytes link in
// benchPageBytes, but they are benchLinkRepeats/repeats distinct links repeated
// `repeats` times each, and no link is shared between pages. So repeats =
// benchLinkRepeats reproduces benchPages exactly and repeats = 1 gives the
// widest distinct set the same bytes can carry.
func benchDistinctPages(repeats int) []string {
	perPage := benchLinkRepeats / repeats
	pages := make([]string, benchPageCount)
	filler := strings.Repeat("x", (benchPageBytes-benchLinkRepeats*(benchLinkBytes+2))/benchLinkRepeats)
	for i := range pages {
		var sb strings.Builder
		sb.Grow(benchPageBytes)
		for j := range benchLinkRepeats {
			sb.WriteString(filler)
			sb.WriteByte(' ')
			sb.WriteString(benchLink(i*perPage + j/repeats))
			sb.WriteByte(' ')
		}
		sb.WriteString(strings.Repeat("x", benchPageBytes-sb.Len()))
		pages[i] = sb.String()
	}
	return pages
}

// benchSegmentedPages is benchPages split into benchLinkRepeats data-post
// messages of one link each — same bytes, same pages, same repeats. No other
// fixture holds a data-post, so without this one harvestPage's boundary walk is
// unpriced and agreement item 14 binds nothing.
func benchSegmentedPages() []string {
	pages := make([]string, benchPageCount)
	for i := range pages {
		link := benchLink(i)
		var sb strings.Builder
		sb.Grow(benchPageBytes)
		const msgClose = "</div>"
		per := len(benchMessageOpen(benchFirstPostID)) + len(msgClose) + len(link) + 2
		filler := strings.Repeat("x", (benchPageBytes-benchLinkRepeats*per)/benchLinkRepeats)
		for j := range benchLinkRepeats {
			sb.WriteString(benchMessageOpen(benchFirstPostID + j))
			sb.WriteString(filler)
			sb.WriteByte(' ')
			sb.WriteString(link)
			sb.WriteByte(' ')
			sb.WriteString(msgClose)
		}
		sb.WriteString(strings.Repeat("x", benchPageBytes-sb.Len()))
		pages[i] = sb.String()
	}
	return pages
}

func benchMessageOpen(post int) string {
	return `<div data-post="` + benchChannel + "/" + strconv.Itoa(post) + `">`
}

// benchNoisePages mirrors benchPages with a link that fails candidate's
// noise-host gate, which is the repost shape a real page carries most of.
func benchNoisePages() []string {
	pages := make([]string, benchPageCount)
	for i := range pages {
		link := benchNoiseLink(i)
		var sb strings.Builder
		sb.Grow(benchPageBytes)
		filler := strings.Repeat("x", (benchPageBytes-benchLinkRepeats*(len(link)+2))/benchLinkRepeats)
		for range benchLinkRepeats {
			sb.WriteString(filler)
			sb.WriteByte(' ')
			sb.WriteString(link)
			sb.WriteByte(' ')
		}
		sb.WriteString(strings.Repeat("x", benchPageBytes-sb.Len()))
		pages[i] = sb.String()
	}
	return pages
}

// benchInlinePages mirrors benchPages for the inline harvest, which is the
// other half of what a scraped page costs and the half no fixture priced until
// the harvest was restricted to page 1: with the scan now off pages 2..N, a
// regression in it moves nothing unless something benchmarks it. Entities are
// deliberate — they are what makes the unescape copy a page. harvestPages fills
// one scratch per call and the node scan runs on page 1 alone, so this fixture
// prices six unescapes into one buffer plus one node scan.
func benchInlinePages() []string {
	pages := make([]string, benchPageCount)
	for i := range pages {
		var sb strings.Builder
		sb.Grow(benchPageBytes)
		for j := range benchInlineNodes {
			fmt.Fprintf(&sb, "<pre>vless://%016x@10.0.%d.%d:443?type=tcp&amp;security=tls#n</pre> ", i, i, j)
		}
		sb.WriteString(strings.Repeat("x", benchPageBytes-sb.Len()))
		pages[i] = sb.String()
	}
	return pages
}

// candidateUnwrapped is candidate returning the bare external error instead of
// wrapping it. It is the alternative the eager-fmt.Errorf finding proposed, and
// the only reason the branch declined it is the difference between
// BenchmarkCandidate and BenchmarkCandidateUnwrapped on the invalid-url cases —
// against a wrapcheck suppression on a bare fetch/net-url error. Both wraps are
// on the invalid-url path only, and the wrap's cost is not one number: it scales
// with the text of the message %w re-renders. It tracks candidate's other
// changes — the single parse below is candidate's — so the pair keeps differing
// in the wraps alone; candidatePreReason is the frozen body, not this one.
func candidateUnwrapped(raw string) (bool, rejectReason, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return false, rejectInvalidURL, err
	}
	if isNoiseHost(u.Hostname()) {
		return false, rejectNoiseHost, nil
	}
	if err = fetch.ValidatePublicParsedHTTPSURL(u); err != nil {
		return false, rejectInvalidURL, err
	}
	return true, "", nil
}

// candidatePreReason is 26c8fe2's candidate, before the branch gave a false
// verdict its reason (git show 26c8fe2:internal/crawl/crawl.go). Its accept path
// is the baseline for BenchmarkCandidate/accept: same gates, same order, one
// bool out.
func candidatePreReason(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || isNoiseHost(u.Hostname()) {
		return false
	}
	return fetch.ValidatePublicHTTPSURL(fetch.SubscriptionURL(raw)) == nil
}

// harvestPagesBlind is the shipped harvest with the dedupe dropped: it asks
// candidate and clones on every occurrence instead of every distinct URL. The
// divergences below are the WHOLE list, enumerated because "differs in that one
// branch" is what let a fourth drift in unnoticed for a wave:
//   - no dedupe — neither cand's distinct-URL set nor harvestPage's memo of the
//     reject a post repeats — which is the branch this pair prices;
//   - no boundary walk: one origin and one whole-page scan, so every value is
//     post 0, because segmentation is the segmented arm's subject, not this one's;
//   - the receiver is a parameter, since this is not a method the package ships.
//
// Everything else mirrors harvestPages over harvestPage: one scratch across
// pages, one URL slice sized off the page and reused across pages
// (discover.go:317-318, 325), the accepted key cloned, the inline scan on page 0
// alone.
func harvestPagesBlind(c *Crawler, pages []string, inline *[]string, rej *rejects, channel string) map[string]uint64 {
	cand := map[string]uint64{}
	var (
		scratch []byte
		urls    []string
	)
	for i, p := range pages {
		text, buf := unescapeInto(scratch, p)
		scratch = buf
		o := origin{Slug: channel}
		if n := strings.Count(text, urlScheme); cap(urls) < n {
			urls = make([]string, 0, n)
		}
		urls = appendURLs(urls[:0], text)
		for _, raw := range urls {
			ok, reason, err := candidate(raw)
			if !ok {
				rej.record(o, raw, reason, 0, err)
				continue
			}
			cand[strings.Clone(raw)] = o.Post
		}
		if i == 0 && c.opts.InlineEnabled && len(*inline) < maxInlineAccum {
			*inline = appendInlineNodes(*inline, text)
			if len(*inline) > maxInlineAccum {
				*inline = (*inline)[:maxInlineAccum]
			}
		}
	}
	return cand
}

// benchCandidateCase is one candidate fixture together with the outcome AND the
// gate its name claims. Both are asserted before the timer starts, because a
// drifted fixture does not fail — it silently reprices a different branch under
// the old name. Making benchUnparseableLink parseable moves invalid-url/parse
// onto the accept path in BOTH benchmarks, so the wrap delta candidate's comment
// cites as "what they cost" reads zero while neither side ever enters a wrapped
// branch; pointing benchLink at a host isNoiseHost matches drops accept to the
// noise-host branch, so the allocation-identity against
// BenchmarkCandidatePreReason keeps reading true off a path neither variant was
// meant to price. Prose cannot catch either one; this is the class of evidence
// the file was written to replace.
type benchCandidateCase struct {
	name       string
	link       string
	wantOK     bool
	wantReason rejectReason
	// wantParse is whether the link must fail at candidate's url.Parse gate
	// rather than at its ValidatePublicParsedHTTPSURL one. wantReason cannot tell
	// the two apart — both report rejectInvalidURL — and a *url.Error is what
	// does: url.Parse always returns one, while the validator's returns are all
	// static strings, since the parsed entry point has no parse of its own.
	wantParse bool
}

// check fails b unless fn(tc.link) takes the gate tc.name claims.
func (tc benchCandidateCase) check(b *testing.B, name string, fn func(string) (bool, rejectReason, error)) {
	b.Helper()
	ok, reason, err := fn(tc.link)
	if ok != tc.wantOK || reason != tc.wantReason {
		b.Fatalf("%s(%q) = %v %q, want %v %q", name, tc.link, ok, reason, tc.wantOK, tc.wantReason)
	}
	var parseErr *url.Error
	if gotParse := errors.As(err, &parseErr); gotParse != tc.wantParse {
		b.Fatalf("%s(%q) err = %v, is *url.Error = %v, want %v: fixture no longer takes the gate %q names",
			name, tc.link, err, gotParse, tc.wantParse, tc.name)
	}
}

// BenchmarkCandidate prices the shipped gate on its accept path and both of its
// invalid-url gates. That is three CASES over two of candidate's three outcomes:
// the noise-host branch is deliberately not priced, because candidate and
// candidateUnwrapped return the identical (false, rejectNoiseHost, nil) there —
// no wrap on either side, so the delta this file exists to measure is zero by
// construction. The accept case is what a cycle pays per harvested link; both
// invalid-url cases carry an eager fmt.Errorf wrap, and their delta against
// BenchmarkCandidateUnwrapped is that wrap's whole cost.
func BenchmarkCandidate(b *testing.B) {
	for _, tc := range []benchCandidateCase{
		{"accept", benchLink(0), true, "", false},
		{"invalid-url/parse", benchUnparseableLink, false, rejectInvalidURL, true},
		{"invalid-url/validate", benchPrivateIPLink, false, rejectInvalidURL, false},
	} {
		tc.check(b, "candidate", candidate)
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				benchOKSink, benchReasonSink, errBenchSink = candidate(tc.link)
			}
		})
	}
}

// BenchmarkCandidateUnwrapped is BenchmarkCandidate's invalid-url cases without
// the wraps. Only invalid-url pays them, and only this delta buys the wrapcheck
// suppression the finding asked for. The fixtures carry the same gate
// expectations as BenchmarkCandidate's, so the two sides of the delta cannot
// drift onto different branches from each other either.
func BenchmarkCandidateUnwrapped(b *testing.B) {
	for _, tc := range []benchCandidateCase{
		{"invalid-url/parse", benchUnparseableLink, false, rejectInvalidURL, true},
		{"invalid-url/validate", benchPrivateIPLink, false, rejectInvalidURL, false},
	} {
		tc.check(b, "candidateUnwrapped", candidateUnwrapped)
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				benchOKSink, benchReasonSink, errBenchSink = candidateUnwrapped(tc.link)
			}
		})
	}
}

// BenchmarkCandidatePreReason is the 26c8fe2 baseline for the accept path, so
// what the reject-reason change costs the path every harvested link takes is a
// delta in this binary rather than a comparison across two trees. It is the
// OTHER half of that delta, so it asserts the accept path too: with only
// BenchmarkCandidate/accept checked, a benchLink pointing at an isNoiseHost
// match still reports a figure here off the noise-host branch.
func BenchmarkCandidatePreReason(b *testing.B) {
	link := benchLink(0)
	if !candidatePreReason(link) {
		b.Fatalf("candidatePreReason(%q) = false, want true: the fixture no longer takes the accept path", link)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		benchOKSink = candidatePreReason(link)
	}
}

// benchHarvestArm is one BenchmarkHarvestPages arm: the harvest it prices, the
// fixture and crawler it prices it on, and what that harvest must DELIVER. The
// fixture guards check the pages; these check the measured call, so an arm whose
// crawler or harvest stopped reaching the branch its name claims fails instead
// of reporting a smaller figure under that name.
type benchHarvestArm struct {
	name    string
	crawler *Crawler
	pages   []string
	harvest func(*Crawler, []string, *[]string, *rejects, string) map[string]uint64
	// record threads a real rejects, per iteration as a cycle does, so the
	// reject bookkeeping a rejected repost pays is inside the figure.
	record     bool
	wantCand   int
	wantInline int
	// wantPost is whether the harvest must ATTRIBUTE a post to what it delivers,
	// read off the map it RETURNS. Counts cannot carry the segmented arm's
	// subject: a harvestPage that stops walking boundaries keeps that arm and
	// guarded byte-identical at 1848 B / 16 allocs with every other guard green,
	// and only makes the family faster (measured 2026-08-19). checkSegmentedPages
	// proves the fixture by calling nextMessage itself, which says nothing about
	// the harvest calling it.
	wantPost bool
	// wantReject is how many verdicts a record arm's bookkeeping must hold — the
	// same argument on the other side of the gate: dropping rej.record from
	// harvestPage takes noise from 3432 B / 37 allocs to 1464 / 12 with every
	// other guard green (measured 2026-08-19, -benchmem -count=5 medians), so the
	// arm that exists to price bookkeeping has to observe some.
	wantReject int
}

// check fails b unless the arm delivers what its name claims, called the way the
// timer calls it — rejects included, since that is the configuration measured.
func (a benchHarvestArm) check(b *testing.B) {
	b.Helper()
	var (
		probe []string
		rej   *rejects
	)
	if a.record {
		rej = newRejects(zerolog.Nop())
	}
	cand := a.harvest(a.crawler, a.pages, &probe, rej, benchChannel)
	if len(cand) != a.wantCand || len(probe) != a.wantInline {
		b.Fatalf("%s harvests %d candidates and %d inline nodes, want %d and %d",
			a.name, len(cand), len(probe), a.wantCand, a.wantInline)
	}
	for u, post := range cand {
		if (post != 0) != a.wantPost {
			b.Fatalf("%s harvests %q with post %d, so post attribution is %v, want %v",
				a.name, u, post, post != 0, a.wantPost)
		}
	}
	if a.record && len(rej.verdict) != a.wantReject {
		b.Fatalf("%s records %d reject verdicts, want %d", a.name, len(rej.verdict), a.wantReject)
	}
}

// BenchmarkHarvestPages prices the dedupe in front of the accepted key's
// strings.Clone and of candidate itself: guarded is the shipped harvestPages,
// blind asks and clones per occurrence. Neither the clone nor the parse is
// optional — extractURLs returns sub-slices, so an uncloned key holds its whole
// page for the cycle — and the dedupe is what keeps both per DISTINCT url
// instead of per repost.
//
// One arm, one subject. inline pays the unescape and runs the node scan, which
// is on page 1 alone: putting it back on every page reads here as six node
// buffers instead of one. segmented is guarded's own fixture rearranged into
// messages, so the pair prices harvestPage's boundary walk alone — the walk owes
// no copy per message, and what says so is the two arms agreeing to the byte
// TOGETHER with benchHarvestArm.wantPost, since a harvest that walked nothing
// would agree just as exactly. noise is the other repost shape, a link the gates
// turn down with a real rejects behind it, and wantReject is what makes the arm
// observe bookkeeping rather than merely be configured for it.
//
// What no arm here varies is the DISTINCT-candidate count: every fixture in
// this family is benchLinkRepeats occurrences of one link per page, the most
// favourable shape for anything that costs per key, and a per-key cost is
// invisible against a fixed six. BenchmarkHarvestPagesByDistinct is that axis,
// and it is where an item-14 reading on cand's value width belongs.
func BenchmarkHarvestPages(b *testing.B) {
	pages := benchPages()
	checkRepostPages(b, pages, benchLink)
	checkAcceptGate(b, pages)

	segPages := benchSegmentedPages()
	checkRepostPages(b, segPages, benchLink)
	checkSegmentedPages(b, segPages)

	inlinePages := benchInlinePages()
	checkInlinePages(b, inlinePages)

	noisePages := benchNoisePages()
	checkRepostPages(b, noisePages, benchNoiseLink)
	checkNoiseGate(b, noisePages)

	for _, tc := range []benchHarvestArm{
		{"guarded", &Crawler{}, pages, (*Crawler).harvestPages, false, benchPageCount, 0, false, 0},
		{"segmented", &Crawler{}, segPages, (*Crawler).harvestPages, false, benchPageCount, 0, true, 0},
		{"blind", &Crawler{}, pages, harvestPagesBlind, false, benchPageCount, 0, false, 0},
		{"inline", &Crawler{opts: Options{InlineEnabled: true}}, inlinePages, (*Crawler).harvestPages, false, 0, benchInlineNodes, false, 0},
		{"noise", &Crawler{}, noisePages, (*Crawler).harvestPages, true, 0, 0, false, benchPageCount},
	} {
		tc.check(b)
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				var inline []string
				var rej *rejects
				if tc.record {
					rej = newRejects(zerolog.Nop())
				}
				benchCandSink = tc.harvest(tc.crawler, tc.pages, &inline, rej, benchChannel)
			}
		})
	}
}

// BenchmarkHarvestPagesByDistinct varies the one axis every other fixture here
// holds fixed: how many DISTINCT candidates a channel's pages yield. Page
// bytes, page count, URL occurrences per page and URL length are all held, so
// the only thing moving is the repost factor, and with it the size of cand.
//
// It exists because a per-key cost is arithmetically invisible at repost factor
// benchLinkRepeats: BenchmarkHarvestPages' whole family yields six distinct
// keys, and a widened map value hid behind the wave's flat per-page saving
// there while crossing above HEAD past ~30 keys. An item-14 reading taken only
// on the favourable shape is not a reading.
func BenchmarkHarvestPagesByDistinct(b *testing.B) {
	if !equalStrings(benchDistinctPages(benchLinkRepeats), benchPages()) {
		b.Fatal("repost factor benchLinkRepeats must reproduce benchPages byte for byte, or this family has drifted off the fixture it varies")
	}
	c := &Crawler{}
	for _, repeats := range []int{20, 10, 5, 4, 2, 1} {
		pages := benchDistinctPages(repeats)
		distinct := benchPageCount * (benchLinkRepeats / repeats)
		checkDistinctPages(b, pages, distinct)
		var probe []string
		if cand := c.harvestPages(pages, &probe, nil, benchChannel); len(cand) != distinct {
			b.Fatalf("%d-distinct fixture harvests %d candidates: the curve is plotted against what the harvest keeps, not what the page holds",
				distinct, len(cand))
		}
		b.Run(strconv.Itoa(distinct), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				var inline []string
				benchCandSink = c.harvestPages(pages, &inline, nil, benchChannel)
			}
		})
	}
}

// checkDistinctPages asserts what the family holds as well as what it varies:
// a page that changed size or stopped carrying benchLinkRepeats occurrences
// would move every figure for a reason that is not the repost factor.
func checkDistinctPages(b *testing.B, pages []string, want int) {
	b.Helper()
	checkAcceptGate(b, pages)
	seen := map[string]bool{}
	for _, p := range pages {
		if len(p) != benchPageBytes {
			b.Fatalf("page is %d B, want benchPageBytes = %d", len(p), benchPageBytes)
		}
		urls := extractURLs(p)
		if len(urls) != benchLinkRepeats {
			b.Fatalf("page yields %d urls, want benchLinkRepeats = %d", len(urls), benchLinkRepeats)
		}
		for _, u := range urls {
			if len(u) != benchLinkBytes {
				b.Fatalf("page yields %q (%d B), want %d B", u, len(u), benchLinkBytes)
			}
			seen[u] = true
		}
	}
	if len(seen) != want {
		b.Fatalf("fixture yields %d distinct urls, want %d", len(seen), want)
	}
}

// checkSegmentedPages asserts the boundaries themselves: a fixture that lost
// them does not fail, it reprices the flat path under the segmented name.
func checkSegmentedPages(b *testing.B, pages []string) {
	b.Helper()
	for _, p := range pages {
		msgs, urls := 0, 0
		for rest := p; rest != ""; {
			seg, post, tail := nextMessage(rest)
			urls += len(extractURLs(seg))
			if post != 0 {
				if want := uint64(benchFirstPostID + msgs); post != want {
					b.Fatalf("boundary %d has post %d, want %d", msgs, post, want)
				}
				msgs++
			}
			rest = tail
		}
		if msgs != benchLinkRepeats || urls != benchLinkRepeats {
			b.Fatalf("page splits into %d messages holding %d urls, want %d of each",
				msgs, urls, benchLinkRepeats)
		}
	}
}

// checkRepostPages fails b unless every page is benchPageBytes long, holds no
// entity and carries benchLinkRepeats copies of link(i). The key ITSELF is
// checked, not merely how many came back: filler that stopped being separated
// from a link, or a scan that started keeping the trailing text, would leave
// the count right and reprice every B/op here. The entity is the dual of
// checkInlinePages' — one '&' anywhere buys these arms a copy of the page they
// exist to price without one. Every page, not page 0: the blind arm clones per
// occurrence over all six, so a page that quietly stopped reposting halves an
// arm's figure with page 0 still perfect.
func checkRepostPages(b *testing.B, pages []string, link func(int) string) {
	b.Helper()
	for i, p := range pages {
		if len(p) != benchPageBytes {
			b.Fatalf("page %d is %d B, want benchPageBytes = %d", i, len(p), benchPageBytes)
		}
		// Aliasing, not content: a rebuilt page compares equal to the one it was
		// built from, so this is the only form that sees an unescapeInto that
		// stopped handing an entity-free page back — every arm here gains a page
		// copy with the content form green (guarded 1848 B / 16 allocs to
		// 59192 / 17, measured 2026-08-19). reject_test.go:802 makes the same
		// class of claim the same way.
		if text, _ := unescapeInto(nil, p); text != p || unsafe.StringData(text) != unsafe.StringData(p) {
			b.Fatalf("page %d is not handed back uncopied: it holds an entity, or the scan rebuilt it", i)
		}
		links := extractURLs(html.UnescapeString(p))
		if len(links) != benchLinkRepeats {
			b.Fatalf("page %d yields %d urls, want benchLinkRepeats = %d", i, len(links), benchLinkRepeats)
		}
		for _, got := range links {
			if got != link(i) || len(got) != benchLinkBytes {
				b.Fatalf("page %d yields %q (%d B), want the %d B link %q", i, got, len(got), benchLinkBytes, link(i))
			}
		}
	}
}

// checkInlinePages fails b unless every page still holds an entity and page 0
// still holds benchInlineNodes nodes: unescapeInto hands back an entity-free
// page uncopied, so a fixture that lost its entities prices a scan under the
// name of six copies.
func checkInlinePages(b *testing.B, pages []string) {
	b.Helper()
	for i, p := range pages {
		if len(p) != benchPageBytes {
			b.Fatalf("inline page %d is %d B, want benchPageBytes = %d", i, len(p), benchPageBytes)
		}
		if text, _ := unescapeInto(nil, p); text == p {
			b.Fatalf("inline page %d holds no entity, so unescapeInto copies nothing", i)
		}
	}
	if got := len(extractInlineNodes(html.UnescapeString(pages[0]))); got != benchInlineNodes {
		b.Fatalf("inline page yields %d nodes, want benchInlineNodes = %d", got, benchInlineNodes)
	}
}

// checkAcceptGate fails b unless the repost fixture still takes candidate's
// accept path: a link that drifted onto a reject branch reprices every arm but
// noise downwards, and a figure that only falls is one no reading questions.
func checkAcceptGate(b *testing.B, pages []string) {
	b.Helper()
	for _, page := range pages {
		for _, raw := range extractURLs(html.UnescapeString(page)) {
			if ok, reason, err := candidate(raw); !ok || reason != "" || err != nil {
				b.Fatalf("candidate(%q) = %v %q %v, want true \"\" <nil>", raw, ok, reason, err)
			}
		}
	}
}

// checkNoiseGate fails b unless the noise fixture still takes candidate's
// noise-host gate, which is the branch it exists to price.
func checkNoiseGate(b *testing.B, pages []string) {
	b.Helper()
	for _, page := range pages {
		for _, raw := range extractURLs(html.UnescapeString(page)) {
			if ok, reason, err := candidate(raw); ok || reason != rejectNoiseHost || err != nil {
				b.Fatalf("candidate(%q) = %v %q %v, want false %q <nil>", raw, ok, reason, err, rejectNoiseHost)
			}
		}
	}
}

// TestHarvestPagesBlindMirrorsTheShippedHarvest makes harvestPagesBlind's
// divergence list executable. That list was prose for a whole wave, and the drift
// it failed to catch — the twin scanning per page after harvestPage stopped —
// published a figure whose agreement with HEAD was a coincidence of the twin's
// own apparatus rather than a property of the harvest.
//
// Pinned here, because behaviour shows them: both harvests keep the SAME keys on
// all three fixture shapes, both clone what they keep, the twin attributes no
// post where the boundary walk attributes one, and with InlineEnabled both
// accumulate page 0's nodes and only page 0's — benchInlinePages numbers its
// nodes by page, so a twin that scanned another page, every page, or none would
// not match. Pinned in twin_test.go instead, because only an allocation level
// shows them: one url slice and one scratch per call, and the presence of the
// dedupe divergence. Pinned by nothing but this file compiling: the receiver
// being a parameter. The SIZE of the dedupe
// divergence is deliberately left to BenchmarkHarvestPages — freezing it here
// would freeze the number that benchmark exists to report.
func TestHarvestPagesBlindMirrorsTheShippedHarvest(t *testing.T) {
	t.Parallel()
	c := &Crawler{}
	harvest := func(fn func(*Crawler, []string, *[]string, *rejects, string) map[string]uint64, pages []string) map[string]uint64 {
		var inline []string
		return fn(c, pages, &inline, nil, benchChannel)
	}
	for _, tc := range []struct {
		name  string
		pages []string
	}{
		{"repost", benchPages()},
		{"distinct", benchDistinctPages(1)},
		{"segmented", benchSegmentedPages()},
	} {
		shipped, twin := harvest((*Crawler).harvestPages, tc.pages), harvest(harvestPagesBlind, tc.pages)
		if len(shipped) != len(twin) {
			t.Fatalf("%s: shipped keeps %d keys and the twin %d, so the twin no longer gates the same urls",
				tc.name, len(shipped), len(twin))
		}
		for k := range shipped {
			if _, ok := twin[k]; !ok {
				t.Fatalf("%s: the twin does not keep %q", tc.name, k)
			}
		}
		checkKeysCloned(t, tc.name+"/shipped", shipped, tc.pages)
		checkKeysCloned(t, tc.name+"/twin", twin, tc.pages)
	}

	segPages := benchSegmentedPages()
	for u, post := range harvest((*Crawler).harvestPages, segPages) {
		if post == 0 {
			t.Fatalf("the shipped harvest attributes no post to %q, so the fixture cannot show the twin's divergence", u)
		}
	}
	for u, post := range harvest(harvestPagesBlind, segPages) {
		if post != 0 {
			t.Fatalf("the twin attributes post %d to %q: it is walking boundaries, which its list says it does not", post, u)
		}
	}

	inlineOn := &Crawler{opts: Options{InlineEnabled: true}}
	inlinePages := benchInlinePages()
	nodes := func(fn func(*Crawler, []string, *[]string, *rejects, string) map[string]uint64) []string {
		var inline []string
		fn(inlineOn, inlinePages, &inline, nil, benchChannel)
		return inline
	}
	shippedNodes, twinNodes := nodes((*Crawler).harvestPages), nodes(harvestPagesBlind)
	if len(shippedNodes) != benchInlineNodes {
		t.Fatalf("the shipped harvest accumulates %d inline nodes, want benchInlineNodes = %d: the fixture cannot show which pages the twin scans", len(shippedNodes), benchInlineNodes)
	}
	if len(twinNodes) != len(shippedNodes) {
		t.Fatalf("the twin accumulates %d inline nodes and the shipped harvest %d: its list says it scans page 0 and nothing else", len(twinNodes), len(shippedNodes))
	}
	for i := range shippedNodes {
		if shippedNodes[i] != twinNodes[i] {
			t.Fatalf("inline node %d is %q in the twin and %q in the shipped harvest: they are not scanning the same page", i, twinNodes[i], shippedNodes[i])
		}
	}
}

// checkKeysCloned fails t unless every key was copied out of the page it was
// scanned from. appendURLs hands out sub-slices, so an uncloned key holds its
// whole page — the property TestHarvestedKeyDoesNotPinThePage asserts for the
// shipped harvest and nothing asserted for the twin. uintptr rather than
// reject_test.go:802's bare comparison because a sub-slice is not the page's own
// pointer: it lands anywhere inside its range.
func checkKeysCloned(t *testing.T, name string, cand map[string]uint64, pages []string) {
	t.Helper()
	for _, p := range pages {
		base := uintptr(unsafe.Pointer(unsafe.StringData(p)))
		for k := range cand {
			got := uintptr(unsafe.Pointer(unsafe.StringData(k)))
			if got >= base && got < base+uintptr(len(p)) {
				t.Fatalf("%s: key %q points into the page it was scanned from, pinning all %d B of it", name, k, len(p))
			}
		}
	}
}
