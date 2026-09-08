package ghfind

// Cover picks the indices of the sets that greedily maximise the union of the
// hashes they cover, best first. Each set must be sorted and deduplicated, as
// Endpoints returns; sets are never copied — gains are recounted against the
// running union each round, whose map is sized once from the total input
// length. Picking stops when the best remaining gain is below floor, when
// accept sets have been picked, or when nothing is left to add; equal gains
// resolve to the lower index, so a run is reproducible.
func Cover(sets [][]uint64, floor, accept int) []int {
	if accept <= 0 || len(sets) == 0 {
		return nil
	}
	total := 0
	for _, s := range sets {
		total += len(s)
	}
	union := make(map[uint64]struct{}, total)
	chosen := make([]bool, len(sets))
	picks := make([]int, 0, min(accept, len(sets)))
	for len(picks) < accept {
		best, bestGain := -1, 0
		for i, s := range sets {
			if chosen[i] {
				continue
			}
			if gain := missing(s, union); gain > bestGain {
				best, bestGain = i, gain
			}
		}
		if best < 0 || bestGain == 0 || bestGain < floor {
			break
		}
		chosen[best] = true
		picks = append(picks, best)
		for _, h := range sets[best] {
			union[h] = struct{}{}
		}
	}
	return picks
}

func missing(set []uint64, union map[uint64]struct{}) int {
	n := 0
	for _, h := range set {
		if _, ok := union[h]; !ok {
			n++
		}
	}
	return n
}
