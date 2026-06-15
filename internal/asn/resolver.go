package asn

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"domains.lst/sub-preprocessor/internal/geofeed"
)

const (
	cymruOriginDomain = "origin.asn.cymru.com"
	cymruASDomain     = "asn.cymru.com"
	minASRecordFields = 5
	minOriginFields   = 3
)

type Result struct {
	Country geofeed.CountryCode
	Name    string
}

type Resolver struct {
	cache   sync.Map
	timeout time.Duration
}

func New(timeout time.Duration) *Resolver {
	return &Resolver{timeout: timeout}
}

func (r *Resolver) Resolve(ctx context.Context, ip netip.Addr) (Result, error) {
	if !ip.Is4() {
		return Result{}, fmt.Errorf("ASN lookup is not supported for IPv6: %s", ip)
	}

	if v, ok := r.cache.Load(ip); ok {
		if res, ok2 := v.(Result); ok2 {
			return res, nil
		}
	}

	result, err := r.lookup(ctx, ip)
	if err != nil {
		return Result{}, err
	}

	r.cache.Store(ip, result)
	return result, nil
}

func reverseIP(ip netip.Addr) string {
	if !ip.Is4() {
		return ""
	}
	ip4 := ip.As4()
	return fmt.Sprintf("%d.%d.%d.%d", ip4[3], ip4[2], ip4[1], ip4[0])
}

func (r *Resolver) lookup(ctx context.Context, ip netip.Addr) (Result, error) {
	resolveCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	netR := &net.Resolver{
		PreferGo: true,
	}

	rev := reverseIP(ip)

	originTXT, err := netR.LookupTXT(resolveCtx, rev+"."+cymruOriginDomain)
	if err != nil {
		return Result{}, fmt.Errorf("cymru origin lookup: %w", err)
	}

	var asn uint32
	var country geofeed.CountryCode
	for _, txt := range originTXT {
		asn, country, err = parseOriginRecord(txt)
		if err == nil {
			break
		}
	}
	if err != nil {
		return Result{}, fmt.Errorf("parse origin record: %w", err)
	}

	asTXT, err := netR.LookupTXT(resolveCtx, fmt.Sprintf("AS%d.%s", asn, cymruASDomain))
	if err != nil {
		return Result{Country: country}, fmt.Errorf("cymru as lookup: %w", err)
	}

	name := ""
	for _, txt := range asTXT {
		if n := parseASRecord(txt); n != "" {
			name = n
			break
		}
	}

	return Result{Country: country, Name: name}, nil
}

func parseOriginRecord(txt string) (uint32, geofeed.CountryCode, error) {
	// "216071 | 146.103.121.0/24 | AE | ripencc | 1992-10-23"
	parts := strings.Split(txt, "|")
	if len(parts) < 1 {
		return 0, geofeed.CountryCode{}, fmt.Errorf("unexpected origin format: %q", txt)
	}
	asnStr := strings.TrimSpace(parts[0])
	asn, err := strconv.ParseUint(asnStr, 10, 32)
	if err != nil {
		return 0, geofeed.CountryCode{}, fmt.Errorf("parse asn %q: %w", asnStr, err)
	}
	var country geofeed.CountryCode
	if len(parts) >= minOriginFields {
		s := strings.TrimSpace(parts[2])
		s = strings.ToUpper(s)
		if len(s) == 2 { //nolint:mnd // ISO 3166-1 alpha-2 length
			country = geofeed.CountryCode{s[0], s[1]}
		}
	}
	return uint32(asn), country, nil
}

func parseASRecord(txt string) string {
	// "216071 | AE | ripencc | 2023-10-30 | VDSINA - SERVERS TECH FZCO, AE"
	parts := strings.Split(txt, "|")
	if len(parts) < minASRecordFields {
		return ""
	}
	return strings.TrimSpace(parts[4])
}
