package crawl

import (
	"net/url"
	"slices"
	"strings"

	"github.com/rs/zerolog"
)

// rejectReason names why one discovered candidate did not become a managed
// source. The crawler's per-cycle counters used to be the only trace of the
// decision — a cycle reported `discovered` and `productive` and nothing about
// the links it looked at and dropped, so a link that stopped becoming a source
// was indistinguishable from a link nobody ever posted. (Those two counters
// cannot be subtracted for a rejected count: `productive` is len(state.Productive),
// a map of CHANNELS, despite its "live subscriptions discovered" message.)
//
// The set covers candidate's pre-fetch rejections and every post-fetch verdict
// (classifyAll). Two reasons, not three, cover candidate: its parse gate and its
// validate gate both report invalid-url, and only the noise-host gate has a
// reason to itself. It is a string type because its only consumers are a log
// field and a summary key; nothing branches on it.
type rejectReason string

const (
	rejectNoiseHost  rejectReason = "noise-host"  // Telegram itself or its media CDN: never a subscription
	rejectInvalidURL rejectReason = "invalid-url" // unparseable, or a URL config.Load would refuse
	// rejectFetch is every non-status error classify.URL can return, not only a
	// transport one: DNS, TLS, timeout and read failures, but also a rejected
	// URL and a body over the 10 MiB cap. Its `create request` wrap is NOT on
	// that list — it is unreachable, for the reason safeError's inventory gives.
	rejectFetch    rejectReason = "fetch-failed"
	rejectStatus   rejectReason = "bad-status"   // origin answered non-2xx; the code is logged alongside
	rejectNodeless rejectReason = "nodeless-2xx" // 2xx body carrying no proxy-scheme node
	rejectExpired  rejectReason = "expired"      // origin advertised an expiry already past
)

// rejectField gives each reason its column in the per-cycle summary.
// Completeness here is not a convention, it is built: .golangci.yml runs
// `exhaustive` with `check: [switch, map]`, which fails a map literal keyed by
// an enum type that is missing a constant. Nothing weaker would do — a test
// only sees the reasons its fixture produces, so a newly added reason wired
// into production and forgotten here would count into the total while having no
// column of its own, and every existing test would stay green.
var rejectField = map[rejectReason]string{
	rejectNoiseHost:  "noise_host",
	rejectInvalidURL: "invalid_url",
	rejectFetch:      "fetch_failed",
	rejectStatus:     "bad_status",
	rejectNodeless:   "nodeless_2xx",
	rejectExpired:    "expired",
}

// maxRejectLines caps per-candidate lines per cycle. These are INFO, not DEBUG,
// and deliberately so: the crawler is a separate process (`sub-preprocessor
// crawl`) that never loads config.yaml, main's log.InitDefault pins the global
// zerolog level to info before the subcommand switch runs, and crawl.Options has
// no level knob — a DEBUG line here would emit zero times in production and
// could not be turned on without a rebuild.
//
// INFO therefore needs its own ceiling. It is set to defaultMaxDiscovered
// because that is what bounds the fan-out feeding it — up to 200 discovered
// channels per cycle, each contributing candidates — so a repost-graph blow-up
// costs a bounded number of lines instead of a multiple of the previous
// cycle's. Counting is NOT capped: the per-cycle summary stays complete no
// matter how many lines were withheld.
const maxRejectLines = defaultMaxDiscovered

// redactedError replaces an error message that still names the candidate URL
// after substitution; see safeError.
const redactedError = "redacted: error names the candidate url"

// redactedPathError replaces an error message that names the candidate's path or
// query while carrying no scheme for the redactedError rule to catch; see
// safeError. It is a separate string because it means something else — but that
// is two possibilities, not one, and the likelier one is a false positive.
// namesSecret substring-matches and is deliberately over-eager, so a SHORT path
// collides with ordinary error prose: a candidate whose path is "/o" matches the
// "i/o" of `dial tcp: i/o timeout`, and `net/http: TLS handshake timeout` has a
// slash of its own — the two commonest crawler failures. Demonstrated on this
// tree: repoint reject_test.go's fixDead at
// https://dead.example/o?payload=DEADSECRET9999, and
// TestRejectLinesCarryNoCredential's dead.example line loses its real diagnosis
// to this string with no code naming any path. Keep a query on fixDead when you
// try it — TestRejectLinesCarryNoCredential runs mustQuery over it, so a bare
// https://dead.example/o aborts on that precondition before a line is emitted.
// (mustQuery covers five of the thirteen fixture URLs. fixPanel deliberately has
// no query at all — see its own comment — so "every fixture carries a query" is
// NOT the contract, and adding one to fixPanel would destroy the Marzban/3x-ui
// shape this file exists to keep out of the log.)
// So read this string in that order: check the candidate's own path and query
// against the message the code would have produced, and only if they cannot
// collide does the text mean the other thing — that an error on the reject path
// really did grow the shape nothing produces today, and the code it names needs
// a look.
const redactedPathError = "redacted: error names the candidate url path or query"

