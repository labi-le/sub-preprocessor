package asn_test

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"domains.lst/sub-preprocessor/internal/asn"
	"domains.lst/sub-preprocessor/internal/geofeed"
)

func BenchmarkResolveASN(b *testing.B) {
	if testing.Short() {
		b.Skip("skipping live ASN benchmark in short mode")
	}

	resolver := asn.New(5*time.Second, time.Hour)
	ip := netip.MustParseAddr("8.8.8.8")
	ctx := context.Background()

	_, err := resolver.Resolve(ctx, ip)
	if err != nil {
		b.Fatalf("initial resolve: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	var res asn.Result
	for range b.N {
		res, err = resolver.Resolve(ctx, ip)
		if err != nil {
			b.Fatalf("resolve: %v", err)
		}
		if res.Country == (geofeed.CountryCode{}) {
			b.Fatal("country is empty")
		}
	}
}
