package stable

import (
	"bytes"
	"cmp"
	"context"
	"net/netip"
	"slices"
	"strconv"

	"domains.lst/sub-preprocessor/internal/geofeed"
	"domains.lst/sub-preprocessor/internal/ioutil"
	"domains.lst/sub-preprocessor/internal/preprocess"
)

// ProbeStage names how far a node's probe got. The values are ordered by
// progress so a fold can keep the highest: a mierus:// label whose port once
// opened a tunnel must not be reported as a connect failure.
//
// StageUnknown is the zero value, so a ProbeResult built without a stage — a
// test fake, say — reports "unknown" rather than a stage it never observed.
type ProbeStage uint8

const (
	StageUnknown ProbeStage = iota
	// StageCondemned is the reachability pre-check's verdict: the server
	// accepted no TCP connection, so no URL test was spent on it.
	StageCondemned
	// StageConnect merges transport and crypto failures on purpose; see
	// probeStage for why mihomo cannot tell them apart.
	StageConnect
	// StageFetch means the tunnel came up and the GET through it failed.
	StageFetch
	StagePassed
)

func (s ProbeStage) String() string {
	switch s {
	case StageCondemned:
		return "condemned"
	case StageConnect:
		return "connect"
	case StageFetch:
		return "fetch"
	case StagePassed:
		return "passed"
	case StageUnknown:
	}

	return "unknown"
}

// ProbeResult aggregates URL test outcomes for one node across all rounds.
// Entries exist for nodes that never succeeded too, so Successes == 0 — not
// absence from the map — is what marks a node dead (see recordDead).
type ProbeResult struct {
	Successes int
	MeanMs    int
	Stage     ProbeStage
}

// Survivor is an entry that passed selection, with its mean delay, whatever the
// bandwidth filter measured, and whatever the node reported about its own
// egress. A zero Egress means no trace ran or none answered.
type Survivor struct {
	Entry
	MeanMs int
	Mbps   int
	Egress preprocess.Egress
}

// SelectSurvivors keeps entries with at most maxFail failed rounds and mean
// delay within maxAvgMs. Entries absent from res count as fully failed.
// The result is sorted by mean delay ascending (stable).
func SelectSurvivors(entries []Entry, res map[string]ProbeResult, rounds, maxFail, maxAvgMs int) []Survivor {
	out := make([]Survivor, 0, len(entries))
	for _, e := range entries {
		r, ok := res[e.Label]
		if !ok {
			continue
		}
		if rounds-r.Successes > maxFail || r.MeanMs > maxAvgMs {
			continue
		}
		out = append(out, Survivor{Entry: e, MeanMs: r.MeanMs})
	}
	slices.SortStableFunc(out, func(a, b Survivor) int { return cmp.Compare(a.MeanMs, b.MeanMs) })
	return out
}

// BuildPayload renders survivors as a plain URI list, one node per line. This
// is where a published name is finally decided: the tags are built ONCE, here,
// after every probe and through-node filter has had its say.
//
// It fills Survivor.Country as it goes, because this is the only place the GEO
// chain runs and the cycle's kept-country and geo-unknown gauges read that
// field straight afterwards. A nil Annotator means annotation is off: the
// merged line is published verbatim and no chain judged a country.
func BuildPayload(ctx context.Context, ann preprocess.Annotator, survivors []Survivor) []byte {
	total := 0
	for i := range survivors {
		total += len(survivors[i].Raw) + 1
	}
	out := make([]byte, 0, total)
	if ann == nil {
		for i := range survivors {
			out = append(out, survivors[i].Raw...)
			out = append(out, '\n')
		}

		return out
	}

	r := renderer{ann: ann}
	for i := range survivors {
		s := &survivors[i]
		line, country, ok := r.render(ctx, s.Raw, preprocess.AnnotateRequest{
			IP:     s.annotatedIP(),
			Egress: s.Egress,
			Prefix: r.speedPrefix(s.Mbps),
		})
		if !ok {
			// Annotation is best-effort and always has been: a line the parser
			// refuses is published as it stands rather than dropped, and stays
			// uncounted by the country gauges.
			out = append(out, s.Raw...)
			out = append(out, '\n')

			continue
		}
		s.Country = countryString(country)
		out = append(out, line...)
		out = append(out, '\n')
	}

	return out
}

