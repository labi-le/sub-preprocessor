package geo_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"domains.lst/sub-preprocessor/internal/asn"
	"domains.lst/sub-preprocessor/internal/geo"
	"domains.lst/sub-preprocessor/internal/geofeed"
)

func cc(s string) geofeed.CountryCode {
	return geofeed.CountryCode{s[0], s[1]}
}

//nolint:ireturn // test helper intentionally returns the lookup interface
func lookupWith(country string) geofeed.CountryLookup {
	return geofeed.NewLookup([]geofeed.Entry{
		{Prefix: netip.MustParsePrefix("1.2.3.0/24"), Country: cc(country)},
	})
}

func TestLookupProviderName(t *testing.T) {
	p := geo.NewLookupProvider("dbip", func() geofeed.CountryLookup { return nil })
	if got := p.Name(); got != "dbip" {
		t.Fatalf("Name() = %q, want %q", got, "dbip")
	}
}

func TestLookupProviderLookupCovered(t *testing.T) {
	lookup := lookupWith("DE")
	p := geo.NewLookupProvider("geofeed", func() geofeed.CountryLookup { return lookup })

	got := p.Lookup(context.Background(), netip.MustParseAddr("1.2.3.4"))
	if got.Country != cc("DE") {
		t.Fatalf("Country = %v, want DE", got.Country)
	}
	if got.ASN != "" {
		t.Fatalf("ASN = %q, want empty", got.ASN)
	}
}

func TestLookupProviderLookupUncovered(t *testing.T) {
	lookup := lookupWith("DE")
	p := geo.NewLookupProvider("geofeed", func() geofeed.CountryLookup { return lookup })

	got := p.Lookup(context.Background(), netip.MustParseAddr("9.9.9.9"))
	if got != (geo.Info{}) {
		t.Fatalf("Lookup uncovered = %+v, want zero Info", got)
	}
}

func TestLookupProviderReflectsSwap(t *testing.T) {
	current := lookupWith("DE")
	p := geo.NewLookupProvider("geofeed", func() geofeed.CountryLookup { return current })

	ip := netip.MustParseAddr("1.2.3.4")
	if got := p.Lookup(context.Background(), ip); got.Country != cc("DE") {
		t.Fatalf("before swap Country = %v, want DE", got.Country)
	}

	// Swap in a lookup that maps the same IP to a different country; the
	// provider must observe the new lookup via the getter.
	current = lookupWith("FR")
	if got := p.Lookup(context.Background(), ip); got.Country != cc("FR") {
		t.Fatalf("after swap Country = %v, want FR", got.Country)
	}
}

type stubResolver struct {
	result asn.Result
	err    error
}

func (s stubResolver) Resolve(_ context.Context, _ netip.Addr) (asn.Result, error) {
	return s.result, s.err
}

func TestASNProviderName(t *testing.T) {
	p := geo.NewASN(stubResolver{})
	if got := p.Name(); got != "asn" {
		t.Fatalf("Name() = %q, want %q", got, "asn")
	}
}

func TestASNProviderLookupSuccess(t *testing.T) {
	p := geo.NewASN(stubResolver{
		result: asn.Result{Country: cc("AE"), Name: "VDSINA - SERVERS TECH FZCO, AE"},
	})

	got := p.Lookup(context.Background(), netip.MustParseAddr("146.103.121.1"))
	if got.Country != cc("AE") {
		t.Fatalf("Country = %v, want AE", got.Country)
	}
	if got.ASN != "VDSINA - SERVERS TECH FZCO, AE" {
		t.Fatalf("ASN = %q, want the AS name", got.ASN)
	}
}

func TestASNProviderLookupError(t *testing.T) {
	p := geo.NewASN(stubResolver{
		result: asn.Result{Country: cc("AE"), Name: "should be ignored"},
		err:    errors.New("resolve failed"),
	})

	got := p.Lookup(context.Background(), netip.MustParseAddr("146.103.121.1"))
	if got != (geo.Info{}) {
		t.Fatalf("Lookup on error = %+v, want zero Info", got)
	}
}
