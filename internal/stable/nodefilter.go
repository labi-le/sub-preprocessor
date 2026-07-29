package stable

import (
	"context"
	"strconv"

	mihomo "github.com/metacubex/mihomo/constant"
	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/subscription"
)

// NodeFilter is a through-node check the worker runs on latency-probe
// survivors: unlike the preprocess IP-filters it routes traffic THROUGH each
// proxy node, so it lives here and only affects /stable.txt. The through-node
// entries of the unified filters list (gemini/claude/chatgpt/tidal/bandwidth)
// select which run.
type NodeFilter interface {
	name() string
	// active reports whether this filter will actually test anything. A
	// configured-but-disabled filter drops nobody, so the checker also refuses
	// to let its remembered rejects suppress nodes.
	active() bool
	// apply narrows survivors using the shared, pre-parsed proxies keyed by
	// node label. The checker owns the proxies' lifecycle; filters only read.
	//
	// rejected lists the Entry.Addr of survivors dropped on a verdict stable
	// enough to be worth remembering for a few cycles — a service refusal, a
	// speed below the floor. Transient outcomes (unreachable, cancelled,
	// no outcomes at all) MUST NOT appear there. Where those addresses are
	// remembered, and for how long, is the checker's decision, not the
	// filter's: a filter that also persists to the long-lived geoblock store
	// does so through its own store field and says so at construction.
	apply(ctx context.Context, survivors []Survivor, proxies map[string]mihomo.Proxy) (kept []Survivor, rejected []string, rep FilterReport)
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

// apiFilter keeps only survivors that pass a through-node API check, and records
// geo-blocked node hosts in the store (TTL) so later cycles skip them before
// probing. A nil store (see the tidal filter) skips that persistence AND the
// short-lived reject the checker caches: both mean "this verdict is solid
// enough to act on beyond this cycle", so one flag decides both. What counts as
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

func (f *apiFilter) name() string { return f.filterName }

func (f *apiFilter) active() bool { return f.enabled == nil || f.enabled() }

func (f *apiFilter) apply(ctx context.Context, survivors []Survivor, proxies map[string]mihomo.Proxy) ([]Survivor, []string, FilterReport) {
	rep := FilterReport{Name: f.filterName, In: len(survivors), Kept: len(survivors), Dropped: map[string]int{}}
	if !f.active() {
		f.logger.Warn().Str("filter", f.filterName).Msg("filter configured but disabled; skipping")
		return survivors, nil, rep
	}

	subset := make([]mihomo.Proxy, 0, len(survivors))
	for _, s := range survivors {
		if px, ok := proxies[s.Label]; ok {
			subset = append(subset, px)
		}
	}
	outcomes := f.check(ctx, subset)
	if outcomes == nil {
		f.logger.Warn().Str("filter", f.filterName).Msg("filter skipped: no outcomes")
		return survivors, nil, rep
	}
	if ctx.Err() != nil {
		// A cancelled check yields partial outcomes; don't record blocks or
		// drop survivors based on them.
		f.logger.Warn().Str("filter", f.filterName).Msg("filter cancelled; keeping survivors unchanged")
		return survivors, nil, rep
	}

	kept := make([]Survivor, 0, len(survivors))
	var rejected []string
	var blocked, unreachable int
	for _, s := range survivors {
		o := outcomes[s.Label]
		switch {
		case o.Blocked:
			blocked++
			// Cached only when this check is trusted enough to persist a
			// verdict at all, which is exactly what a non-nil store means. The
			// weak checks are weak in the same way for both: tidal's verdict is
			// a bare status code, so a 429 from api.tidal.com marks the whole
			// batch Blocked -- and since filterDead consults the reject cache
			// BEFORE the probe, caching that would drop every one of those
			// nodes from /stable.txt for the cache's whole TTL instead of for
			// one cycle. One rule, one place: no store, no memory.
			if f.store != nil {
				rejected = append(rejected, s.Addr)
				if err := f.store.Block(o.Server); err != nil {
					f.logger.Warn().Err(err).Str("host", o.Server).Msg("geoblock write failed")
				}
			}
		case !o.Reachable:
			// Not cached: a node that answered the latency probe but not this
			// endpoint is as likely to be a transient path failure as a
			// standing one, and the next cycle is cheap enough to find out.
			unreachable++
		default:
			kept = append(kept, s)
		}
	}
	f.logger.Info().Str("filter", f.filterName).Int("survivors", len(survivors)).Int("kept", len(kept)).
		Int("blocked", blocked).Int("unreachable", unreachable).Msg("node filter")
	rep.Kept = len(kept)
	rep.Dropped = map[string]int{"blocked": blocked, "unreachable": unreachable}
	return kept, rejected, rep
}

// bandwidthFilter keeps only survivors whose measured through-node download
// speed is at least minMbps (minMbps==0 disables the floor and keeps all
// reachable nodes). It records Mbps on each kept survivor and, when annotate is
// set, prepends a [SPD:<n>M] tag to the published name via the vmess-aware
// relabel path. No store: a speed measurement is far too volatile for the
// host-keyed, month-long geoblock store, so a sub-floor node is only returned
// to the checker as a short-lived reject.
type bandwidthFilter struct {
	minMbps  int
	annotate bool
	check    func(ctx context.Context, proxies []mihomo.Proxy) map[string]BandwidthOutcome
	logger   zerolog.Logger
}

func (f *bandwidthFilter) name() string { return bandwidthFilterName }

// active is unconditional: the bandwidth check needs no credential, and a zero
// minMbps still measures every node for the [SPD:] annotation.
func (f *bandwidthFilter) active() bool { return true }

func (f *bandwidthFilter) apply(ctx context.Context, survivors []Survivor, proxies map[string]mihomo.Proxy) ([]Survivor, []string, FilterReport) {
	rep := FilterReport{Name: bandwidthFilterName, In: len(survivors), Kept: len(survivors), Dropped: map[string]int{}}
	subset := make([]mihomo.Proxy, 0, len(survivors))
	for _, s := range survivors {
		if px, ok := proxies[s.Label]; ok {
			subset = append(subset, px)
		}
	}
	outcomes := f.check(ctx, subset)
	if outcomes == nil {
		f.logger.Warn().Str("filter", bandwidthFilterName).Msg("filter skipped: no outcomes")
		return survivors, nil, rep
	}
	if ctx.Err() != nil {
		f.logger.Warn().Str("filter", bandwidthFilterName).Msg("filter cancelled; keeping survivors unchanged")
		return survivors, nil, rep
	}

	kept := make([]Survivor, 0, len(survivors))
	var slow, unreachable int
	for _, s := range survivors {
		o := outcomes[s.Label]
		switch {
		case !o.Reachable:
			// Not cached: measure returns unreachable for a refused or reset
			// transfer too, which is the endpoint's mood as much as the node's.
			unreachable++
		case f.minMbps > 0 && o.Mbps < f.minMbps:
			// Not cached either, for the same reason as unreachable above: the
			// concurrent downloads share one host uplink -- config.yaml says
			// outright that this "can under-report fast nodes" -- so a dip on
			// our side puts the whole batch under the floor. Remembering that
			// would hide those nodes for the cache's TTL rather than re-measure
			// them next cycle, which is cheap.
			slow++
		default:
			s.Mbps = o.Mbps
			if f.annotate {
				s.Tagged = annotateSpeed(s.Tagged, o.Mbps)
			}
			kept = append(kept, s)
		}
	}
	f.logger.Info().Str("filter", bandwidthFilterName).Int("survivors", len(survivors)).
		Int("kept", len(kept)).Int("slow", slow).Int("unreachable", unreachable).Msg("node filter")
	rep.Kept = len(kept)
	rep.Dropped = map[string]int{"slow": slow, "unreachable": unreachable}
	// Never any reject: neither drop reason is stable enough to remember, and
	// both say so above.
	return kept, nil, rep
}

// annotateSpeed prepends [SPD:<mbps>M] to a node's published name. It re-parses
// the line and relabels through relabelNode so vmess (base64 ps) and URI
// (#fragment) nodes are both handled; on any parse failure the line is returned
// unchanged (annotation is best-effort, never fatal).
func annotateSpeed(line string, mbps int) string {
	var out string
	found := false
	subscription.Parse([]byte(line), func(n subscription.Node) bool {
		if relabeled, ok := relabelNode(n, "[SPD:"+strconv.Itoa(mbps)+"M] "+n.Name); ok {
			out = relabeled
			found = true
		}
		return false
	})
	if !found {
		return line
	}
	return out
}

// buildNodeFilters constructs the configured Layer-2 filters in order. Unknown
// names are warned and skipped; the gemini filter needs a prober with Gemini
// support (a resolved API key); the claude, chatgpt and tidal filters are
// keyless (the first two geo-block before authentication, tidal's /v1/country
// needs no credential); the bandwidth filter needs a prober with bandwidth
// support.
func buildNodeFilters(names []string, prober Prober, store Blocklist, annotate bool, logger zerolog.Logger) []NodeFilter {
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
			// or rate-limit would evict the node from every endpoint. Having no
			// store also means no remembered reject (apply seeds the reject
			// cache only for store-backed checks), so a rate-limited batch
			// costs this one cycle instead of the cache's whole TTL.
			filters = append(filters, &apiFilter{
				filterName: tidalFilterName,
				check:      td.TidalCheck,
				logger:     logger,
			})
		case bandwidthFilterName:
			bc, ok := prober.(bandwidthChecker)
			if !ok {
				logger.Warn().Msg("bandwidth filter requested but prober lacks bandwidth support; skipping")
				continue
			}
			filters = append(filters, &bandwidthFilter{
				minMbps:  bc.BandwidthMinMbps(),
				annotate: annotate,
				check:    bc.BandwidthCheck,
				logger:   logger,
			})
		default:
			logger.Warn().Str("filter", n).Msg("unknown node filter; skipping")
		}
	}
	return filters
}
