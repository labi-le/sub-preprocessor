package stable

import (
	"cmp"
	"slices"
)

// ProbeResult aggregates URL test outcomes for one node across all rounds.
type ProbeResult struct {
	Successes int
	MeanMs    int
}

// Survivor is an entry that passed selection, with its mean delay.
type Survivor struct {
	Entry
	MeanMs int
}

// SelectSurvivors keeps entries with at most maxFail failed rounds and mean
// delay within maxAvgMs. Entries absent from res count as fully failed.
// The result is sorted by mean delay ascending (stable).
func SelectSurvivors(entries []Entry, res map[string]ProbeResult, rounds, maxFail, maxAvgMs int) []Survivor {
	var out []Survivor
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

// BuildPayload renders survivors as a plain URI list, one node per line.
func BuildPayload(survivors []Survivor) []byte {
	var b []byte
	for _, s := range survivors {
		b = append(b, s.Tagged...)
		b = append(b, '\n')
	}
	return b
}
