package geofeed_test

import (
	"net/netip"
	"sort"
	"strings"
	"testing"

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

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Prefix.Bits() > entries[j].Prefix.Bits()
	})

	if got := geofeed.LookupCountry(entries, netip.MustParseAddr("198.51.100.10")); got != "NL" {
		t.Fatalf("unexpected country: %q", got)
	}
}
