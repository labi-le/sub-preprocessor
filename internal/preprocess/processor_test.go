package preprocess_test

import (
	"bytes"
	"net/netip"
	"strings"
	"testing"
	"time"

	"domains.lst/sub-preprocessor/internal/geofeed"
	"domains.lst/sub-preprocessor/internal/preprocess"
	"domains.lst/sub-preprocessor/internal/rewrite"
	"domains.lst/sub-preprocessor/internal/subscription"
)

func TestRewriteNodeName(t *testing.T) {
	t.Parallel()

	var nodes []subscription.Node
	subscription.Parse([]byte("vless://uuid@example.com:443?security=tls#Old Name"), func(n subscription.Node) bool {
		nodes = append(nodes, n)
		return true
	})

	var b bytes.Buffer
	rewrite.NodeName(&b, nodes[0], geofeed.CountryCode{'N', 'L'}, netip.MustParseAddr("198.51.100.10"))
	got := b.String()
	want := "vless://uuid@example.com:443?security=tls#[GEO:NL][IP:198.51.100.10] Old Name"
	if got != want {
		t.Fatalf("unexpected rewritten uri:\n got: %q\nwant: %q", got, want)
	}
}

func TestRewriteNodeNameUnknownSchemeStillRewritesURIFragment(t *testing.T) {
	t.Parallel()

	var nodes []subscription.Node
	subscription.Parse([]byte("trojan://uuid@example.com:443#Old Name"), func(n subscription.Node) bool {
		nodes = append(nodes, n)
		return true
	})

	var b bytes.Buffer
	rewrite.NodeName(&b, nodes[0], geofeed.CountryCode{'N', 'L'}, netip.MustParseAddr("198.51.100.10"))
	got := b.String()
	want := "trojan://uuid@example.com:443#[GEO:NL][IP:198.51.100.10] Old Name"
	if got != want {
		t.Fatalf("unexpected rewritten uri:\n got: %q\nwant: %q", got, want)
	}
}

func TestStripKnownTags(t *testing.T) {
	t.Parallel()

	if got := rewrite.StripKnownTags("[GEO:NL][IP:1.2.3.4][OK] Amsterdam 01"); got != "Amsterdam 01" {
		t.Fatalf("unexpected cleaned name: %q", got)
	}
}

func TestFormatStats(t *testing.T) {
	t.Parallel()

	got := preprocess.FormatStats(preprocess.Stats{Total: 10, Kept: 3, DNSDrop: 1, GeoDrop: 6, ASNDrop: 1})
	if !strings.Contains(got, "total=10") || !strings.Contains(got, "kept=3") {
		t.Fatalf("unexpected stats: %q", got)
	}
}

func TestShouldReloadGeofeed(t *testing.T) {
	t.Parallel()

	svc := &preprocess.Processor{RefreshInterval: time.Hour, LoadedAt: time.Now().Add(-2 * time.Hour)}
	if !svc.ShouldReloadGeofeed(time.Now()) {
		t.Fatal("expected geofeed reload")
	}

	svc = &preprocess.Processor{RefreshInterval: time.Hour, LoadedAt: time.Now().Add(-30 * time.Minute)}
	if svc.ShouldReloadGeofeed(time.Now()) {
		t.Fatal("did not expect geofeed reload")
	}

	svc = &preprocess.Processor{RefreshInterval: 0, LoadedAt: time.Now().Add(-24 * time.Hour)}
	if svc.ShouldReloadGeofeed(time.Now()) {
		t.Fatal("did not expect geofeed reload when refresh interval disabled")
	}
}
