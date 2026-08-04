package crawl //nolint:testpackage // benchmarks unexported crawl internals (candidate, harvestPages)

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

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
	benchCandSink   map[string]struct{}
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

	// benchPrivateIPLink takes candidate's validate gate: ValidatePublicHTTPSURL
	// parses it and refuses a non-public target with a static string, so the
	// returned error is wrapped by the second fmt.Errorf and is NOT a
	// *url.Error. benchCandidateCase.check asserts that gate; this comment no
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

// benchPages builds benchPageCount pages of exactly benchPageBytes, each
// carrying ONE distinct link reposted benchLinkRepeats times among filler that
// contains no URL and no HTML entity — html.UnescapeString copies only when it
// finds an '&', and a page that made it copy would price the unescape instead
// of the harvest.
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

// candidateUnwrapped is candidate returning the bare external error instead of
// wrapping it. It is the alternative the eager-fmt.Errorf finding proposed, and
// the only reason the branch declined it is the difference between
// BenchmarkCandidate and BenchmarkCandidateUnwrapped on the invalid-url cases —
// against a wrapcheck suppression on a bare fetch/net-url error. Both wraps are
// on the invalid-url path only, and the wrap's cost is not one number: it scales
// with the text of the message %w re-renders.
func candidateUnwrapped(raw string) (bool, rejectReason, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return false, rejectInvalidURL, err
	}
	if isNoiseHost(u.Hostname()) {
		return false, rejectNoiseHost, nil
	}
	if err = fetch.ValidatePublicHTTPSURL(fetch.SubscriptionURL(raw)); err != nil {
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

// harvestPagesBlind is harvestPages with the dedupe guard dropped: it clones on
// every occurrence instead of every distinct URL. Everything else is the shipped
// body, so the two benchmarks differ in that one branch. The receiver is a
// parameter deliberately — this is not a method the package ships.
func harvestPagesBlind(c *Crawler, pages []string, inline *[]string, rej *rejects, channel string) map[string]struct{} {
	cand := map[string]struct{}{}
	for _, p := range pages {
		for _, raw := range extractURLs(p) {
			ok, reason, err := candidate(raw)
			if !ok {
				rej.record(channel, raw, reason, 0, err)
				continue
			}
			cand[strings.Clone(raw)] = struct{}{}
		}
		if c.opts.InlineEnabled && len(*inline) < maxInlineAccum {
			*inline = append(*inline, extractInlineNodes(p)...)
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
	// rather than at its ValidatePublicHTTPSURL one. wantReason cannot tell the
	// two apart — both report rejectInvalidURL — and a *url.Error is what does:
	// url.Parse always returns one, while the validator's reachable returns on
	// this path are static strings (its single "invalid url: %w" branch cannot
	// fire from candidate, which parsed the same URL a line above).
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

// BenchmarkHarvestPages prices the dedupe guard in front of the accepted key's
// strings.Clone: guarded is the shipped harvestPages, blind clones per
// occurrence. The clone itself is not optional — extractURLs returns sub-slices,
// so an uncloned key holds its whole page for the cycle — and the guard is what
// keeps its cost per DISTINCT url instead of per repost.
func BenchmarkHarvestPages(b *testing.B) {
	pages := benchPages()
	for _, p := range pages {
		if len(p) != benchPageBytes {
			b.Fatalf("page is %d B, want benchPageBytes = %d", len(p), benchPageBytes)
		}
	}
	// What the harvest allocates is one clone of the key extractURLs hands it,
	// so the key ITSELF is checked, not merely how many came back: filler that
	// stopped being separated from a link, or a urlRe that started keeping the
	// trailing text, would leave the count right and reprice every B/op here.
	links := extractURLs(pages[0])
	if len(links) != benchLinkRepeats {
		b.Fatalf("page yields %d urls, want benchLinkRepeats = %d", len(links), benchLinkRepeats)
	}
	for _, got := range links {
		if got != benchLink(0) || len(got) != benchLinkBytes {
			b.Fatalf("page yields %q (%d B), want the %d B link %q",
				got, len(got), benchLinkBytes, benchLink(0))
		}
	}

	for _, tc := range []struct {
		name    string
		harvest func(*Crawler, []string, *[]string, *rejects, string) map[string]struct{}
	}{
		{"guarded", (*Crawler).harvestPages},
		{"blind", harvestPagesBlind},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				var inline []string
				benchCandSink = tc.harvest(&Crawler{}, pages, &inline, nil, "benchchannel")
			}
		})
	}
}
