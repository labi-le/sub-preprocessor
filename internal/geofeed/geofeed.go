package geofeed

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"sort"
	"strings"

	"domains.lst/sub-preprocessor/internal/fetch"
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

type Source struct {
	URL  string
	Type fetch.FileType
}

func LoadAll(ctx context.Context, sources []Source) ([]Entry, error) {
	var entries []Entry
	for _, source := range sources {
		source.URL = strings.TrimSpace(source.URL)
		if source.URL == "" {
			continue
		}

		body, err := fetch.BytesWithType(ctx, source.URL, maxGeofeedSize, source.Type)
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

func Parse(body []byte) ([]Entry, error) {
	// Estimate entry count: count newlines, approximate one entry per line.
	nlCount := bytes.Count(body, []byte{'\n'})
	entries := make([]Entry, 0, nlCount)
	buf := filterBody(body)

	r := csv.NewReader(bytes.NewReader(buf))
	r.FieldsPerRecord = -1
	r.TrimLeadingSpace = true

	for {
		rec, errCSV := r.Read()
		if errors.Is(errCSV, io.EOF) {
			break
		}
		if errCSV != nil {
			return nil, fmt.Errorf("csv read: %w", errCSV)
		}
		if len(rec) < minCSVFields {
			continue
		}

		entry, ok := parseEntry(rec)
		if !ok {
			continue
		}
		entries = append(entries, entry)
	}

	return entries, nil
}

func filterBody(body []byte) []byte {
	buf := make([]byte, 0, len(body))
	remain := body
	for {
		idx := bytes.IndexByte(remain, '\n')
		var line []byte
		if idx < 0 {
			line = remain
			remain = nil
		} else {
			line = remain[:idx]
			remain = remain[idx+1:]
		}

		line = bytes.TrimSpace(line)
		if len(line) != 0 && line[0] != '#' {
			buf = append(buf, line...)
			buf = append(buf, '\n')
		}

		if idx < 0 {
			return buf
		}
	}
}

func parseEntry(rec []string) (Entry, bool) {
	prefix, errPrefix := parsePrefixOrAddr(strings.TrimSpace(rec[0]))
	if errPrefix != nil {
		return Entry{}, false
	}

	country := strings.ToUpper(strings.TrimSpace(rec[1]))
	if country == "" {
		return Entry{}, false
	}

	entry := Entry{Prefix: prefix, Country: country}
	if len(rec) > idxRegion {
		entry.Region = strings.TrimSpace(rec[idxRegion])
	}
	if len(rec) > idxCity {
		entry.City = strings.TrimSpace(rec[idxCity])
	}
	return entry, true
}

func LookupCountry(entries []Entry, ip netip.Addr) string {
	for _, e := range entries {
		if e.Prefix.Contains(ip) {
			return e.Country
		}
	}
	return ""
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