// annotatedIP is the address the tags describe: what the node reported about
// itself when the trace answered, the address the IP-filters judged otherwise.
// The choice is the CALLER's — the annotator is handed one address and has no
// business branching on where it came from.
func (s *Survivor) annotatedIP() netip.Addr {
	if s.Egress.Valid() {
		return s.Egress.IP
	}

	return s.Entry.IP
}

// speedPrefix is the tag the bandwidth filter's measurement earns. It rides in
// front of the configured tags rather than being one of them because the speed
// is measured a whole stage after the annotate chain was built, and only for
// the nodes that got that far.
//
// The bytes stay in the renderer: Annotate copies the prefix into its own
// scratch before returning, so this view never outlives the call reading it.
func (r *renderer) speedPrefix(mbps int) string {
	if mbps <= 0 {
		return ""
	}
	r.prefix = append(r.prefix[:0], "[SPD:"...)
	r.prefix = strconv.AppendInt(r.prefix, int64(mbps), decimalBase)
	r.prefix = append(r.prefix, "M] "...)

	return ioutil.UnsafeString(r.prefix)
}

// countryUnknown is the annotator's marker for "no provider resolved a
// country" ([GEO:??]); it is the preprocess constant so the gauge counting
// [GEO:??] nodes and this comparison can never disagree.
var countryUnknown = preprocess.UnknownCountry

// alphaLetters is the alphabet countryCodes spans.
const alphaLetters = 'Z' - 'A' + 1

// countryCodes holds every AA..ZZ pair back to back so countryString can hand
// out a view of immortal package data.
var countryCodes = func() string {
	var b [alphaLetters * alphaLetters * countryCodeLen]byte
	for i := range alphaLetters * alphaLetters {
		b[countryCodeLen*i] = byte('A' + i/alphaLetters)
		b[countryCodeLen*i+1] = byte('A' + i%alphaLetters)
	}

	return string(b[:])
}()

// countryString renders the GEO chain's verdict into Entry.Country.
//
// The chain hands back a geofeed.CountryCode VALUE rather than a slice of
// whatever body resolved it, and that is deliberate: Entry.Country outlives the
// cycle inside the metrics snapshot, where it becomes a Prometheus label, so a
// 2-byte view would pin its whole root until the next publication and a code
// that is not two letters would break the entire scrape.
//
// The codes come from one immortal table, so a published node allocates nothing
// here and the label a scrape retains points at package data, not at a root.
func countryString(c geofeed.CountryCode) string {
	if c == (geofeed.CountryCode{}) {
		return countryUnknown
	}
	hi, lo := int(c[0])-'A', int(c[1])-'A'
	if hi < 0 || hi >= alphaLetters || lo < 0 || lo >= alphaLetters {
		return c.String()
	}
	i := countryCodeLen * (hi*alphaLetters + lo)

	return countryCodes[i : i+countryCodeLen]
}

// renderer annotates node lines, holding its buffers across a whole survivor
// set so one publication costs a bounded number of allocations rather than one
// set per node.
type renderer struct {
	ann     preprocess.Annotator
	dst     bytes.Buffer
	scratch bytes.Buffer
	line    []byte
	prefix  []byte
}

// render returns the annotated line for raw plus the country the GEO chain
// resolved. The bytes alias the renderer and are valid only until the next
// call. ok is false when the line does not parse — the only case the annotator
// cannot be asked about, since it falls back to the raw node itself for a
// scheme it cannot rewrite.
func (r *renderer) render(
	ctx context.Context, raw string, req preprocess.AnnotateRequest,
) ([]byte, geofeed.CountryCode, bool) {
	r.line = append(r.line[:0], raw...)
	n, ok := parseOne(r.line)
	if !ok {
		return nil, geofeed.CountryCode{}, false
	}
	req.Node = n
	// Annotate appends; the buffer is ours, so this reset is what makes the
	// result exactly one node's line.
	r.dst.Reset()
	country := r.ann.Annotate(ctx, &r.dst, &r.scratch, req)

	return r.dst.Bytes(), country, true
}
