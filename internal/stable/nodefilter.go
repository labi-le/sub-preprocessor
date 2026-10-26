package stable

import (
	"context"

	mihomo "github.com/metacubex/mihomo/constant"
	"github.com/rs/zerolog"
)

// NodeFilter is a through-node check the worker runs on latency-probe
// survivors: unlike the preprocess IP-filters it routes traffic THROUGH each
// proxy node, so it lives here and only affects /stable.txt. The through-node
// entries of the unified filters list (gemini/claude/chatgpt/tidal/deepl/
// bandwidth) select which run.
type NodeFilter interface {
	// apply narrows survivors using the shared, pre-parsed proxies grouped by
	// node label. One label can hold several proxies — mihomo expands a
	// mierus:// link into one per configured port — and ALL of them belong in
	// the check subset, so a port that is dead on our egress cannot mask a
	// live sibling. The check folds their outcomes back onto the label. The
	// checker owns the proxies' lifecycle; filters only read.
	//
	// A drop lasts exactly this cycle: the checker remembers nothing a filter
	// decides. A verdict worth outliving the cycle goes to the long-lived
	// geoblock store, which the filter writes itself through its own store
	// field (see apiFilter) and which preprocess then honours on every later
	// cycle, dropping the node before it is even merged.
	apply(ctx context.Context, survivors []Survivor, proxies map[string][]mihomo.Proxy) (kept []Survivor, rep FilterReport)
}

// geminiChecker is the through-node Gemini capability of a Prober.
type geminiChecker interface {
	GeminiEnabled() bool
	GeminiCheck(ctx context.Context, proxies []mihomo.Proxy) map[string]APIOutcome
}

// claudeChecker is the through-node Anthropic capability of a Prober.
type claudeChecker interface {
	ClaudeCheck(ctx context.Context, proxies []mihomo.Proxy) map[string]APIOutcome
}

// chatgptChecker is the through-node OpenAI capability of a Prober.
type chatgptChecker interface {
	ChatGPTCheck(ctx context.Context, proxies []mihomo.Proxy) map[string]APIOutcome
}

// tidalChecker is the through-node Tidal capability of a Prober.
type tidalChecker interface {
	TidalCheck(ctx context.Context, proxies []mihomo.Proxy) map[string]APIOutcome
}

// deeplChecker is the through-node DeepL capability of a Prober.
type deeplChecker interface {
	DeeplCheck(ctx context.Context, proxies []mihomo.Proxy) map[string]APIOutcome
}

// bandwidthChecker is the through-node download-speed capability of a Prober.
type bandwidthChecker interface {
	BandwidthCheck(ctx context.Context, proxies []mihomo.Proxy) map[string]BandwidthOutcome
	BandwidthMinMbps() int
}

// Configuration names of the through-node API filters.
const (
	geminiFilterName    = "gemini"
	claudeFilterName    = "claude"
	chatgptFilterName   = "chatgpt"
	tidalFilterName     = "tidal"
	deeplFilterName     = "deepl"
	bandwidthFilterName = "bandwidth"
)

// Drop reasons a through-node filter reports in FilterReport.Dropped, and the
// matching log keys. They are not free prose: internal/metrics publishes each
// one verbatim as stable_filter_dropped_nodes{reason=...}, and the Grafana
// dashboard documents them by name, so the VALUES are a wire format — rename
// the identifiers freely, never the strings.
const (
	dropBlocked     = "blocked"
	dropUnreachable = "unreachable"
	dropSlow        = "slow"
)

// apiFilter keeps only survivors that pass a through-node API check, and records
// geo-blocked node hosts in the store (TTL) so later cycles drop them in
// preprocess, before they are ever merged. A nil store (see the tidal filter)
// skips that persistence, so its drop costs this cycle only. What counts as
// blocked is the check's own verdict: a geo-block marker in the body for
// gemini/claude/chatgpt, a non-2xx for tidal, a 451 for deepl. A nil enabled
// func means the check is always active (e.g. Anthropic geo-blocks before
// authentication, so its check is keyless).
//
// A response the check could not read to its end -- a read error, a timeout
// after the headers, or a body past maxAPIBody -- is NOT a pass for a
// marker-based check unless the bytes received already carry the marker: an
// observed marker is a refusal, so it stays Blocked/Verified and can drop the
// node and write the store. Without one, and for a Gemini answer that carries
// no location verdict (geminiInconclusive), APIOutcome.Verified stays false and
// the node is kept unverified (fail-open) with no store write. DeepL instead
// requires verified dual-transport acceptance and drops unverified responses.
//
// The report's Coverage counts those outcomes per SURVIVOR label, not per
// proxy port, and is measured on every completing path including the breaker's.
// DeepL still drops failed nodes when the breaker suppresses persistence;
// other gates keep the batch when its verdict is disbelieved.
type apiFilter struct {
	filterName      string
	requireVerified bool
	enabled         func() bool
	check           func(ctx context.Context, proxies []mihomo.Proxy) map[string]APIOutcome
	store           Blocklist
	logger          zerolog.Logger
}

