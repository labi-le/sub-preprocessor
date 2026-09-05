package geofeed_test

import (
	"context"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"domains.lst/sub-preprocessor/internal/fetch"
	"domains.lst/sub-preprocessor/internal/geofeed"
)

func TestParseAndLookupCountry(t *testing.T) {
	t.Parallel()

	body := []byte(strings.Join([]string{
		"# comment",
		"198.51.100.0/24,DE",
		"198.51.100.10/32,NL,ZH,Amsterdam",
		// Digits fold to themselves, not letters: a junk country must drop
		// the line rather than mint a CountryCode of raw bytes. Its range sits
		// apart from the two above, so nothing else can answer for it.
		"203.0.113.0/24,11",
	}, "\n"))

	entries, err := geofeed.Parse(body)
	if err != nil {
		t.Fatal(err)
	}

	lookup := geofeed.NewLookup(entries)

	if got := geofeed.LookupCountry(lookup, netip.MustParseAddr("198.51.100.10")); got != (geofeed.CountryCode{'N', 'L'}) {
		t.Fatalf("unexpected country: %q", got)
	}
	if got := geofeed.LookupCountry(lookup, netip.MustParseAddr("203.0.113.1")); got != (geofeed.CountryCode{}) {
		t.Fatalf("a line with a non-letter country must be dropped whole, got %q", got)
	}
}

// The CSV parsers once disagreed on the unknown-country marker: a geofeed row
// kept its prefix under ZZ while dbip dropped the range.
// That kept row indexed a range whose country every chain consumer treated as
// a first-hit answer — ZZ shadowed dbip/registry and rendered [GEO:ZZ] instead
// of falling through as unknown. The parsers are now aligned (round 2): every
// reserved non-country marker (ZZ, EU) is a miss. This test is
// TestZZCountryDivergence reconciled to the shared contract.
func TestReservedMarkersDroppedByCSVParsers(t *testing.T) {
	t.Parallel()

	// A geofeed whose only country is ZZ parses to nothing: Parse errors on
	// zero entries, which is the miss the chain must fall through.
	if _, err := geofeed.Parse([]byte("198.51.100.0/24,ZZ\n")); err == nil {
		t.Fatal("a ZZ-only body must parse to nothing: ZZ is a miss, not a country")
	}

	// Beside a real row, ZZ and EU rows must not index.
	entries, err := geofeed.Parse([]byte(strings.Join([]string{
		"198.51.100.0/24,ZZ",
		"198.51.100.0/24,EU", // ripencc region marker: a region, not a country
		"203.0.113.0/24,DE",
	}, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Country != (geofeed.CountryCode{'D', 'E'}) {
		t.Fatalf("ZZ/EU rows must fall through to real rows, got %+v", entries)
	}

	// The dropped row's prefix must miss: no range answers for it, so a chain
	// would consult the next database instead of stopping at a false code.
	lookup := geofeed.NewLookup(entries)
	if got := geofeed.LookupCountry(lookup, netip.MustParseAddr("198.51.100.5")); got != (geofeed.CountryCode{}) {
		t.Fatalf("a dropped ZZ row must not answer its prefix, got %q", got)
	}

	if ranges := geofeed.ParseDBIP([]byte("0.0.0.0,0.255.255.255,ZZ\n")); len(ranges) != 0 {
		t.Fatalf("dbip must drop ZZ rows, got %+v", ranges)
	}
}

func TestGstaticGeofeedLive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	if os.Getenv("LIVE_TESTS") == "" {
		t.Skip("skipping live test; set LIVE_TESTS to enable")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	body, err := fetch.BytesWithType(ctx, fetch.SubscriptionURL("https://www.gstatic.com/geofeed/corp_external"), 10<<20, fetch.FileTypeRaw)
	if err != nil {
		t.Fatalf("fetch gstatic geofeed: %v", err)
	}

	entries, err := geofeed.Parse(body)
	if err != nil {
		t.Fatalf("parse gstatic geofeed: %v", err)
	}

	if len(entries) < 1000 {
		t.Fatalf("too few entries: got %d, want >= 1000", len(entries))
	}

	// Verify lookup works for known Google IPs.
	lookup := geofeed.NewLookup(entries)

	if got := geofeed.LookupCountry(lookup, netip.MustParseAddr("8.8.8.8")); got != (geofeed.CountryCode{'U', 'S'}) {
		t.Logf("expected 8.8.8.8 → US, got %q (possibly changed)", got)
	}

	if got := geofeed.LookupCountry(lookup, netip.MustParseAddr("142.250.80.46")); got == (geofeed.CountryCode{}) {
		t.Logf("expected known Google IP to resolve to a country, got empty (possibly changed)")
	}
}

func TestExtraGeofeedsLive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	if os.Getenv("LIVE_TESTS") == "" {
		t.Skip("skipping live test; set LIVE_TESTS to enable")
	}

	urls := []struct {
		name     string
		url      string
		minRows  int
		fileType fetch.FileType
	}{
		{name: "NTT", url: "https://geo.ip.gin.ntt.net/geofeeds/geofeeds.csv", minRows: 100, fileType: fetch.FileTypeRaw},
		{name: "Cyberzone", url: "https://geofeeds.cyberzonehub.com/geofeed.csv", minRows: 100, fileType: fetch.FileTypeRaw},
		{name: "TNG", url: "https://tngnet.com/public/geofeed.csv", minRows: 10, fileType: fetch.FileTypeRaw},
	}

	for _, src := range urls {
		t.Run(src.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			body, err := fetch.BytesWithType(ctx, fetch.SubscriptionURL(src.url), 10<<20, src.fileType)
			if err != nil {
				t.Fatalf("fetch %s: %v", src.name, err)
			}

			entries, err := geofeed.Parse(body)
			if err != nil {
				t.Fatalf("parse %s: %v", src.name, err)
			}

			if len(entries) < src.minRows {
				t.Fatalf("too few entries from %s: got %d, want >= %d", src.name, len(entries), src.minRows)
			}

			// Collect unique countries for reporting.
			countries := make(map[geofeed.CountryCode]int)
			for _, e := range entries {
				countries[e.Country]++
			}
			t.Logf("%s: %d entries, %d unique countries", src.name, len(entries), len(countries))
		})
	}
}
