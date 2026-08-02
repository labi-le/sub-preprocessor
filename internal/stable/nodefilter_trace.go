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
// how many tags it could correct, which is the number worth watching: a high
// share means the offline chain was mostly wrong about the pool. The count of
// unanswered traces is the separate figure that bounds it — those nodes keep
// the offline guess, so a correction share is only as trustworthy as the share
// that answered. Both ride FilterReport.Notes, never Dropped, since nothing
// here is dropped; see the note names in nodefilter.go.
type geotraceFilter struct {
	check    func(context.Context, []mihomo.Proxy) map[string]TraceResult
	annotate bool
	logger   zerolog.Logger
}

func (f *geotraceFilter) apply(
	ctx context.Context, survivors []Survivor, proxies map[string][]mihomo.Proxy,
) ([]Survivor, FilterReport) {
	rep := FilterReport{
		Name: geotraceFilterName, In: len(survivors), Kept: len(survivors),
		Dropped: map[string]int{}, Notes: map[string]int{},
	}
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
		after, geoMoved := retagTraced(before, res)
		if after == before {
			continue
		}
		survivors[i].Entry.Tagged = after
		retagged++
		if !geoMoved {
			continue
		}
		// Entry.Country is read AFTER the filter pass (checker's kept-country
		// and geo-unknown gauges), so leaving it on the pre-trace value would
		// publish [GEO:DE] while counting the node as CA. No re-validation:
		// parseTrace admits only two ASCII letters, which is exactly what
		// tagCountry would have extracted. Cloned for tagCountry's other
		// reason — the value is a sub-slice of the node's trace response body,
		// bounded only by apiProbeOne's 64 KiB cap, and Entry.Country outlives
		// the cycle inside the metrics snapshot.
		survivors[i].Entry.Country = strings.Clone(res.Country)
		corrected++
	}
	rep.Notes[noteUnanswered] = unanswered
	rep.Notes[noteCorrected] = corrected
	f.logger.Info().Int("survivors", len(survivors)).Int("retagged", retagged).
		Int("corrected", corrected).Int("unanswered", unanswered).
		Str("filter", geotraceFilterName).Msg("node filter")

	return survivors, rep
}

// retagTraced rewrites the GEO and IP values inside a published node's name and
// reports whether the GEO value actually moved — the count this filter exists
// to publish, which a substring search on the whole line cannot answer because
// vmess and ssr keep the name inside their base64 payload.
//
// It only substitutes tags that are already there: annotation is the operator's
// choice, so a node published without tags stays that way. Any other tag (SPD)
// keeps its place, and on a parse failure the line is returned unchanged —
// annotation is best-effort, never fatal.
func retagTraced(line string, res TraceResult) (string, bool) {
	out, moved, found := line, false, false
	subscription.Parse([]byte(line), func(n subscription.Node) bool {
		// LeadingTags trims the leading blank run off its own result, so it is
		// a literal prefix only of the already-trimmed name.
		name := strings.TrimLeft(n.Name, blankTagChars)
		tags := rewrite.LeadingTags(name)
		if tags == "" {
			return false
		}
		swapped, geoMoved := swapTagValues(tags, res)
		if swapped == tags {
			return false
		}
		if relabeled, ok := relabelNode(n, swapped+strings.TrimPrefix(name, tags)); ok {
			out, moved, found = relabeled, geoMoved, true
		}

		return false
	})
	if !found {
		return line, false
	}

	return out, moved
}

// blankTagChars are the bytes rewrite.LeadingTags skips between tags. Not
// unicode.IsSpace: its scan tests these two literally, and swapTagValues has to
// consume exactly the same run to reproduce its input byte for byte.
const blankTagChars = " \t"

// swapTagValues substitutes the GEO and IP values in a run of [KEY:VALUE] tags,
// leaving every other tag, the order, and the blanks between tags untouched.
// The blanks are not cosmetic: bandwidth runs before geotrace and annotateSpeed
// prepends "[SPD:20M] ", so every survivor in the configured chain arrives with
// a gap in the middle of its tag run. It also reports whether a GEO tag was
// present AND carried a different country than res.
func swapTagValues(tags string, res TraceResult) (string, bool) {
	var b strings.Builder
	b.Grow(len(tags))
	geoMoved := false
	for len(tags) > 0 {
		rest := strings.TrimLeft(tags, blankTagChars)
		b.WriteString(tags[:len(tags)-len(rest)])
		tags = rest
		end := strings.IndexByte(tags, ']')
		if !strings.HasPrefix(tags, "[") || end < 0 {
			b.WriteString(tags)

			break
		}
		key, value, hasValue := strings.Cut(tags[1:end], ":")
		switch {
		case hasValue && key == config.TagGEO:
			geoMoved = geoMoved || value != res.Country
			b.WriteString("[" + config.TagGEO + ":" + res.Country + "]")
		case hasValue && key == config.TagIP:
			b.WriteString("[" + config.TagIP + ":" + res.IP + "]")
		default:
			b.WriteString(tags[:end+1])
		}
		tags = tags[end+1:]
	}

	return b.String(), geoMoved
}