func (f *apiFilter) apply(ctx context.Context, survivors []Survivor, proxies map[string][]mihomo.Proxy) ([]Survivor, FilterReport) {
	rep := FilterReport{Name: f.filterName, In: len(survivors), Kept: len(survivors), Dropped: map[string]int{}}
	if f.enabled != nil && !f.enabled() {
		f.logger.Warn().Str("filter", f.filterName).Msg("filter configured but disabled; skipping")
		return survivors, rep
	}

	subset := filterSubset(survivors, proxies)
	outcomes := f.check(ctx, subset)
	if ctx.Err() != nil && !f.requireVerified {
		// A cancelled check yields partial outcomes; don't record blocks or
		// drop survivors based on them.
		f.logger.Warn().Str("filter", f.filterName).Msg("filter cancelled; keeping survivors unchanged")
		return survivors, rep
	}

	// Count before persisting: a batch-wide outage must not poison the host store.
	blocked, unreachable, unverified, verified := 0, 0, 0, 0
	for _, s := range survivors {
		switch o := outcomes[s.Label]; {
		case o.Blocked:
			blocked++
			verified++
		case !o.Reachable:
			unreachable++
		case o.Verified:
			verified++
		default:
			unverified++
		}
	}
	// Refusals are conclusive evidence too; unanswered requests carry no coverage.
	rep.Coverage = FilterCoverage{Verified: verified, Unverified: unverified, Measured: true}
	failed := blocked + unreachable
	if f.requireVerified {
		failed += unverified
	}
	tripped := breakerTrips(failed, len(survivors))
	if tripped && !f.requireVerified {
		f.logger.Warn().Str("filter", f.filterName).Int("survivors", len(survivors)).
			Int(dropBlocked, blocked).Int(dropUnreachable, unreachable).
			Int("threshold_pct", precheckBreakerPercent).
			Msg("gate refused every survivor; treating the batch verdict as the endpoint's or our egress's, not the nodes', and keeping all survivors")
		rep.Dropped[dropBlocked] = 0
		rep.Dropped[dropUnreachable] = 0
		rep.State = FilterTripped
		return survivors, rep
	}

	kept := f.keepOutcomes(ctx, survivors, outcomes, !tripped)
	if unverified > 0 && !f.requireVerified {
		f.logger.Warn().Str("filter", f.filterName).Int("unverified", unverified).Int("survivors", len(survivors)).
			Msg("API checks reached these survivors without a conclusive verdict; their nodes are kept unverified")
	}
	f.logger.Info().Str("filter", f.filterName).Int("survivors", len(survivors)).Int("kept", len(kept)).
		Int(dropBlocked, blocked).Int(dropUnreachable, unreachable).Int("unverified", unverified).Msg("node filter")
	rep.Kept = len(kept)
	rep.Dropped = map[string]int{dropBlocked: blocked, dropUnreachable: unreachable}
	rep.State = FilterRan
	if f.requireVerified {
		rep.Dropped["unverified"] = unverified
		if tripped {
			rep.State = FilterTripped
			f.logger.Warn().Str("filter", f.filterName).Int("failed", failed).
				Msg("strict gate dropped failed survivors; batch breaker suppresses persistence, not drops")
		}
	}
	return kept, rep
}

func (f *apiFilter) keepOutcomes(ctx context.Context, survivors []Survivor, outcomes map[string]APIOutcome, trusted bool) []Survivor {
	kept := make([]Survivor, 0, len(survivors))
	for _, s := range survivors {
		o := outcomes[s.Label]
		if o.Blocked {
			// Host-wide persistence must not turn an endpoint outage into a month-long ban.
			if f.store != nil && trusted && ctx.Err() == nil {
				if err := f.store.Block(o.Server); err != nil {
					f.logger.Warn().Err(err).Str("host", o.Server).Msg("geoblock write failed")
				}
			}
			continue
		}
		if o.Reachable && (o.Verified || !f.requireVerified) {
			kept = append(kept, s)
		}
	}
	return kept
}

