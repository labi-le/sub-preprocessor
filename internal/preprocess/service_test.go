package preprocess

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"domains.lst/sub-preprocessor/internal/geofeed"
	"domains.lst/sub-preprocessor/internal/subscription"
)

func TestRewriteNodeName(t *testing.T) {
	t.Parallel()

	nodes, err := subscription.Parse([]byte("vless://uuid@example.com:443?security=tls#Old Name"))
	if err != nil {
		t.Fatal(err)
	}

	got := rewriteNodeName(nodes[0], "NL", netip.MustParseAddr("198.51.100.10"))
	want := "vless://uuid@example.com:443?security=tls#[GEO:NL][IP:198.51.100.10] Old Name"
	if got != want {
		t.Fatalf("unexpected rewritten uri:\n got: %q\nwant: %q", got, want)
	}
}

func TestFirstAllowedIP(t *testing.T) {
	t.Parallel()

	entries := []geofeed.Entry{{Prefix: netip.MustParsePrefix("198.51.100.0/24"), Country: "DE"}, {Prefix: netip.MustParsePrefix("203.0.113.0/24"), Country: "US"}}
	ips := []netip.Addr{netip.MustParseAddr("203.0.113.10"), netip.MustParseAddr("198.51.100.10")}
	allowed := parseAllowCountries([]string{"DE"})

	ip, country, ok := firstAllowedIP(entries, ips, allowed)
	if !ok || country != "DE" || ip.String() != "198.51.100.10" {
		t.Fatalf("unexpected firstAllowedIP result: %v %q %v", ip, country, ok)
	}
}

func TestStripKnownTags(t *testing.T) {
	t.Parallel()

	if got := stripKnownTags("[GEO:NL][IP:1.2.3.4][OK] Amsterdam 01"); got != "Amsterdam 01" {
		t.Fatalf("unexpected cleaned name: %q", got)
	}
}

func TestFormatStats(t *testing.T) {
	t.Parallel()

	got := FormatStats(Stats{Total: 10, Kept: 3, DNSDrop: 1, GeoDrop: 6})
	if !strings.Contains(got, "total=10") || !strings.Contains(got, "kept=3") {
		t.Fatalf("unexpected stats: %q", got)
	}
}

func TestShouldReloadGeofeed(t *testing.T) {
	t.Parallel()

	svc := &Service{refreshInterval: time.Hour, loadedAt: time.Now().Add(-2 * time.Hour)}
	if !svc.shouldReloadGeofeed(time.Now()) {
		t.Fatal("expected geofeed reload")
	}

	svc = &Service{refreshInterval: time.Hour, loadedAt: time.Now().Add(-30 * time.Minute)}
	if svc.shouldReloadGeofeed(time.Now()) {
		t.Fatal("did not expect geofeed reload")
	}

	svc = &Service{refreshInterval: 0, loadedAt: time.Now().Add(-24 * time.Hour)}
	if svc.shouldReloadGeofeed(time.Now()) {
		t.Fatal("did not expect geofeed reload when refresh interval disabled")
	}
}