// maxRejectTracked bounds the dedupe set. It deliberately mirrors
// maxInlineAccum, the bound on the OTHER accumulator harvestPages fills — over
// the newest page only, where this one spans every page. Same reason, too: a
// single page can carry a huge list, and unlike live there is no network brake
// here — a URL that fails a candidate gate is rejected by local string work
// alone, so one 8 MiB page of `https://t.me/a`-shaped links yields hundreds of
// thousands of distinct keys. maxRejectLines does not bound this: it caps
// lines, and counting is deliberately not capped.
const maxRejectTracked = maxInlineAccum

// rejects accumulates, over one cycle, why each discovered candidate failed to
// become a managed source. It is created per cycle by scan and threaded down the
// discovery path; a nil *rejects records nothing, which is how recheckManaged
// shares classifyAll without folding existing sources into a summary about newly
// discovered links.
//
// Not safe for concurrent use: classifyAll records under the same mutex that
// guards its result maps, and every other caller is single-goroutine.
type rejects struct {
	logger zerolog.Logger
	// verdict is the first rejection recorded per URL. The same link is
	// repeated across posts and pages, and a link reposted into two channels is
	// classified once per channel, so without this dedupe one candidate could
	// spend dozens of lines and inflate every count.
	verdict map[string]rejectReason
	// untracked counts rejections past maxRejectTracked, where the dedupe set
	// stopped growing and a repeat can no longer be recognized. They are
	// reported apart from the per-reason counts rather than folded in, which
	// would inflate those with duplicates.
	untracked int
	logged    int
}

func newRejects(logger zerolog.Logger) *rejects {
	return &rejects{logger: logger, verdict: map[string]rejectReason{}}
}

// record notes one rejected candidate and, until the cap, logs it. code is the
// HTTP status for rejectStatus and 0 otherwise; err carries the detail for
// rejectFetch and rejectInvalidURL.
func (r *rejects) record(channel, rawURL string, reason rejectReason, code int, err error) {
	if r == nil {
		return
	}
	if _, dup := r.verdict[rawURL]; dup {
		return
	}
	if len(r.verdict) >= maxRejectTracked {
		r.untracked++
		return
	}
	// Clone: rawURL is a sub-slice of the scraped page. extractURLs runs
	// html.UnescapeString then urlRe.FindAllString, regexp returns s[a:b], and
	// strings.TrimRight narrows without copying — so a 40-byte key kept for the
	// whole cycle would keep its entire page reachable (up to maxPageBytes,
	// 8 MiB) until scan returns. A cloned key costs its own length and nothing
	// else, whatever the page size.
	//
	// This map was never the only holder, and the accepted key was always the
	// longer-lived one: harvestPages puts it in cand, classifyAll copies it into
	// live, and scanChannel hands live up to RunOnce for mergeManaged. It clones
	// for this same reason, and did not before — the pin there predates this map
	// rather than arriving with it. BenchmarkHarvestPages prices that twin.
	r.verdict[strings.Clone(rawURL)] = reason
	if r.logged >= maxRejectLines {
		return
	}
	r.logged++

	u, parseErr := url.Parse(rawURL)
	ev := r.logger.Info().
		Str("channel", channel).
		// The name sourceName would give this URL if this channel were its
		// discoverer — no existing name and no used-name set, which is exactly
		// a never-accepted candidate's state, and reading a nil map is legal.
		// A URL first discovered elsewhere gets a different slug there, so it
		// is the channel-independent sha6 suffix that correlates this line to a
		// private.yaml entry. Either way no credential is in it.
		Str("source", sourceName(rawURL, "", channel, nil)).
		Str("host", logHost(u)).
		Str("reason", string(reason))
	if code != 0 {
		ev = ev.Int("status", code)
	}
	if err != nil {
		ev = ev.Str("error", safeError(err, rawURL, u))
	}
	if parseErr != nil {
		// logHost is empty for a URL that would not parse, so say so rather
		// than emit a line whose only identity is the hash.
		ev = ev.Bool("unparseable", true)
	}
	ev.Msg("candidate rejected")
}

// report emits the per-cycle summary: one count per reason plus the total, so
// "why did this cycle find fewer subscriptions" is answerable without reading
// every line. The counts are complete even when the cap withheld lines, and the
// withheld count is reported with them. They are complete only up to
// maxRejectTracked, though: past that bound record can no longer dedupe, so the
// overflow is reported as `untracked` beside the columns rather than counted
// into them, and `rejected` still equals their sum.
//
// live is the cycle's accepted set. A URL rejected in one channel and classified
// live in another is not a rejection — the cycle adopted it — so it is dropped
// from the counts here. Its per-candidate line still stands: that line records
// what one channel's fetch actually answered, which is true and is the point.
func (r *rejects) report(live map[string]string) {
	if r == nil {
		return
	}
	counts := make(map[rejectReason]int, len(rejectField))
	total := 0
	for u, reason := range r.verdict {
		if _, accepted := live[u]; accepted {
			continue
		}
		counts[reason]++
		total++
	}

	type column struct {
		field string
		n     int
	}
	cols := make([]column, 0, len(rejectField))
	for reason, field := range rejectField {
		cols = append(cols, column{field, counts[reason]})
	}
	// Map iteration is randomized; a summary whose field order changes every
	// cycle is needlessly hard to diff.
	slices.SortFunc(cols, func(a, b column) int { return strings.Compare(a.field, b.field) })

	ev := r.logger.Info().Int("rejected", total)
	for _, c := range cols {
		ev = ev.Int(c.field, c.n)
	}
	if r.untracked > 0 {
		// Reported apart from the per-reason counts, not added to them: past
		// maxRejectTracked a repeat cannot be told from a new candidate, so
		// these are rejections-with-duplicates, a different quantity.
		ev = ev.Int("untracked", r.untracked)
	}
	ev.Msg("candidates rejected by reason")

	if suppressed := len(r.verdict) - r.logged; suppressed > 0 {
		r.logger.Info().Int("suppressed", suppressed).Int("cap", maxRejectLines).
			Msg("per-candidate reject lines withheld by the cycle cap")
	}
}

