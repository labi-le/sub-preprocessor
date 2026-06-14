package asn //nolint:testpackage // accesses unexported symbols

import (
	"context"
	"net/netip"
	"testing"
	"time"
)

func BenchmarkParseOriginRecord(b *testing.B) {
	txt := "216071 | 146.103.121.0/24 | AE | ripencc | 1992-10-23"
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		got, country, err := parseOriginRecord(txt)
		if err != nil {
			b.Fatal(err)
		}
		if got != 216071 {
			b.Fatalf("unexpected ASN: %d", got)
		}
		if country != "AE" {
			b.Fatalf("unexpected country: %q", country)
		}
	}
}

func BenchmarkParseASRecord(b *testing.B) {
	txt := "216071 | AE | ripencc | 2023-10-30 | VDSINA - SERVERS TECH FZCO, AE"
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		got := parseASRecord(txt)
		if got != "VDSINA - SERVERS TECH FZCO, AE" {
			b.Fatalf("unexpected name: %q", got)
		}
	}
}

func BenchmarkParseASRecord_Short(b *testing.B) {
	// record with fewer fields than expected — should return ""
	txt := "216071 | AE | ripencc"
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		got := parseASRecord(txt)
		if got != "" {
			b.Fatalf("expected empty for short record, got: %q", got)
		}
	}
}

func BenchmarkResolve_CacheHit(b *testing.B) {
	r := New(time.Second)
	ip := netip.MustParseAddr("146.103.121.166")
	r.cache.Store(ip, Result{ASN: 216071, Country: "AE", Name: "VDSINA - SERVERS TECH FZCO, AE"})
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		got, err := r.Resolve(ctx, ip)
		if err != nil {
			b.Fatal(err)
		}
		if got.ASN != 216071 {
			b.Fatalf("unexpected ASN: %d", got.ASN)
		}
	}
}

func BenchmarkResolve_CacheHit_IPv6(b *testing.B) {
	r := New(time.Second)
	ip := netip.MustParseAddr("2001:db8::1")
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, err := r.Resolve(ctx, ip)
		if err == nil {
			b.Fatal("expected error for IPv6")
		}
	}
}
