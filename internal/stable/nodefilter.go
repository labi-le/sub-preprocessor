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
	apply(ctx context.Context, entries []Entry, survivors []Survivor) []Survivor
}

// geminiChecker is the through-node Gemini capability of a Prober.
type geminiChecker interface {
	GeminiEnabled() bool
	GeminiCheck(ctx context.Context, payload []byte) map[string]GeminiOutcome
}

// geminiFilter keeps only survivors that can reach the Gemini API through their
// own node, and records geo-blocked node hosts in the store (TTL) so later
// cycles skip them before probing. A survivor is kept only when the through-node
// API GET returns a body without the geo-block marker.
type geminiFilter struct {
	prober geminiChecker
	store  Blocklist
	logger zerolog.Logger
}

func (g *geminiFilter) name() string { return "gemini" }

func (g *geminiFilter) apply(ctx context.Context, _ []Entry, survivors []Survivor) []Survivor {
	if !g.prober.GeminiEnabled() {
		g.logger.Warn().Msg("gemini filter configured but no API key; skipping")
		return survivors
	}

	subset := make([]Entry, 0, len(survivors))
	for _, s := range survivors {
		subset = append(subset, s.Entry)
	}
	outcomes := g.prober.GeminiCheck(ctx, entriesPayload(subset))
	if outcomes == nil {
		g.logger.Warn().Msg("gemini filter skipped: no outcomes")
		return survivors
	}

	kept := make([]Survivor, 0, len(survivors))
	var blocked, unreachable int
	for _, s := range survivors {
		o := outcomes[s.Label]
		switch {
		case o.Blocked:
			blocked++
			if g.store != nil {
				if err := g.store.Block(o.Server); err != nil {
					g.logger.Warn().Err(err).Str("host", o.Server).Msg("geoblock write failed")
				}
			}
		case !o.Reachable:
			unreachable++
		default:
			kept = append(kept, s)
		}
	}
	g.logger.Info().Int("survivors", len(survivors)).Int("kept", len(kept)).
		Int("gemini_blocked", blocked).Int("gemini_unreachable", unreachable).Msg("gemini filter")
	return kept
}

// buildNodeFilters constructs the configured Layer-2 filters in order. Unknown
// names are warned and skipped; the gemini filter needs a prober with Gemini
// support (a resolved API key).
func buildNodeFilters(names []string, prober Prober, store Blocklist, logger zerolog.Logger) []NodeFilter {
	var filters []NodeFilter
	for _, n := range names {
		switch n {
		case "gemini":
			gc, ok := prober.(geminiChecker)
			if !ok {
				logger.Warn().Msg("gemini filter requested but prober lacks Gemini support; skipping")
				continue
			}
			filters = append(filters, &geminiFilter{prober: gc, store: store, logger: logger})
		default:
			logger.Warn().Str("filter", n).Msg("unknown node filter; skipping")
		}
	}
	return filters
}
