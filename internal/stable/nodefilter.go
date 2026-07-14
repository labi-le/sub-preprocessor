package stable

import (
	"context"

	"github.com/rs/zerolog"
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

// Configuration names of the through-node API filters.
const (
	geminiFilterName = "gemini"
	claudeFilterName = "claude"
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
