package cidrset_test

import (
	"net/netip"
	"strings"
	"testing"

	"domains.lst/sub-preprocessor/internal/cidrset"
)

func TestParseAndContains(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		lines       []string
		wantLen     int
		wantSkipped int
		in          []string
		out         []string
	}{
		{
			name:    "prefix covers both boundary addresses only",
			lines:   []string{"192.0.2.0/24"},
			wantLen: 1,
			in:      []string{"192.0.2.0", "192.0.2.128", "192.0.2.255"},
			out:     []string{"192.0.1.255", "192.0.3.0"},
		},
		{
			name:    "bare address is a host route",
			lines:   []string{"203.0.113.7"},
			wantLen: 1,
			in:      []string{"203.0.113.7"},
			out:     []string{"203.0.113.6", "203.0.113.8"},
		},
		{
			name:    "unaligned prefix is masked",
			lines:   []string{"198.51.100.77/24"},
			wantLen: 1,
			in:      []string{"198.51.100.0", "198.51.100.255"},
			out:     []string{"198.51.99.255", "198.51.101.0"},
		},
		{
			name:    "overlapping and adjacent ranges coalesce",
			lines:   []string{"10.0.1.0/24", "10.0.0.0/24", "10.0.0.128/25", "10.0.3.0/24"},
			wantLen: 2,
			in:      []string{"10.0.0.255", "10.0.1.0", "10.0.3.1"},
			out:     []string{"10.0.2.0", "10.0.4.0"},
		},
		{
			name:    "a contained range does not shrink the merge",
			lines:   []string{"10.1.0.0/16", "10.1.5.0/24"},
			wantLen: 1,
			in:      []string{"10.1.5.1", "10.1.255.255"},
			out:     []string{"10.2.0.0"},
		},
		{
			name:    "default route covers the whole space",
			lines:   []string{"0.0.0.0/0", "8.8.8.8"},
			wantLen: 1,
			in:      []string{"0.0.0.0", "8.8.8.8", "100.64.3.9", "255.255.255.255"},
		},
		{
			name:    "adjacency at the top of the space does not wrap",
			lines:   []string{"255.255.254.0/24", "255.255.255.0/24", "255.255.255.128/25"},
			wantLen: 1,
			in:      []string{"255.255.254.0", "255.255.255.128", "255.255.255.255"},
			out:     []string{"255.255.253.255"},
		},
		{
			name: "non-IPv4 and junk lines are skipped without poisoning the answers",
			lines: []string{
				"# comment", "", "2001:db8::/32", "::ffff:1.2.3.4",
				"1.2.3.4/33", "nonsense", "10.0.0.0/8",
			},
			wantLen:     1,
			wantSkipped: 4,
			in:          []string{"10.1.2.3"},
			out:         []string{"1.2.3.4", "2001:db8::1"},
		},
		{
			name:    "a comment-only body yields nothing to match",
			lines:   []string{"# nothing here", ""},
			wantLen: 0,
			out:     []string{"10.0.0.1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			set, skipped := cidrset.Parse([]byte(strings.Join(tt.lines, "\n") + "\n"))
			if skipped != tt.wantSkipped {
				t.Errorf("skipped = %d, want %d", skipped, tt.wantSkipped)
			}
			if got := set.Len(); got != tt.wantLen {
				t.Errorf("Len() = %d, want %d (merge is wrong)", got, tt.wantLen)
			}
			assertMembership(t, set, tt.in, tt.out)
		})
	}
}

func assertMembership(t *testing.T, set cidrset.Set, in, out []string) {
	t.Helper()

	for _, addr := range in {
		if !set.Contains(netip.MustParseAddr(addr)) {
			t.Errorf("Contains(%s) = false, want true", addr)
		}
	}
	for _, addr := range out {
		if set.Contains(netip.MustParseAddr(addr)) {
			t.Errorf("Contains(%s) = true, want false", addr)
		}
	}
}

// TestContainsUnmapsIPv4In6 pins the asymmetry with the skipped "::ffff:1.2.3.4"
// LINE above: a mapped literal is not a list entry, but a mapped query address
// is the same host and must match.
func TestContainsUnmapsIPv4In6(t *testing.T) {
	t.Parallel()

	set, _ := cidrset.Parse([]byte("1.2.3.0/24\n"))

	if !set.Contains(netip.MustParseAddr("::ffff:1.2.3.4")) {
		t.Error("Contains(::ffff:1.2.3.4) = false, want true")
	}
	if set.Contains(netip.MustParseAddr("::ffff:1.2.4.4")) {
		t.Error("Contains(::ffff:1.2.4.4) = true, want false")
	}
	if set.Contains(netip.MustParseAddr("2001:db8::1")) {
		t.Error("Contains(2001:db8::1) = true, want false")
	}
}

// TestZeroSetMatchesNothing: an unloaded set must never read as permissive --
// the callers treat an empty set as fail-closed, not as "no filter".
func TestZeroSetMatchesNothing(t *testing.T) {
	t.Parallel()

	var set cidrset.Set

	if got := set.Len(); got != 0 {
		t.Errorf("Len() = %d, want 0", got)
	}
	if got := set.Covered(); got != 0 {
		t.Errorf("Covered() = %d, want 0", got)
	}
	for _, addr := range []netip.Addr{
		netip.MustParseAddr("10.0.0.1"),
		netip.MustParseAddr("0.0.0.0"),
		netip.MustParseAddr("255.255.255.255"),
		{},
	} {
		if set.Contains(addr) {
			t.Errorf("Contains(%v) = true, want false", addr)
		}
	}
}

// TestCoveredIsNotRangeCount pins the property the refresh guard rests on: the
// range count can GROW while coverage collapses. The second body is how the
// same upstream renders this space in ipwhitelist.txt -- the .1 of every /24 --
// so a guard reading Len() sees 4x growth for 1/256th of the addresses.
func TestCoveredIsNotRangeCount(t *testing.T) {
	t.Parallel()

	blocks, _ := cidrset.Parse([]byte("1.2.0.0/22\n"))
	hosts, _ := cidrset.Parse([]byte("1.2.0.1\n1.2.1.1\n1.2.2.1\n1.2.3.1\n"))

	if blocks.Len() != 1 || blocks.Covered() != 4*256 {
		t.Fatalf("blocks: Len=%d Covered=%d, want 1 and 1024", blocks.Len(), blocks.Covered())
	}
	if hosts.Len() != 4 || hosts.Covered() != 4 {
		t.Fatalf("hosts: Len=%d Covered=%d, want 4 and 4", hosts.Len(), hosts.Covered())
	}

	merged, _ := cidrset.Parse([]byte("1.2.0.0/24\n1.2.1.0/24\n"))
	if merged.Len() != 1 || merged.Covered() != 512 {
		t.Fatalf("adjacent: Len=%d Covered=%d, want 1 and 512: coalescing must not lose coverage",
			merged.Len(), merged.Covered())
	}
}

// TestCoveredWholeSpace: 0.0.0.0/0 is 2^32 addresses, which only fits because
// Covered is uint64.
func TestCoveredWholeSpace(t *testing.T) {
	t.Parallel()

	set, _ := cidrset.Parse([]byte("0.0.0.0/0\n"))

	if got := set.Covered(); got != 1<<32 {
		t.Errorf("Covered() = %d, want %d", got, uint64(1)<<32)
	}
}
