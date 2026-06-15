package geofeed_test

import (
	"context"
	"net/netip"
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
	}, "\n"))

	entries, err := geofeed.Parse(body)
	if err != nil {
		t.Fatal(err)
	}

	lookup := geofeed.NewLookup(entries)

	if got := geofeed.LookupCountry(lookup, netip.MustParseAddr("198.51.100.10")); got != (geofeed.CountryCode{'N', 'L'}) {
		t.Fatalf("unexpected country: %q", got)
	}
}

func TestGstaticGeofeedLive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
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