// bandwidthFilter keeps only survivors whose measured through-node download
// speed is at least minMbps and records Mbps on each kept survivor, which the
// publication turns into the [SPD:] tag. minMbps==0 removes only the speed
// THRESHOLD: a survivor whose download failed outright (dial error, refused or
// reset transfer) is still dropped, so "no floor" is not "annotate only" --
// only a whole batch is spared, by the same breaker as the API filters (see
// apiFilter.apply). No store: a speed measurement is far too volatile for the
// host-keyed, month-long geoblock store, so a sub-floor node is dropped for
// this cycle and re-measured next.
type bandwidthFilter struct {
	minMbps int
	check   func(ctx context.Context, proxies []mihomo.Proxy) map[string]BandwidthOutcome
	logger  zerolog.Logger
}

func (f *bandwidthFilter) apply(ctx context.Context, survivors []Survivor, proxies map[string][]mihomo.Proxy) ([]Survivor, FilterReport) {
	rep := FilterReport{Name: bandwidthFilterName, In: len(survivors), Kept: len(survivors), Dropped: map[string]int{}}
	subset := filterSubset(survivors, proxies)
	outcomes := f.check(ctx, subset)
	if ctx.Err() != nil {
		f.logger.Warn().Str("filter", bandwidthFilterName).Msg("filter cancelled; keeping survivors unchanged")
		return survivors, rep
	}

	// Count the batch verdict first, with the same plausibility breaker as the
	// API filters (apiFilter.apply): every download shares one test endpoint
	// and one host uplink, so a batch that ALL failed to download or ALL came
	// in under the floor is the endpoint's mood or a dip on our side as much
	// as a pool of bad nodes -- the slow-drop comment below quotes config.yaml
	// on under-reporting. A disbelieved batch is kept, left untagged (its
	// numbers were measured under the same doubt), and re-measured next cycle.
	slow, unreachable := 0, 0
	for _, s := range survivors {
		switch o := outcomes[s.Label]; {
		case !o.Reachable:
			unreachable++
		case f.minMbps > 0 && o.Mbps < f.minMbps:
			slow++
		}
	}
	if breakerTrips(slow+unreachable, len(survivors)) {
		f.logger.Warn().Str("filter", bandwidthFilterName).Int("survivors", len(survivors)).
			Int(dropSlow, slow).Int(dropUnreachable, unreachable).
			Int("threshold_pct", precheckBreakerPercent).
			Msg("bandwidth check condemned every survivor; treating the batch verdict as the test endpoint's or our uplink's, not the nodes', and keeping all survivors")
		rep.Dropped[dropSlow] = 0
		rep.Dropped[dropUnreachable] = 0
		rep.State = FilterTripped
		return survivors, rep
	}

	kept := make([]Survivor, 0, len(survivors))
	for _, s := range survivors {
		o := outcomes[s.Label]
		switch {
		case !o.Reachable:
			// Never persisted: measure returns unreachable for a refused or
			// reset transfer too, which is the endpoint's mood as much as the
			// node's.
		case f.minMbps > 0 && o.Mbps < f.minMbps:
			// Never persisted either, for a related reason: the concurrent
			// downloads share one host uplink -- config.yaml says outright that
			// this "can under-report fast nodes" -- so a dip on our side puts
			// the whole batch under the floor. Re-measuring next cycle is
			// cheap; carrying the verdict forward would hide good nodes.
		default:
			s.Mbps = o.Mbps
			kept = append(kept, s)
		}
	}
	f.logger.Info().Str("filter", bandwidthFilterName).Int("survivors", len(survivors)).
		Int("kept", len(kept)).Int(dropSlow, slow).Int(dropUnreachable, unreachable).Msg("node filter")
	rep.Kept = len(kept)
	rep.Dropped = map[string]int{dropSlow: slow, dropUnreachable: unreachable}
	rep.State = FilterRan
	return kept, rep
}

// filterSubset flattens the proxies of every survivor into the slice a
// through-node check fans out over. Capacity comes from the proxy count, not
// the survivor count: a mierus:// survivor contributes one proxy per
// configured port, so sizing by survivors would grow-and-copy the slice on
// every filter of every cycle that sees a multi-port node.
func filterSubset(survivors []Survivor, proxies map[string][]mihomo.Proxy) []mihomo.Proxy {
	n := 0
	for _, s := range survivors {
		n += len(proxies[s.Label])
	}
	subset := make([]mihomo.Proxy, 0, n)
	for _, s := range survivors {
		subset = append(subset, proxies[s.Label]...)
	}

	return subset
}

