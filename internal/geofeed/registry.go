package geofeed

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"strconv"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/fetch"
	"domains.lst/sub-preprocessor/internal/ioutil"
)

// LoadRegistry fetches and parses the RIR delegated-extended files, skipping
// (with a warning) any single RIR that fails so one registry outage cannot
// take down startup (mirrors LoadAll). It fails only when NO ranges load.
func LoadRegistry(ctx context.Context, urls []string, logger zerolog.Logger) ([]Range, error) {
	var ranges []Range
	var failed int
	for _, url := range urls {
		if url == "" {
			continue
		}

		body, err := fetchBytes(ctx, fetch.SubscriptionURL(url), maxGeofeedSize, fetch.FileTypeRaw)
		if err != nil {
			failed++
			logger.Warn().Err(err).Str("url", url).Msg("registry source fetch failed; skipping")
			continue
		}

		part := ParseDelegated(body)
		if len(part) == 0 {
			failed++
			logger.Warn().Str("url", url).Msg("registry source parsed no ranges; skipping")
			continue
		}
		ranges = append(ranges, part...)
	}

	if len(ranges) == 0 {
		return nil, fmt.Errorf("no registry ranges loaded (%d source(s) failed)", failed)
	}
	return ranges, nil
}

// ParseDelegated parses an RIR delegated-extended body:
// registry|cc|type|start|value|date|status[|extensions]. Only ipv4/ipv6
// records with status allocated/assigned and a real country survive; version
// header, summary rows, asn records, available/reserved, and ZZ/*/empty
// countries are skipped. Per-line tolerant like the other parsers.
func ParseDelegated(body []byte) []Range {
	nlCount := bytes.Count(body, []byte{'\n'})
	ranges := make([]Range, 0, nlCount)

	it := ioutil.NewLines(body)
	for {
		line := it.Next()
		if line == nil {
			break
		}
		if r, ok := parseDelegatedLine(line); ok {
			ranges = append(ranges, r)
		}
	}
	return ranges
}

func parseDelegatedLine(line []byte) (Range, bool) {
	sep := []byte{'|'}
	_, rest, ok := bytes.Cut(line, sep) // registry, unused
	if !ok {
		return Range{}, false
	}
	ccBytes, rest, ok := bytes.Cut(rest, sep)
	if !ok {
		return Range{}, false
	}
	typBytes, rest, ok := bytes.Cut(rest, sep)
	if !ok {
		return Range{}, false
	}
	startBytes, rest, ok := bytes.Cut(rest, sep)
	if !ok {
		return Range{}, false
	}
	valueBytes, rest, ok := bytes.Cut(rest, sep)
	if !ok {
		return Range{}, false
	}
	// The version header and |summary rows have fewer fields and die on this
	// cut; the type/status checks below reject any that squeeze through.
	_, rest, ok = bytes.Cut(rest, sep) // date, unused
	if !ok {
		return Range{}, false
	}
	statusBytes, _, _ := bytes.Cut(rest, sep)

	isV4 := bytes.Equal(typBytes, []byte("ipv4"))
	if !isV4 && !bytes.Equal(typBytes, []byte("ipv6")) {
		return Range{}, false
	}
	if !bytes.Equal(statusBytes, []byte("allocated")) && !bytes.Equal(statusBytes, []byte("assigned")) {
		return Range{}, false
	}
	country, okCC := parseCountry(ccBytes)
	if !okCC {
		return Range{}, false
	}
	start, errAddr := netip.ParseAddr(ioutil.UnsafeString(startBytes))
	if errAddr != nil || start.Is4() != isV4 {
		return Range{}, false
	}
	value, errValue := strconv.ParseUint(ioutil.UnsafeString(valueBytes), 10, 64)
	if errValue != nil || value == 0 {
		return Range{}, false
	}

	if isV4 {
		// v4 value is an address COUNT; blocks may not be CIDR-aligned.
		start32 := addrToUint32(start)
		if value > uint64(^uint32(0)-start32)+1 {
			return Range{}, false
		}
		return Range{Start: start, End: uint32ToAddr(start32 + uint32(value) - 1), Country: country}, true
	}

	// v6 value is a prefix LENGTH; delegations are CIDR by format definition.
	if value > 128 { //nolint:mnd // IPv6 = 128 bits
		return Range{}, false
	}
	prefix, errPrefix := start.Prefix(int(value))
	if errPrefix != nil {
		return Range{}, false
	}
	return prefixRange(prefix, country), true
}
