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
// take down startup (mirrors LoadAll). It fails only when NO ranges load;
// failed reports how many RIRs were skipped, so a caller that already holds a
// good database can refuse to replace it with a partial one.
//
// Every source that loads logs its header serial. That is an OBSERVABLE, not a
// gate -- nothing branches on it and a stale file still loads. It exists
// because not every default URL is served by the RIR that cut the file
// (config.defaultRegistryURLs reads APNIC off a mirror), and a copy frozen
// months ago is otherwise indistinguishable from a current one: it fetches 200,
// parses clean, reports sources_failed=0, and lands a range count well inside
// swapRefusal's minSwapRatio. Deciding what to DO about lag needs a policy this
// does not have; seeing it needs only the field.
//
// The serial's FORM is the publishing registry's business and the five do not
// agree -- apnic, lacnic and afrinic write YYYYMMDD (20260804), ripencc a unix
// second count (1785794399), arin unix milliseconds (1785848421615) -- so read
// it per source against that source's own last value, never across sources. It
// is monotonic within a source, which is all a lag signal needs, and on the one
// URL that is actually a third party's copy it happens to be legible as a date.
func LoadRegistry(ctx context.Context, urls []string, logger zerolog.Logger) (ranges []Range, failed int, err error) {
	for _, url := range urls {
		if url == "" {
			continue
		}

		body, fetchErr := fetchBytes(ctx, fetch.SubscriptionURL(url), maxGeofeedSize, fetch.FileTypeRaw)
		if fetchErr != nil {
			failed++
			logger.Warn().Err(fetchErr).Str("url", url).Msg("registry source fetch failed; skipping")
			continue
		}

		part := ParseDelegated(body)
		if len(part) == 0 {
			failed++
			logger.Warn().Str("url", url).Msg("registry source parsed no ranges; skipping")
			continue
		}
		logger.Info().Str("url", url).Str("serial", delegatedSerial(body)).
			Int("ranges", len(part)).Msg("registry source loaded")
		if ranges == nil {
			// Adopt the first parsed source's backing array, as LoadAll does:
			// appending into nil would duplicate that largest source with a
			// full-size []Range allocation and memcpy on every build.
			ranges = part
			continue
		}
		ranges = append(ranges, part...)
	}

	if len(ranges) == 0 {
		return nil, failed, fmt.Errorf("no registry ranges loaded (%d source(s) failed)", failed)
	}
	return ranges, failed, nil
}

// delegatedSerial reads the serial out of a delegated-extended version header
// -- "2.3|apnic|20260804|188932||20260803|+1000" yields "20260804". The header
// is the first non-comment line of the body and ParseDelegated drops it (field
// 2, where a record carries its type, holds the serial instead, so it matches
// neither ipv4 nor ipv6), which is why the loaded database otherwise carries no
// trace of when the file was cut.
//
// Returns "" for a body that does not open on a header, rather than reporting a
// record's own field 2: "serial=ipv4" in a log line is worse than no field.
func delegatedSerial(body []byte) string {
	sep := []byte{'|'}
	it := ioutil.NewLines(body)
	line := it.Next()
	if line == nil {
		return ""
	}
	_, rest, ok := bytes.Cut(line, sep) // version
	if !ok {
		return ""
	}
	_, rest, ok = bytes.Cut(rest, sep) // registry
	if !ok {
		return ""
	}
	serial, _, ok := bytes.Cut(rest, sep)
	if !ok || bytes.Equal(serial, []byte("ipv4")) || bytes.Equal(serial, []byte("ipv6")) ||
		bytes.Equal(serial, []byte("asn")) {
		return ""
	}
	return string(serial)
}

// ParseDelegated parses an RIR delegated-extended body:
// registry|cc|type|start|value|date|status[|extensions]. Only ipv4/ipv6
// records with status allocated/assigned and a real country survive; version
// header, summary rows, asn records, available/reserved rows, non-country
// markers (ZZ, EU), '*', and empty countries are skipped.
// Per-line tolerant like the other parsers.
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
