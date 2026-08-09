// Package cidrset answers IPv4 membership against an operator allow-list — a
// ~30k-line CIDR file — merged so a lookup costs one binary search.
package cidrset

import (
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"net/netip"
	"slices"
	"sort"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/fetch"
	"domains.lst/sub-preprocessor/internal/ioutil"
)

// maxListSize bounds ONE decoded source body; the shipped whitelist is ~500 KB,
// so this leaves a mirror room to grow while still refusing a memory bomb.
const maxListSize = 16 << 20

// fetchBytes fetches a source body. It is a package var so tests can stub the
// network fetch (Load otherwise goes through the SSRF-guarded real client).
var fetchBytes = fetch.BytesWithType

// ipRange is an inclusive IPv4 range in native uint32 form.
type ipRange struct {
	lo, hi uint32
}

// Set is an immutable IPv4 allow-list of merged, sorted, disjoint ranges. The
// zero value matches nothing, so a caller wanting fail-closed semantics on a
// failed load must check Covered itself.
type Set struct {
	ranges  []ipRange
	covered uint64
}

// Parse reads one CIDR-per-line body, also accepting bare addresses (the
// upstream repo ships both forms). The second result counts lines that are not
// a valid IPv4 prefix or address — IPv6 included, since the pipeline is
// IPv4-only — which are skipped rather than failing the whole body.
func Parse(body []byte) (Set, int) {
	ranges, skipped := parseRanges(body)
	return newSet(ranges), skipped
}

// Load fetches and parses every URL, skipping (with a warning) any single
// source that fails so one flaky mirror cannot take down startup (mirrors
// geofeed.LoadAll). It fails only when NO source yields a range; failed reports
// how many were skipped, so a caller holding a good set can refuse to replace
// it with a partial one.
func Load(
	ctx context.Context, urls []string, fileType fetch.FileType, logger zerolog.Logger,
) (set Set, failed int, err error) {
	var ranges []ipRange

	for _, url := range urls {
		if url == "" {
			continue
		}

		body, fetchErr := fetchBytes(ctx, fetch.SubscriptionURL(url), maxListSize, fileType)
		if fetchErr != nil {
			failed++
			logger.Warn().Err(fetchErr).Str("url", url).Msg("cidr source fetch failed; skipping")
			continue
		}

		part, skipped := parseRanges(body)
		if len(part) == 0 {
			failed++
			logger.Warn().Str("url", url).Int("skipped_lines", skipped).
				Msg("cidr source parsed no ranges; skipping")
			continue
		}
		logger.Info().Str("url", url).Int("ranges", len(part)).Int("skipped_lines", skipped).
			Msg("cidr source loaded")
		if ranges == nil {
			// The shipped config lists one url; adopting its slice skips a
			// full-size copy newSet would immediately sort in place anyway.
			ranges = part
			continue
		}
		ranges = append(ranges, part...)
	}

	if len(ranges) == 0 {
		return Set{}, failed, fmt.Errorf("no cidr ranges loaded (%d source(s) failed)", failed)
	}

	// Merged once over the union: per-source merging would leave ranges that are
	// adjacent across two sources uncoalesced.
	return newSet(ranges), failed, nil
}

func (s Set) Contains(ip netip.Addr) bool {
	addr := ip.Unmap()
	if len(s.ranges) == 0 || !addr.Is4() {
		return false
	}

	value := addrToUint32(addr)
	// The ranges are disjoint, so the last one starting at or below value is the
	// only candidate.
	idx := sort.Search(len(s.ranges), func(i int) bool { return s.ranges[i].lo > value })
	if idx == 0 {
		return false
	}
	return value <= s.ranges[idx-1].hi
}

// Len reports the MERGED range count. It is an operator-facing number for logs,
// NOT the quantity a refresh guard may compare: see Covered.
func (s Set) Len() int {
	return len(s.ranges)
}

// Covered reports how many addresses the set admits. This is what a refresh
// guard must compare, because the range count moves independently of it in
// BOTH directions: the upstream's own ipwhitelist.txt renders the same space as
// the .1 of every /24, which is 9x the ranges for 0.39% of the coverage, while
// an upstream that consolidates adjacent prefixes shrinks the count over a
// strictly better dataset.
func (s Set) Covered() uint64 {
	return s.covered
}

func parseRanges(body []byte) ([]ipRange, int) {
	ranges := make([]ipRange, 0, bytes.Count(body, []byte{'\n'}))
	skipped := 0

	it := ioutil.NewLines(body)
	for {
		line := it.Next()
		if line == nil {
			break
		}
		parsed, ok := parseLine(line)
		if !ok {
			skipped++
			continue
		}
		ranges = append(ranges, parsed)
	}

	return ranges, skipped
}

func parseLine(line []byte) (ipRange, bool) {
	text := ioutil.UnsafeString(line)

	if bytes.ContainsRune(line, '/') {
		prefix, err := netip.ParsePrefix(text)
		if err != nil || !prefix.Addr().Is4() {
			return ipRange{}, false
		}
		// Masked so a sloppy "1.2.3.4/24" still yields the whole /24.
		prefix = prefix.Masked()
		lo := addrToUint32(prefix.Addr())
		return ipRange{lo: lo, hi: lo | hostMask(prefix.Bits())}, true
	}

	addr, err := netip.ParseAddr(text)
	if err != nil || !addr.Is4() {
		return ipRange{}, false
	}
	value := addrToUint32(addr)
	return ipRange{lo: value, hi: value}, true
}

// newSet sorts and coalesces in place, so ranges must not be retained by the
// caller afterwards.
func newSet(ranges []ipRange) Set {
	if len(ranges) == 0 {
		return Set{}
	}

	slices.SortFunc(ranges, func(a, b ipRange) int { return cmp.Compare(a.lo, b.lo) })

	last := 0
	for _, next := range ranges[1:] {
		if ranges[last].hi == math.MaxUint32 {
			// hi+1 would wrap, and every remaining range starts at or above
			// ranges[last].lo, so all of them are already covered.
			break
		}
		if next.lo <= ranges[last].hi+1 {
			ranges[last].hi = max(ranges[last].hi, next.hi)
			continue
		}
		last++
		ranges[last] = next
	}

	// The scratch slice is sized per LINE while the set lives until the next
	// refresh, so copy out rather than pin the whole parse buffer for a day.
	merged := make([]ipRange, last+1)
	copy(merged, ranges[:last+1])

	var covered uint64
	for _, r := range merged {
		covered += uint64(r.hi-r.lo) + 1
	}
	return Set{ranges: merged, covered: covered}
}

// hostMask returns the low-ones host mask for an IPv4 prefix length; a /32
// shifts the whole word out, which Go defines as 0.
func hostMask(bits int) uint32 {
	return ^uint32(0) >> bits
}

func addrToUint32(addr netip.Addr) uint32 {
	a4 := addr.As4()
	return binary.BigEndian.Uint32(a4[:])
}
