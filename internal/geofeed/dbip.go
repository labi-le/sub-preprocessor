package geofeed

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/fetch"
	"domains.lst/sub-preprocessor/internal/ioutil"
)

// timeNow is a package var so tests pin the month templating in LoadDBIP.
var timeNow = time.Now

// ExpandMonthURL replaces the literal {yyyy-mm} placeholder with now's UTC
// month (DB-IP publishes per UTC calendar month).
func ExpandMonthURL(url string, now time.Time) string {
	return strings.ReplaceAll(url, "{yyyy-mm}", now.UTC().Format("2006-01"))
}

// LoadDBIP fetches and parses the DB-IP country lite database (gzip CSV). On a
// 404 for the current month (not yet published right after the month rollover)
// it retries once with the previous month. Both failing returns the error; the
// caller degrades to an empty lookup rather than failing startup.
func LoadDBIP(ctx context.Context, url string, logger zerolog.Logger) ([]Range, error) {
	now := timeNow()
	monthURL := ExpandMonthURL(url, now)
	body, err := fetchBytes(ctx, fetch.SubscriptionURL(monthURL), maxGeofeedSize, fetch.FileTypeGzip)
	if err != nil {
		// AddDate overflows a month's last days into the current one (Mar 31
		// minus a month is Mar 3), so derive the previous month from the first.
		first := time.Date(now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
		prevURL := ExpandMonthURL(url, first.AddDate(0, -1, 0))
		var statusErr *fetch.StatusError
		if prevURL == monthURL || !errors.As(err, &statusErr) || statusErr.Code != http.StatusNotFound {
			return nil, fmt.Errorf("fetch dbip: %w", err)
		}
		logger.Warn().Str("url", monthURL).Msg("dbip current month not published; retrying previous month")
		body, err = fetchBytes(ctx, fetch.SubscriptionURL(prevURL), maxGeofeedSize, fetch.FileTypeGzip)
		if err != nil {
			return nil, fmt.Errorf("fetch dbip previous month: %w", err)
		}
	}

	ranges := ParseDBIP(body)
	if len(ranges) == 0 {
		return nil, errors.New("no dbip ranges parsed")
	}
	return ranges, nil
}

// ParseDBIP parses a DB-IP country lite CSV body: start_ip,end_ip,CC per line,
// v4 and v6 mixed. Per-line tolerant: malformed, mixed-family, unordered, and
// reserved non-country-marker (ZZ, EU) lines are skipped.
func ParseDBIP(body []byte) []Range {
	nlCount := bytes.Count(body, []byte{'\n'})
	ranges := make([]Range, 0, nlCount)

	it := ioutil.NewLines(body)
	for {
		line := it.Next()
		if line == nil {
			break
		}
		if r, ok := parseDBIPLine(line); ok {
			ranges = append(ranges, r)
		}
	}
	return ranges
}

func parseDBIPLine(line []byte) (Range, bool) {
	startBytes, rest, ok := bytes.Cut(line, []byte{','})
	if !ok {
		return Range{}, false
	}
	endBytes, rest, ok := bytes.Cut(rest, []byte{','})
	if !ok {
		return Range{}, false
	}
	ccBytes, _, _ := bytes.Cut(rest, []byte{','})

	country, ok := parseCountry(ccBytes)
	if !ok {
		return Range{}, false
	}
	start, errStart := netip.ParseAddr(ioutil.UnsafeString(startBytes))
	if errStart != nil {
		return Range{}, false
	}
	end, errEnd := netip.ParseAddr(ioutil.UnsafeString(endBytes))
	if errEnd != nil || start.Is4() != end.Is4() || end.Less(start) {
		return Range{}, false
	}
	return Range{Start: start, End: end, Country: country}, true
}

// parseCountry is the one country-code validator every CSV parser shares
// (geofeed, dbip, registry): exactly two ASCII letters, folded to upper case,
// minus the reserved non-country markers the sources emit in place of a code.
// ZZ marks an IP the source cannot attribute to any country; EU is ripencc's
// region marker for allocations RIPE cannot attribute to one country (the only
// non-ISO code in the five delegated-extended files: 10 rows in
// delegated-ripencc-extended-latest, measured 2026-09-05). Either must read as
// a miss, or the lookup chain would publish a non-country ahead of the real
// databases.
func parseCountry(b []byte) (CountryCode, bool) {
	if len(b) != 2 { //nolint:mnd // ISO 3166-1 alpha-2 length
		return CountryCode{}, false
	}
	const caseBit = 0x20 // ASCII case bit; clearing it upper-folds letters
	c1, c2 := b[0]&^caseBit, b[1]&^caseBit
	if c1 < 'A' || c1 > 'Z' || c2 < 'A' || c2 > 'Z' {
		return CountryCode{}, false
	}
	cc := CountryCode{c1, c2}
	switch cc {
	case CountryCode{'Z', 'Z'}, CountryCode{'E', 'U'}:
		return CountryCode{}, false
	}
	return cc, true
}