// buildNodeFilters constructs the configured Layer-2 filters in order. Unknown
// names are warned and skipped; the gemini filter needs a prober with Gemini
// support (a resolved API key); the claude, chatgpt, tidal and deepl filters
// are keyless (the first two geo-block before authentication, tidal's
// /v1/country needs no credential, and DeepL answers 451 from the translator
// page a browser loads); the bandwidth filter needs a prober with bandwidth
// support.
func buildNodeFilters(names []string, prober Prober, store Blocklist, logger zerolog.Logger) []NodeFilter {
	var filters []NodeFilter
	for _, n := range names {
		switch n {
		case geminiFilterName:
			gc, ok := prober.(geminiChecker)
			if !ok {
				logger.Warn().Msg("gemini filter requested but prober lacks Gemini support; skipping")
				continue
			}
			// The only gate with an enabled() hook: its credential can go
			// missing at runtime, and a keyless check would pass every node
			// silently. The others cannot be disabled that way.
			filters = append(filters, &apiFilter{
				filterName: geminiFilterName,
				enabled:    gc.GeminiEnabled,
				check:      gc.GeminiCheck,
				store:      store,
				logger:     logger,
			})
		case claudeFilterName:
			cc, ok := prober.(claudeChecker)
			if !ok {
				logger.Warn().Msg("claude filter requested but prober lacks Claude support; skipping")
				continue
			}
			filters = append(filters, &apiFilter{
				filterName: claudeFilterName,
				check:      cc.ClaudeCheck,
				store:      store,
				logger:     logger,
			})
		case chatgptFilterName:
			cg, ok := prober.(chatgptChecker)
			if !ok {
				logger.Warn().Msg("chatgpt filter requested but prober lacks ChatGPT support; skipping")
				continue
			}
			filters = append(filters, &apiFilter{
				filterName: chatgptFilterName,
				check:      cg.ChatGPTCheck,
				store:      store,
				logger:     logger,
			})
		case tidalFilterName:
			td, ok := prober.(tidalChecker)
			if !ok {
				logger.Warn().Msg("tidal filter requested but prober lacks Tidal support; skipping")
				continue
			}
			// No store on purpose: this gate's verdict is a bare status code,
			// a far weaker signal than the explicit refusal markers the AI
			// checks match. The store is keyed by host with no service
			// dimension and lives for its whole TTL, so a transient CDN error
			// or rate-limit would evict the node from every endpoint. With no
			// store nothing outlives the cycle, and the batch breaker in
			// apiFilter.apply keeps a whole batch this status-only verdict
			// would condemn, so a wholesale rate-limit costs nothing at all.
			filters = append(filters, &apiFilter{
				filterName: tidalFilterName,
				check:      td.TidalCheck,
				logger:     logger,
			})
		case bandwidthFilterName:
			filters = appendBandwidthFilter(filters, prober, logger)
		case deeplFilterName:
			dl, ok := prober.(deeplChecker)
			if !ok {
				logger.Warn().Msg("deepl filter requested but prober lacks DeepL support; skipping")
				continue
			}
			// A non-451 failure costs this cycle only; both transports must pass.
			filters = append(filters, &apiFilter{
				filterName:      deeplFilterName,
				requireVerified: true,
				check:           dl.DeeplCheck,
				store:           store,
				logger:          logger,
			})
		default:
			logger.Warn().Str("filter", n).Msg("unknown node filter; skipping")
		}
	}
	return filters
}

// appendBandwidthFilter adds the speed filter when the prober can measure. It
// takes and returns the slice so the capability check lives here rather than as
// another branch inside buildNodeFilters' switch.
func appendBandwidthFilter(dst []NodeFilter, prober Prober, logger zerolog.Logger) []NodeFilter {
	bc, ok := prober.(bandwidthChecker)
	if !ok {
		logger.Warn().Msg("bandwidth filter requested but prober lacks bandwidth support; skipping")

		return dst
	}

	return append(dst, &bandwidthFilter{
		minMbps: bc.BandwidthMinMbps(),
		check:   bc.BandwidthCheck,
		logger:  logger,
	})
}
