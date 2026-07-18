package geofeed_test

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"domains.lst/sub-preprocessor/internal/geofeed"
)

func TestExpandMonthURL(t *testing.T) {
	t.Parallel()

	utc := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)
	if got := geofeed.ExpandMonthURL("https://x/db-{yyyy-mm}.csv.gz", utc); got != "https://x/db-2026-07.csv.gz" {
		t.Fatalf("ExpandMonthURL = %q", got)
	}

	// Local time already in August, but UTC still July: the UTC month must win
	// (db-ip publishes on UTC month boundaries).
	east := time.Date(2026, time.August, 1, 0, 30, 0, 0, time.FixedZone("east", 2*3600))
	if got := geofeed.ExpandMonthURL("https://x/db-{yyyy-mm}.csv.gz", east); got != "https://x/db-2026-07.csv.gz" {
		t.Fatalf("ExpandMonthURL(east) = %q", got)
	}

	if got := geofeed.ExpandMonthURL("https://x/static.csv.gz", utc); got != "https://x/static.csv.gz" {
		t.Fatalf("ExpandMonthURL(no placeholder) = %q", got)
	}
}

func TestParseDBIP(t *testing.T) {
	t.Parallel()

	body := []byte(strings.Join([]string{
		"# comment",
		"1.0.0.0,1.0.0.255,AU",
		"1.0.1.0,1.0.3.255,cn", // lowercase folds to CN
		"2600:6000::,2600:6fff:ffff:ffff:ffff:ffff:ffff:ffff,US",
		"0.0.0.0,0.255.255.255,ZZ", // unknown country skipped
		"1.2.3.4,2001:db8::1,US",   // mixed family skipped
		"5.6.7.8,5.6.7.0,US",       // end < start skipped
		"9.9.9.9,9.9.9.10",         // missing country skipped
		"foo,bar,US",               // unparseable addrs skipped
		"10.0.0.0,10.0.0.255,USA",  // 3-letter country skipped
		"",
	}, "\n"))

	got := geofeed.ParseDBIP(body)
	want := []geofeed.Range{
		mustRange("1.0.0.0", "1.0.0.255", "AU"),
		mustRange("1.0.1.0", "1.0.3.255", "CN"),
		mustRange("2600:6000::", "2600:6fff:ffff:ffff:ffff:ffff:ffff:ffff", "US"),
	}
	if len(got) != len(want) {
		t.Fatalf("ParseDBIP returned %d ranges, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("range[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseDBIP_LookupIntegration(t *testing.T) {
	t.Parallel()

	ranges := geofeed.ParseDBIP([]byte("1.0.0.0,1.0.0.255,AU\n"))
	lookup := geofeed.NewRangeLookup(ranges)
	if got := lookup.LookupCountry(netip.MustParseAddr("1.0.0.42")); got != (geofeed.CountryCode{'A', 'U'}) {
		t.Fatalf("lookup = %q, want AU", got)
	}
}
