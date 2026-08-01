package stable

import (
	"context"
	"strings"

	mihomo "github.com/metacubex/mihomo/constant"
	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/rewrite"
	"domains.lst/sub-preprocessor/internal/subscription"
)

const geotraceFilterName = "geotrace"

// geotraceFilter replaces the GEO and IP tags with what the node itself
// reports, and drops nothing: every survivor reaching it has already earned its
// place, and a node whose trace fails keeps the offline chain's guess rather
// than losing its label.
//
// It is a NodeFilter only to inherit the plumbing — ordering from the `filters`
// list, the shared parsed-proxy set, and per-filter metrics. Its report counts
// how many tags it could correct, which is the number worth watching: a low
// share means most nodes are answering, a high one means the offline chain was
// mostly wrong.
type geotraceFilter struct {
	check    func(context.Context, []mihomo.Proxy) map[string]TraceResult
	annotate bool
	logger   zerolog.Logger
}

func (f *geotraceFilter) apply(
	ctx context.Context, survivors []Survivor, proxies map[string][]mihomo.Proxy,
) ([]Survivor, FilterReport) {
	rep := FilterReport{Name: geotraceFilterName, In: len(survivors), Kept: len(survivors), Dropped: map[string]int{}}
	if !f.annotate {
		return survivors, rep
	}

	traced := f.check(ctx, filterSubset(survivors, proxies))
	retagged, corrected, unanswered := 0, 0, 0
	for i := range survivors {
		res, ok := traced[survivors[i].Entry.Label]
		if !ok {
			unanswered++

			continue
		}
		before := survivors[i].Entry.Tagged
		after := retagTraced(before, res)
		if after == before {
			continue
		}
		survivors[i].Entry.Tagged = after
		retagged++
		if !strings.Contains(before, "[GEO:"+res.Country+"]") {
			corrected++
		}
	}
	// Not booked as drops: nothing was dropped. The reasons map is the only
	// per-filter channel metrics expose, so the counts ride there.
	rep.Dropped["unanswered"] = unanswered
	rep.Dropped["corrected"] = corrected
	f.logger.Info().Int("survivors", len(survivors)).Int("retagged", retagged).
		Int("corrected", corrected).Int("unanswered", unanswered).
		Str("filter", geotraceFilterName).Msg("node filter")

	return survivors, rep
}

// retagTraced rewrites the GEO and IP values inside a published node's name.
// It only substitutes tags that are already there: annotation is the operator's
// choice, so a node published without tags stays that way. Any other tag (SPD)
// keeps its place, and on a parse failure the line is returned unchanged —
// annotation is best-effort, never fatal.
func retagTraced(line string, res TraceResult) string {
	out, found := line, false
	subscription.Parse([]byte(line), func(n subscription.Node) bool {
		tags := rewrite.LeadingTags(n.Name)
		if tags == "" {
			return false
		}
		swapped := swapTagValues(tags, res)
		if swapped == tags {
			return false
		}
		if relabeled, ok := relabelNode(n, swapped+strings.TrimPrefix(n.Name, tags)); ok {
			out, found = relabeled, true
		}

		return false
	})
	if !found {
		return line
	}

	return out
}

// swapTagValues substitutes the GEO and IP values in a run of [KEY:VALUE] tags,
// leaving every other tag and the order untouched.
func swapTagValues(tags string, res TraceResult) string {
	var b strings.Builder
	b.Grow(len(tags))
	for len(tags) > 0 {
		end := strings.IndexByte(tags, ']')
		if !strings.HasPrefix(tags, "[") || end < 0 {
			b.WriteString(tags)

			break
		}
		key, _, hasValue := strings.Cut(tags[1:end], ":")
		switch {
		case hasValue && key == config.TagGEO:
			b.WriteString("[" + config.TagGEO + ":" + res.Country + "]")
		case hasValue && key == config.TagIP:
			b.WriteString("[" + config.TagIP + ":" + res.IP + "]")
		default:
			b.WriteString(tags[:end+1])
		}
		tags = tags[end+1:]
	}

	return b.String()
}
