package geofeed

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/fetch"
	"domains.lst/sub-preprocessor/internal/ioutil"
)

const (
	maxGeofeedSize = 256 << 20
)

// fetchBytes fetches a source body. It is a package var so tests can stub the
// network fetch (LoadAll otherwise goes through the SSRF-guarded real client).
var fetchBytes = fetch.BytesWithType

// CountryCode is a strict 2-byte ISO country code.
type CountryCode [2]byte

func (c CountryCode) String() string {
	return string(c[:])
}

type Entry struct {
	Prefix  netip.Prefix
	Country CountryCode
}

type Source struct {
	URL  string         `yaml:"url"`
	Type fetch.FileType `yaml:"type"`
}

// LoadAll fetches and parses every source, skipping (with a warning) any single
// source that fails to fetch or parse so one flaky third-party feed cannot take
// down startup. It fails only when NO source yields entries.
func LoadAll(ctx context.Context, sources []Source, logger zerolog.Logger) ([]Entry, error) {
	var entries []Entry
	var failed int
	for _, source := range sources {
		if source.URL == "" {
			continue
		}

		body, err := fetchBytes(ctx, fetch.SubscriptionURL(source.URL), maxGeofeedSize, source.Type)
		if err != nil {
			failed++
			logger.Warn().Err(err).Str("url", source.URL).Msg("geofeed source fetch failed; skipping")
			continue
		}

		part, err := Parse(body)
		if err != nil {
			failed++
			logger.Warn().Err(err).Str("url", source.URL).Msg("geofeed source parse failed; skipping")
			continue
		}
		entries = append(entries, part...)
	}

	if len(entries) == 0 {
		return nil, fmt.Errorf("no geofeed entries loaded (%d source(s) failed)", failed)
	}

	return entries, nil
}

// Parse parses a geofeed CSV body (prefix, country, region, city per line).
// Comments starting with '#' are skipped. Only lines with at least 2 fields
// (prefix and country) are kept.
func Parse(body []byte) ([]Entry, error) {
	entries := parseBody(body)
	if len(entries) == 0 {
		return nil, errors.New("no valid geofeed entries found")
	}
	return entries, nil
}

func parseBody(body []byte) []Entry {
	nlCount := bytes.Count(body, []byte{'\n'})
	entries := make([]Entry, 0, nlCount)

	it := ioutil.NewLines(body)
	for {
		line := it.Next()
		if line == nil {
			break
		}
		if entry, ok := parseLine(line); ok {
			entries = append(entries, entry)
		}
	}

	return entries
}

// parseLine parses a single geofeed CSV line.
func parseLine(line []byte) (Entry, bool) {
	prefixBytes, rest, ok := bytes.Cut(line, []byte{','})
	if !ok {
		return Entry{}, false
	}

	prefixStr := ioutil.UnsafeString(prefixBytes)
	prefix, err := parsePrefixOrAddr(prefixStr)
	if err != nil {
		return Entry{}, false
	}

	countryBytes, _, _ := bytes.Cut(rest, []byte{','})
	if len(countryBytes) != 2 { //nolint:mnd // ISO 3166-1 alpha-2 length
		return Entry{}, false
	}

	c1, c2 := countryBytes[0], countryBytes[1]
	if c1 >= 'a' && c1 <= 'z' {
		c1 -= 32
	}
	if c2 >= 'a' && c2 <= 'z' {
		c2 -= 32
	}

	return Entry{Prefix: prefix, Country: CountryCode{c1, c2}}, true
}

func parsePrefixOrAddr(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		p, errParse := netip.ParsePrefix(s)
		if errParse != nil {
			return netip.Prefix{}, fmt.Errorf("parse prefix: %w", errParse)
		}
		return p.Masked(), nil
	}

	addr, errAddr := netip.ParseAddr(s)
	if errAddr != nil {
		return netip.Prefix{}, fmt.Errorf("parse addr: %w", errAddr)
	}
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}
