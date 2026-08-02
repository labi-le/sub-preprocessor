package stable

import (
	"context"

	mihomo "github.com/metacubex/mihomo/constant"
	"github.com/rs/zerolog"
)

// NodeFilter is a through-node check the worker runs on latency-probe
// survivors: unlike the preprocess IP-filters it routes traffic THROUGH each
// proxy node, so it lives here and only affects /stable.txt. The through-node
// entries of the unified filters list (gemini/claude/chatgpt/tidal/bandwidth)
// select which run.
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
// gemini/claude/chatgpt, a refused request for tidal. A nil enabled func means
// the check is always active (e.g. Anthropic geo-blocks before authentication,
// so its check is keyless).
type apiFilter struct {
	filterName string
	enabled    func() bool
	check      func(ctx context.Context, proxies []mihomo.Proxy) map[string]APIOutcome
	store      Blocklist
	logger     zerolog.Logger
}

func (f *apiFilter) apply(ctx context.Context, survivors []Survivor, proxies map[string][]mihomo.Proxy) ([]Survivor, FilterReport) {
	rep := FilterReport{Name: f.filterName, In: len(survivors), Kept: len(survivors), Dropped: map[string]int{}}
	if f.enabled != nil && !f.enabled() {
		f.logger.Warn().Str("filter", f.filterName).Msg("filter configured but disabled; skipping")
		return survivors, rep
	}

	subset := filterSubset(survivors, proxies)
	outcomes := f.check(ctx, subset)
	if outcomes == nil {
		f.logger.Warn().Str("filter", f.filterName).Msg("filter skipped: no outcomes")
		return survivors, rep
	}
	if ctx.Err() != nil {
		// A cancelled check yields partial outcomes; don't record blocks or
		// drop survivors based on them.
		f.logger.Warn().Str("filter", f.filterName).Msg("filter cancelled; keeping survivors unchanged")
		return survivors, rep
	}

	kept := make([]Survivor, 0, len(survivors))
	var blocked, unreachable int
	for _, s := range survivors {
		o := outcomes[s.Label]
		switch {
		case o.Blocked:
			blocked++
			// Persisted only when this check is trusted enough to act on the
			// verdict beyond this cycle, which is exactly what a non-nil store
			// means. tidal is the one exception and says why at construction:
			// its verdict is a bare status code, so a single 429 from
			// api.tidal.com marks the whole batch Blocked.
			if f.store != nil {
				if err := f.store.Block(o.Server); err != nil {
					f.logger.Warn().Err(err).Str("host", o.Server).Msg("geoblock write failed")
				}
			}
		case !o.Reachable:
			// Never persisted: a node that answered the latency probe but not
			// this endpoint is as likely to be a transient path failure as a
			// standing one, and the next cycle is cheap enough to find out.
			unreachable++
		default:
			kept = append(kept, s)
		}
	}
	f.logger.Info().Str("filter", f.filterName).Int("survivors", len(survivors)).Int("kept", len(kept)).
		Int(dropBlocked, blocked).Int(dropUnreachable, unreachable).Msg("node filter")
	rep.Kept = len(kept)
	rep.Dropped = map[string]int{dropBlocked: blocked, dropUnreachable: unreachable}
	return kept, rep
}

// bandwidthFilter keeps only survivors whose measured through-node download
// speed is at least minMbps (minMbps==0 disables the floor and keeps all
// reachable nodes) and records Mbps on each kept survivor, which the
// publication turns into the [SPD:] tag. No store: a speed measurement is far
// too volatile for the host-keyed, month-long geoblock store, so a sub-floor
// node is dropped for this cycle and re-measured next.
type bandwidthFilter struct {
	minMbps int
	check   func(ctx context.Context, proxies []mihomo.Proxy) map[string]BandwidthOutcome
	logger  zerolog.Logger
}

func (f *bandwidthFilter) apply(ctx context.Context, survivors []Survivor, proxies map[string][]mihomo.Proxy) ([]Survivor, FilterReport) {
	rep := FilterReport{Name: bandwidthFilterName, In: len(survivors), Kept: len(survivors), Dropped: map[string]int{}}
	subset := filterSubset(survivors, proxies)
	outcomes := f.check(ctx, subset)
	if outcomes == nil {
		f.logger.Warn().Str("filter", bandwidthFilterName).Msg("filter skipped: no outcomes")
		return survivors, rep
	}
	if ctx.Err() != nil {
		f.logger.Warn().Str("filter", bandwidthFilterName).Msg("filter cancelled; keeping survivors unchanged")
		return survivors, rep
	}

	kept := make([]Survivor, 0, len(survivors))
	var slow, unreachable int
	for _, s := range survivors {
		o := outcomes[s.Label]
		switch {
		case !o.Reachable:
			// Never persisted: measure returns unreachable for a refused or
			// reset transfer too, which is the endpoint's mood as much as the
			// node's.
			unreachable++
		case f.minMbps > 0 && o.Mbps < f.minMbps:
			// Never persisted either, for a related reason: the concurrent
			// downloads share one host uplink -- config.yaml says outright that
			// this "can under-report fast nodes" -- so a dip on our side puts
			// the whole batch under the floor. Re-measuring next cycle is
			// cheap; carrying the verdict forward would hide good nodes.
			slow++
		default:
			s.Mbps = o.Mbps
			kept = append(kept, s)
		}
	}
	f.logger.Info().Str("filter", bandwidthFilterName).Int("survivors", len(survivors)).
		Int("kept", len(kept)).Int(dropSlow, slow).Int(dropUnreachable, unreachable).Msg("node filter")
	rep.Kept = len(kept)
	rep.Dropped = map[string]int{dropSlow: slow, dropUnreachable: unreachable}
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
// support (a resolved API key); the claude, chatgpt and tidal filters are
// keyless (the first two geo-block before authentication, tidal's /v1/country
// needs no credential); the bandwidth filter needs a prober with bandwidth
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
			// store nothing outlives the cycle, so a rate-limited batch costs
			// this one cycle.
			filters = append(filters, &apiFilter{
				filterName: tidalFilterName,
				check:      td.TidalCheck,
				logger:     logger,
			})
		case bandwidthFilterName:
			filters = appendBandwidthFilter(filters, prober, logger)
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
