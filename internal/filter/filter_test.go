package filter_test

import (
	"net/netip"
	"testing"

	"domains.lst/sub-preprocessor/internal/filter"
	"domains.lst/sub-preprocessor/internal/geofeed"
)

func TestParseAllowed_CountriesOnly(t *testing.T) {
	t.Parallel()

	set := filter.ParseAllowed("DE,US,  nl  ")
	if !set.Has(geofeed.CountryCode{'D', 'E'}) {
		t.Fatal("expected DE")
	}
	if !set.Has(geofeed.CountryCode{'U', 'S'}) {
		t.Fatal("expected US")
	}
	if !set.Has(geofeed.CountryCode{'N', 'L'}) {
		t.Fatal("expected NL")
	}
	if set.Has(geofeed.CountryCode{'G', 'B'}) {
		t.Fatal("unexpected GB")
	}
}

func TestParseAllowed_Empty(t *testing.T) {
	t.Parallel()

	set := filter.ParseAllowed("")
	for _, v := range set {
		if v != 0 {
			t.Fatal("expected empty set")
		}
	}
}

func TestParseAllowed_Single(t *testing.T) {
	t.Parallel()

	set := filter.ParseAllowed("DE")
	if !set.Has(geofeed.CountryCode{'D', 'E'}) {
		t.Fatal("expected DE")
	}
	if set.Has(geofeed.CountryCode{'U', 'S'}) {
		t.Fatal("unexpected US")
	}
}

func TestParseAllowed_UnknownAndShort(t *testing.T) {
	t.Parallel()

	set := filter.ParseAllowed("DE,XX,XXX,U")
	if !set.Has(geofeed.CountryCode{'D', 'E'}) {
		t.Fatal("expected DE")
	}
	if !set.Has(geofeed.CountryCode{'X', 'X'}) {
		t.Fatal("XX is a valid 2-letter code and should be kept")
	}
	if set.Has(geofeed.CountryCode{'U', 'A'}) {
		t.Fatal("single-letter U and three-letter XXX should not produce a UA bit")
	}
}

func TestCountrySetHas_CaseAndRange(t *testing.T) {
	t.Parallel()

	set := filter.ParseAllowed("de,US")
	if !set.Has(geofeed.CountryCode{'D', 'E'}) {
		t.Fatal("expected DE")
	}
	if !set.Has(geofeed.CountryCode{'U', 'S'}) {
		t.Fatal("expected US")
	}
	if set.Has(geofeed.CountryCode{'d', 'e'}) {
		t.Fatal("lowercase CountryCode values should not be matched")
	}
	if set.Has(geofeed.CountryCode{'A', 'A'}) {
		t.Fatal("unexpected AA")
	}
	if set.Has(geofeed.CountryCode{'Z', 'Z'}) {
		t.Fatal("unexpected ZZ")
	}
}

func TestAllAllowed(t *testing.T) {
	t.Parallel()

	lookup := geofeed.NewLookup([]geofeed.Entry{
		{Prefix: netip.MustParsePrefix("198.51.100.0/24"), Country: geofeed.CountryCode{'N', 'L'}},
		{Prefix: netip.MustParsePrefix("203.0.113.0/24"), Country: geofeed.CountryCode{'D', 'E'}},
	})
	allowed := filter.ParseAllowed("NL")
	got := filter.AllAllowed(lookup, []netip.Addr{
		netip.MustParseAddr("198.51.100.10"),
		netip.MustParseAddr("203.0.113.5"),
		netip.MustParseAddr("198.51.100.20"),
	}, allowed)

	if len(got) != 2 {
		t.Fatalf("unexpected allowed count: %d", len(got))
	}
	if got[0] != netip.MustParseAddr("198.51.100.10") {
		t.Fatalf("unexpected first ip: %s", got[0])
	}
	if got[1] != netip.MustParseAddr("198.51.100.20") {
		t.Fatalf("unexpected second ip: %s", got[1])
	}
	if cap(got) < len(got) {
		t.Fatal("unexpected slice capacity")
	}
	_ = cap(got)
}

func TestAllAllowed_ReusesInputBackingArray(t *testing.T) {
	t.Parallel()

	lookup := geofeed.NewLookup([]geofeed.Entry{
		{Prefix: netip.MustParsePrefix("198.51.100.0/24"), Country: geofeed.CountryCode{'N', 'L'}},
		{Prefix: netip.MustParsePrefix("203.0.113.0/24"), Country: geofeed.CountryCode{'D', 'E'}},
	})
	allowed := filter.ParseAllowed("NL")
	input := []netip.Addr{
		netip.MustParseAddr("198.51.100.10"),
		netip.MustParseAddr("203.0.113.5"),
		netip.MustParseAddr("198.51.100.20"),
	}

	got := filter.AllAllowed(lookup, input, allowed)

	if len(got) != 2 {
		t.Fatalf("unexpected allowed count: %d", len(got))
	}
	if got[0] != input[0] || got[1] != input[2] {
		t.Fatalf("unexpected filtered values: %v", got)
	}
}

func TestCountrySetExclude_RemovesSpecificCountries(t *testing.T) {
	t.Parallel()

	set := filter.ParseAllowed("DE,US,NL")
	set.Exclude(filter.ParseAllowed("US,NL"))

	if !set.Has(geofeed.CountryCode{'D', 'E'}) {
		t.Fatal("expected DE to remain")
	}
	if set.Has(geofeed.CountryCode{'U', 'S'}) {
		t.Fatal("expected US to be excluded")
	}
	if set.Has(geofeed.CountryCode{'N', 'L'}) {
		t.Fatal("expected NL to be excluded")
	}
}

func TestCountrySetExclude_UnknownIgnored(t *testing.T) {
	t.Parallel()

	set := filter.ParseAllowed("DE,US")
	set.Exclude(filter.ParseAllowed("XXX,AA, U "))

	if !set.Has(geofeed.CountryCode{'D', 'E'}) {
		t.Fatal("expected DE to remain")
	}
	if !set.Has(geofeed.CountryCode{'U', 'S'}) {
		t.Fatal("expected US to remain")
	}
}

func TestCountrySetAll(t *testing.T) {
	t.Parallel()

	set := filter.All()
	for c1 := byte('A'); c1 <= 'Z'; c1++ {
		for c2 := byte('A'); c2 <= 'Z'; c2++ {
			cc := geofeed.CountryCode{c1, c2}
			if !set.Has(cc) {
				t.Fatalf("expected %s to be set", cc)
			}
		}
	}
}

func TestCountrySetAll_ExceptExcluded(t *testing.T) {
	t.Parallel()

	set := filter.All()
	set.Exclude(filter.ParseAllowed("DE,US"))

	for c1 := byte('A'); c1 <= 'Z'; c1++ {
		for c2 := byte('A'); c2 <= 'Z'; c2++ {
			cc := geofeed.CountryCode{c1, c2}
			want := (c1 != 'D' || c2 != 'E') && (c1 != 'U' || c2 != 'S')
			if got := set.Has(cc); got != want {
				t.Fatalf("%s: got %v, want %v", cc, got, want)
			}
		}
	}
}

func BenchmarkCountrySetAll(b *testing.B) {
	for b.Loop() {
		_ = filter.All()
	}
}

func BenchmarkCountrySetExclude(b *testing.B) {
	allowed := filter.ParseAllowed("DE,US,NL,FI,EE,GB,FR")
	excluded := filter.ParseAllowed("US,GB")

	for b.Loop() {
		set := allowed
		set.Exclude(excluded)
	}
}