// logHost renders a candidate URL as its host and port. Neither the path nor the
// query is logged, and dropping the path is not caution about noise: Marzban
// serves subscriptions at {prefix}/{XRAY_SUBSCRIPTION_PATH}/<USER_TOKEN> and
// 3x-ui at /<subPath>/<subId>, so for those two the credential IS the path and
// there is no query at all. The observed
// is.wepogp.gay/bypass-hwid-lock-3z5O6BFAaJQzGlamvtSo is the same shape with the
// token as the FIRST segment, so keeping a prefix of the path would not help
// either. Nothing is lost: the tg-<slug>-<sha6> on the same line is the exact
// identity, and the host is the part an operator recognizes.
//
// url.URL keeps userinfo in User, so Host cannot carry credentials that way.
func logHost(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.Host
}

// safeError renders err for a candidate line with the candidate's own URL
// swapped for its host. net/http wraps every transport failure in *url.Error,
// whose Error() embeds the full request URL — path and query included — so the
// verbatim text can never be logged.
//
// Any URL the substitution did not recognize costs the whole message. A refused
// redirect puts a SECOND, different URL in the error (CheckRedirect validates
// the redirect target and net/http wraps that verdict around the original
// request), and there is no way to tell which part of an unknown URL is a
// credential. Dropping the text loses a diagnosis; editing it down guesses with
// the credential.
//
// An error naming a URL's path or query with NO scheme slips both rules above:
// the substitution matches the whole URL only, and there is no "://" left to
// see. namesSecret is the guard for that, and it is the last line of defence
// for the credential — for Marzban and 3x-ui the path IS the token (see
// logHost). Nothing on the reject path returns that shape today, but not for one
// reason. candidate's parse wrap carries a *url.Error, %q-quoted with its scheme
// intact; its validate wrap carries no part of the URL at all, since the
// reachable returns of fetch.ValidatePublicHTTPSURL are the four static strings
// at internal/fetch/fetch.go:35-38 and the one branch that would quote a URL
// (`invalid url: %w`) cannot fire from candidate, which ran the same url.Parse
// one line above. Do NOT read that as "internal/fetch quotes its URLs" — nothing
// in that package quotes anything, so an %s of URL detail appended to one of
// those four strings is precisely the leak this guard exists for. classify.URL's
// are `validate url`, `do request` (*url.Error, %q-quoted, scheme intact),
// `read response` and `response too large` — its `create request` wrap is
// unreachable, since http.NewRequestWithContext re-parses a URL ValidateHTTPSURL
// already parsed, and its *StatusError never arrives here at all (classifyAll
// records rejectStatus with the code and a nil err). That inventory is the
// invariant, and it is one edit away from false — `read response %s` naming
// req.URL.EscapedPath() is the obvious "which subscription failed?" change and
// would land the token in a log verbatim.
//
// So the guard is not a spare tyre for a shape nobody produces: it is what makes
// the inventory safe to be wrong about. TestRejectLineKeepsRealClassifyErrors
// routes each of those errors, produced by a real server through the real
// classify.URL, into record and asserts the diagnosis SURVIVES — so the edit
// above stops being a silent leak and becomes a failing test, because the guard
// redacts it. TestSafeErrorRedactsASchemelessPath exercises the guard itself.
func safeError(err error, rawURL string, u *url.URL) string {
	msg := strings.ReplaceAll(err.Error(), rawURL, logHost(u))
	if strings.Contains(msg, "://") {
		return redactedError
	}
	if namesSecret(msg, u) {
		return redactedPathError
	}
	return msg
}

// namesSecret reports whether msg still carries the part of the candidate URL
// that may BE the credential: its path or its query. Deliberately substring
// matching, and deliberately over-eager — a one-character path like "/o" also
// matches the "i/o" in a dial timeout, costing that line its diagnosis. The
// asymmetry is the point: a false positive loses a message, a false negative
// publishes a subscription token. A path of exactly "/" is not a secret and
// would match nearly every message, so it is excluded.
func namesSecret(msg string, u *url.URL) bool {
	if u == nil {
		return false
	}
	if q := u.RawQuery; q != "" && strings.Contains(msg, q) {
		return true
	}
	p := u.EscapedPath()
	return len(p) > 1 && strings.Contains(msg, p)
}
