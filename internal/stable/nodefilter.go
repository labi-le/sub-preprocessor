package stable

import (
	"context"
	"strconv"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/subscription"
)

// NodeFilter is a Layer-2 check applied in the worker after the IP-filter
// pipeline (workflow.stages) and the latency probe. Unlike the preprocess
// IP-filters it routes traffic THROUGH each surviving proxy node, so it lives
// in the worker and only affects /stable.txt. Filters are selected by name via
// subscriptions.check.filters. The methods are unexported so the set is sealed
// to this package.
type NodeFilter interface {
	name() string
	apply(ctx context.Context, survivors []Survivor) []Survivor
}

// geminiChecker is the through-node Gemini capability of a Prober.
type geminiChecker interface {
	GeminiEnabled() bool
	GeminiCheck(ctx context.Context, payload []byte) map[string]APIOutcome
}

// claudeChecker is the through-node Anthropic capability of a Prober.
type claudeChecker interface {
	ClaudeCheck(ctx context.Context, payload []byte) map[string]APIOutcome
}

// bandwidthChecker is the through-node download-speed capability of a Prober.
type bandwidthChecker interface {
	BandwidthCheck(ctx context.Context, payload []byte) map[string]BandwidthOutcome
	BandwidthMinMbps() int
}

// Configuration names of the through-node API filters.
const (
	geminiFilterName    = "gemini"
	claudeFilterName    = "claude"
	bandwidthFilterName = "bandwidth"
)

// apiFilter keeps only survivors that can reach an API endpoint through their
// own node, and records geo-blocked node hosts in the store (TTL) so later
// cycles skip them before probing. A survivor is kept only when the
// through-node API GET returns a body without the geo-block marker. A nil
// enabled func means the check is always active (e.g. Anthropic geo-blocks
// before authentication, so its check is keyless).
type apiFilter struct {
	filterName string
	enabled    func() bool
	check      func(ctx context.Context, payload []byte) map[string]APIOutcome
	store      Blocklist
	logger     zerolog.Logger
}

func (f *apiFilter) name() string { return f.filterName }

func (f *apiFilter) apply(ctx context.Context, survivors []Survivor) []Survivor {
	if f.enabled != nil && !f.enabled() {
		f.logger.Warn().Str("filter", f.filterName).Msg("filter configured but disabled; skipping")
		return survivors
	}

	subset := make([]Entry, 0, len(survivors))
	for _, s := range survivors {
		subset = append(subset, s.Entry)
	}
	outcomes := f.check(ctx, entriesPayload(subset))
	if outcomes == nil {
		f.logger.Warn().Str("filter", f.filterName).Msg("filter skipped: no outcomes")
		return survivors
	}
	if ctx.Err() != nil {
		// A cancelled check yields partial outcomes; don't record blocks or
		// drop survivors based on them.
		f.logger.Warn().Str("filter", f.filterName).Msg("filter cancelled; keeping survivors unchanged")
		return survivors
	}

	kept := make([]Survivor, 0, len(survivors))
	var blocked, unreachable int
	for _, s := range survivors {
		o := outcomes[s.Label]
		switch {
		case o.Blocked:
			blocked++
			if f.store != nil {
				if err := f.store.Block(o.Server); err != nil {
					f.logger.Warn().Err(err).Str("host", o.Server).Msg("geoblock write failed")
				}
			}
		case !o.Reachable:
			unreachable++
		default:
			kept = append(kept, s)
		}
	}
	f.logger.Info().Str("filter", f.filterName).Int("survivors", len(survivors)).Int("kept", len(kept)).
		Int("blocked", blocked).Int("unreachable", unreachable).Msg("node filter")
	return kept
}

// bandwidthFilter keeps only survivors whose measured through-node download
// speed is at least minMbps (minMbps==0 disables the floor and keeps all
// reachable nodes). It records Mbps on each kept survivor and, when annotate is
// set, prepends a [SPD:<n>M] tag to the published name via the vmess-aware
// relabel path. No store: bandwidth results are never persisted.
type bandwidthFilter struct {
	minMbps  int
	annotate bool
	check    func(ctx context.Context, payload []byte) map[string]BandwidthOutcome
	logger   zerolog.Logger
}

func (f *bandwidthFilter) name() string { return bandwidthFilterName }

func (f *bandwidthFilter) apply(ctx context.Context, survivors []Survivor) []Survivor {
	subset := make([]Entry, 0, len(survivors))
	for _, s := range survivors {
		subset = append(subset, s.Entry)
	}
	outcomes := f.check(ctx, entriesPayload(subset))
	if outcomes == nil {
		f.logger.Warn().Str("filter", bandwidthFilterName).Msg("filter skipped: no outcomes")
		return survivors
	}
	if ctx.Err() != nil {
		f.logger.Warn().Str("filter", bandwidthFilterName).Msg("filter cancelled; keeping survivors unchanged")
		return survivors
	}

	kept := make([]Survivor, 0, len(survivors))
	var slow, unreachable int
	for _, s := range survivors {
		o := outcomes[s.Label]
		switch {
		case !o.Reachable:
			unreachable++
		case f.minMbps > 0 && o.Mbps < f.minMbps:
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
	return kept
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
// support (a resolved API key); the claude filter is keyless.
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
		default:
			logger.Warn().Str("filter", n).Msg("unknown node filter; skipping")
		}
	}
	return filters
}
