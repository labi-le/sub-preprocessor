package geofeed

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"domains.lst/sub-preprocessor/internal/fetch"
	"domains.lst/sub-preprocessor/internal/ioutil"
)

const maxGeofeedSize = 256 << 20

const (
	minCSVFields = 2
	idxRegion    = 2
	idxCity      = 3
	bitsV4       = 32
	bitsV6       = 128
)

type Entry struct {
	Prefix  netip.Prefix
	Country string
	Region  string
	City    string
}

// Source defines a geofeed data source.
type Source struct {
	URL  string         `yaml:"url"`
	Type fetch.FileType `yaml:"type"`
}

func LoadAll(ctx context.Context, sources []Source) ([]Entry, error) {
	var entries []Entry
	for _, source := range sources {
		if source.URL == "" {
			continue
		}

		body, err := fetch.BytesWithType(ctx, fetch.SubscriptionURL(source.URL), maxGeofeedSize, source.Type)
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", source.URL, err)
		}

		part, err := Parse(body)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", source.URL, err)
		}
		entries = append(entries, part...)
	}

	if len(entries) == 0 {
		return nil, errors.New("no geofeed entries loaded")
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Prefix.Bits() > entries[j].Prefix.Bits()
	})

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
// Uses the batch-string technique: one string allocation per line, then
// field substrings reference the same backing memory.
func parseLine(line []byte) (Entry, bool) {
	// Create batch string — one alloc for all fields.
	s := ioutil.UnsafeString(line)

	prefixStr, rest, ok := strings.Cut(s, ",")
	if !ok {
		return Entry{}, false
	}

	prefix, err := parsePrefixOrAddr(prefixStr)
	if err != nil {
		return Entry{}, false
	}

	countryStr, rest, hasMore := strings.Cut(rest, ",")
	var country string
	if !hasMore {
		country = strings.ToUpper(rest)
	} else {
		country = strings.ToUpper(countryStr)
	}
	if country == "" {
		return Entry{}, false
	}

	entry := Entry{Prefix: prefix, Country: country}

	if hasMore {
		regionStr, cityStr, hasCity := strings.Cut(rest, ",")
		entry.Region = strings.TrimSpace(regionStr)
		if hasCity {
			entry.City = strings.TrimSpace(cityStr)
		}
	}

	return entry, true
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
	if addr.Is4() {
		return netip.PrefixFrom(addr, bitsV4), nil
	}
	return netip.PrefixFrom(addr, bitsV6), nil
}
