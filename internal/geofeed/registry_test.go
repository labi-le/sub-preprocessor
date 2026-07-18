package geofeed_test

import (
	"strings"
	"testing"

	"domains.lst/sub-preprocessor/internal/geofeed"
)

func TestParseDelegated(t *testing.T) {
	t.Parallel()

	body := []byte(strings.Join([]string{
		"2|ripencc|20260718|123456|19830705|20260717|+0200", // version header
		"# comment",
		"ripencc|*|ipv4|*|71111|summary",                       // summary row
		"ripencc|*|ipv6|*|12345|summary",                       // summary row
		"ripencc|*|asn|*|999|summary",                          // summary row
		"apnic|AU|ipv4|1.0.0.0|256|20110811|assigned|A9186214", // extension field ok
		"ripencc|FR|ipv4|2.0.0.0|768|20100712|allocated",       // non-CIDR count
		"apnic|jp|ipv6|2001:200::|35|19990813|allocated",       // lowercase folds
		"ripencc|FR|asn|3215|1|19950101|allocated",             // asn record skipped
		"ripencc|ZZ|ipv4|10.0.0.0|256|20100101|allocated",      // unknown country skipped
		"arin||ipv4|192.0.2.0|256||available",                  // available skipped
		"arin|US|ipv4|198.51.100.0|256|20000101|reserved",      // reserved skipped
		"arin|US|ipv4|255.255.255.0|512|20000101|allocated",    // count overflows v4 space
		"arin|US|ipv4|not-an-ip|256|20000101|allocated",        // bad addr skipped
		"arin|US|ipv6|2001:db8::|200|20000101|allocated",       // prefix length > 128 skipped
		"arin|US|ipv4|192.0.2.0|0|20000101|allocated",          // zero count skipped
		"garbage",
	}, "\n"))

	got := geofeed.ParseDelegated(body)
	want := []geofeed.Range{
		mustRange("1.0.0.0", "1.0.0.255", "AU"),
		// 768 addresses from 2.0.0.0: not expressible as one CIDR block.
		mustRange("2.0.0.0", "2.0.2.255", "FR"),
		// ipv6 value is a prefix LENGTH: 2001:200::/35.
		mustRange("2001:200::", "2001:200:1fff:ffff:ffff:ffff:ffff:ffff", "JP"),
	}
	if len(got) != len(want) {
		t.Fatalf("ParseDelegated returned %d ranges, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("range[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
