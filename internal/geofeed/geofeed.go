package geofeed

import (
	"bufio"
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
	var filtered bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		filtered.WriteString(line)
		filtered.WriteByte('\n')
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan lines: %w", err)
	}

	r := csv.NewReader(bytes.NewReader(filtered.Bytes()))
	r.FieldsPerRecord = -1
	r.TrimLeadingSpace = true

	var entries []Entry
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

		prefix, errPrefix := parsePrefixOrAddr(strings.TrimSpace(rec[0]))
		if errPrefix != nil {
			continue
		}
		country := strings.ToUpper(strings.TrimSpace(rec[1]))
		if country == "" {
			continue
		}

		entry := Entry{Prefix: prefix, Country: country}
		if len(rec) > idxRegion {
			entry.Region = strings.TrimSpace(rec[idxRegion])
		}
		if len(rec) > idxCity {
			entry.City = strings.TrimSpace(rec[idxCity])
		}
		entries = append(entries, entry)
	}

	return entries, nil
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
