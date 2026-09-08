package ghfind_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"domains.lst/sub-preprocessor/internal/ghfind"
)

func TestCodeQueriesGridStableNonEmptyQualified(t *testing.T) {
	first := ghfind.CodeQueries()
	if len(first) == 0 {
		t.Fatal("code query grid is empty")
	}
	if len(first) != 30 {
		t.Fatalf("grid has %d entries, want 6 schemes x 5 shapes", len(first))
	}
	if !slices.Equal(first, ghfind.CodeQueries()) {
		t.Fatal("code query grid is not stable across calls")
	}
	schemes := []string{"vless://", "vmess://", "trojan://", "hysteria2://", "ss://", "tuic://"}
	for i, q := range first {
		if !strings.Contains(q, "in:file") {
			t.Errorf("entry %d %q lacks in:file", i, q)
		}
		hasScheme := false
		for _, scheme := range schemes {
			if strings.Contains(q, scheme) {
				hasScheme = true
				break
			}
		}
		if !hasScheme {
			t.Errorf("entry %d %q carries no scheme literal", i, q)
		}
	}
}

func TestRepoQueriesRenderPushedWindowInUTC(t *testing.T) {
	// 2026-09-07T02:00+05:00 is 2026-09-06T21:00Z; a fresh window of 24h cuts
	// off at 2026-09-05T21:00Z. Formatting the cutoff in the +05:00 zone
	// instead of UTC would render 2026-09-06, so the expected date proves the
	// UTC conversion.
	now := time.Date(2026, 9, 7, 2, 0, 0, 0, time.FixedZone("UTC+5", int(5*time.Hour/time.Second)))
	grid := ghfind.RepoQueries(now, 24*time.Hour)
	if len(grid) == 0 {
		t.Fatal("repo query grid is empty")
	}
	if !slices.Equal(grid, ghfind.RepoQueries(now, 24*time.Hour)) {
		t.Fatal("repo query grid is not stable across calls")
	}
	for i, q := range grid {
		if !strings.Contains(q, "pushed:>=2026-09-05") {
			t.Errorf("entry %d %q lacks the pushed window date", i, q)
		}
	}
	topics := []string{"vless", "v2ray-config", "free-vpn", "v2ray-subscription", "clash-subscription", "proxy-list"}
	for _, topic := range topics {
		if !hasEntryPrefix(grid, "topic:"+topic+" ") {
			t.Errorf("grid lacks topic query for %q", topic)
		}
	}
	for _, fragment := range []string{"免费节点 订阅", "机场 订阅", "free vpn subscription link", "proxy subscription collector"} {
		if !hasEntryFragment(grid, fragment) {
			t.Errorf("grid lacks keyword query containing %q", fragment)
		}
	}
}

func hasEntryPrefix(grid []string, prefix string) bool {
	for _, q := range grid {
		if strings.HasPrefix(q, prefix) {
			return true
		}
	}
	return false
}

func hasEntryFragment(grid []string, fragment string) bool {
	for _, q := range grid {
		if strings.Contains(q, fragment) {
			return true
		}
	}
	return false
}
