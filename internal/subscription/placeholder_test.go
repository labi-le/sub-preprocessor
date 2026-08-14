package subscription_test

import (
	"net/netip"
	"strings"
	"testing"

	"domains.lst/sub-preprocessor/internal/subscription"
)

// plainCredential carries an ordinary UUID, so in serverCases the address rule
// is the only rule that can fire.
const plainCredential = "vless://a1b2c3d4-1111-4000-8000-000000000009@192.0.2.1:443#a"

// credentialServer is the server plainCredential and every credentialCases raw
// name: routable documentation space, so the credential rule decides alone.
const credentialServer = "192.0.2.1"

// credentialCases exercise the credential rule. The near misses matter as much
// as the hits: the predicate gates a probe slot in the stable worker and a
// live/nodeless verdict in classify, so a false positive deletes a working node
// from a working source.
var credentialCases = []struct {
	name string
	raw  string
	want bool
}{{
	name: "nil uuid credential",
	raw:  "vless://00000000-0000-0000-0000-000000000000@192.0.2.1:1111?type=tcp#notice",
	want: true,
}, {
	name: "nil uuid with query and fragment before the at",
	raw:  "vless://00000000-0000-0000-0000-000000000000@192.0.2.1:1111#a@b",
	want: true,
}, {
	name: "one digit short of the nil uuid",
	raw:  "vless://00000000-0000-0000-0000-000000000001@192.0.2.1:443#a",
	want: false,
}, {
	name: "leading nonzero digit",
	raw:  "vless://10000000-0000-0000-0000-000000000000@192.0.2.1:443#b",
	want: false,
}, {
	name: "short all-zero password is a legal password",
	raw:  "trojan://0@192.0.2.1:443#a",
	want: false,
}, {
	name: "dash-only credential",
	raw:  "vless://0-0@192.0.2.1:443#b",
	want: false,
}, {
	name: "no userinfo at all",
	raw:  "ss://192.0.2.1:443#a",
	want: false,
}, {
	name: "not a uri",
	raw:  "just some prose about an expired subscription",
	want: false,
}}

// serverCases exercise the address rule against plainCredential. Every spelling
// a dialer sends to the local machine is a hit; every malformed all-zero string
// is a miss that must cost no allocation, which is why this table is also the
// allocation gate's table.
var serverCases = []struct {
	server string
	want   bool
}{
	{server: "0.0.0.0", want: true},
	{server: "::", want: true},
	{server: "::1", want: true},
	{server: "127.0.0.1", want: true},
	// The whole loopback /8, not just its first address.
	{server: "127.0.0.2", want: true},
	{server: "127.255.255.254", want: true},
	// A dialer unmaps these and connects over IPv4.
	{server: "::ffff:127.0.0.1", want: true},
	{server: "::FFFF:0.0.0.0", want: true},
	// Long forms of the two IPv6 spellings.
	{server: "0:0:0:0:0:0:0:0", want: true},
	{server: "0:0:0:0:0:0:0:1", want: true},
	{server: "0000:0000:0000:0000:0000:0000:0000:0001", want: true},
	{server: "::0", want: true},
	{server: "0::", want: true},
	{server: "0::0:1", want: true},
	// Neighbours of the rule, all of them dialable remotes.
	{server: "0.0.0.1", want: false},
	{server: "1.0.0.127", want: false},
	{server: "126.255.255.255", want: false},
	{server: "128.0.0.1", want: false},
	{server: "192.0.2.1", want: false},
	{server: "2001:db8::1", want: false},
	{server: "1::", want: false},
	{server: "0:1:0:0:0:0:0:0", want: false},
	{server: "::2", want: false},
	{server: "::11", want: false},
	// Hostnames, including the tunnel shape that once reached netip.ParseAddr
	// and a wildcard name whose loopback prefix is not an address. A name needs
	// a resolver, so none of these is judged local -- with one exception below.
	{server: "cdn.example.com", want: false},
	{server: "0.tcp.example.com", want: false},
	{server: "127.0.0.1.example.com", want: false},
	{server: "127.0.0.256", want: false},
	{server: "127.1", want: false},
	// The exception: RFC 6761 §6.3 reserves this name and REQUIRES resolution to
	// the loopback, so it is local by definition and needs no lookup. Measured
	// 2026-08-14, 15 published nodes in the configured corpus name it outright.
	{server: "localhost", want: true},
	{server: "LOCALHOST", want: true},
	{server: "LocalHost", want: true},
	// The RFC reserves everything under ".localhost" too, so the last label
	// decides.
	{server: "foo.localhost", want: true},
	{server: "a.b.LOCALHOST", want: true},
	// Not the reserved name: a longer label, or a real domain that merely
	// contains it. The absolute form is a deliberate miss (empty last label).
	{server: "notlocalhost", want: false},
	{server: "localhostx", want: false},
	{server: "localhost.example.com", want: false},
	// A name may begin with a byte the address arms dispatch on. These answered
	// false while those arms returned unconditionally on a failed match, which
	// is the reachability bug the switch was restructured to remove.
	{server: "0.localhost", want: true},
	{server: "1.localhost", want: true},
	{server: "127.localhost", want: true},
	{server: "127.0.0.1.localhost", want: true},
	{server: "2.localhost", want: true},
	{server: "localhost.", want: false},
	// Out of scope by decision, documented on localV6: netip reads these as
	// local, but no formatter emits them, and a miss only costs a probe slot.
	// The long-form mapping is here because the enumeration test cannot reach it
	// (maxLen 7, and the shortest such spelling is 22 chars), so this table is
	// the only thing stopping a widened cutMappedV4 from moving it silently.
	{server: "::1%lo", want: false},
	{server: "::ffff:7f00:1", want: false},
	{server: "::0.0.0.0", want: false},
	{server: "0:0:0:0:0:ffff:127.0.0.1", want: false},
	{server: "0000:0000:0000:0000:0000:ffff:127.0.0.1", want: false},
	// Malformed: the shapes the charset filter used to hand to netip.ParseAddr.
	{server: "00.00.00.00", want: false},
	{server: "0.0.0.0.0", want: false},
	{server: "0.0", want: false},
	{server: "000", want: false},
	{server: "0", want: false},
	{server: "0.0.0.00", want: false},
	{server: "::ffff:0.0.0.0.0", want: false},
	{server: "0:0:0:0:0:0:0", want: false},
	{server: "0:0:0:0:0:0:0:0:0", want: false},
	{server: "::0:0:0:0:0:0:0:0", want: false},
	{server: "0::0::1", want: false},
	{server: ":::", want: false},
	{server: ":", want: false},
	{server: ":1", want: false},
	{server: "0:", want: false},
	{server: "", want: false},
}

func TestPlaceholderNode(t *testing.T) {
	t.Parallel()

	for _, tc := range credentialCases {
		if got := subscription.PlaceholderNode(tc.raw, credentialServer); got != tc.want {
			t.Errorf("%s: PlaceholderNode(%q, %q) = %v, want %v",
				tc.name, tc.raw, credentialServer, got, tc.want)
		}
	}
	for _, tc := range serverCases {
		if got := subscription.PlaceholderNode(plainCredential, tc.server); got != tc.want {
			t.Errorf("PlaceholderNode(plain credential, %q) = %v, want %v",
				tc.server, got, tc.want)
		}
	}
}

// TestPlaceholderNodeAllocatesNothing runs the correctness tables verbatim, so a
// shape added for correctness cannot skip the allocation gate: the malformed
// all-zero server that reached netip.ParseAddr for 48 B a call was already in
// the correctness table while this test named three fixtures of its own.
func TestPlaceholderNodeAllocatesNothing(t *testing.T) {
	for _, tc := range credentialCases {
		got := false
		allocs := testing.AllocsPerRun(50, func() {
			got = subscription.PlaceholderNode(tc.raw, credentialServer)
		})
		if got != tc.want {
			t.Errorf("%s: verdict %v, want %v — fixture stopped exercising its path",
				tc.name, got, tc.want)
		}
		if allocs != 0 {
			t.Errorf("%s: allocated %.0f times per run, want 0", tc.name, allocs)
		}
	}
	for _, tc := range serverCases {
		got := false
		allocs := testing.AllocsPerRun(50, func() {
			got = subscription.PlaceholderNode(plainCredential, tc.server)
		})
		if got != tc.want {
			t.Errorf("%q: verdict %v, want %v — fixture stopped exercising its path",
				tc.server, got, tc.want)
		}
		if allocs != 0 {
			t.Errorf("%q: allocated %.0f times per run, want 0", tc.server, allocs)
		}
	}
}

// TestPlaceholderNodeAgreesWithNetip: the address rule recognizes the local
// spellings itself instead of calling netip.ParseAddr, whose error is heap-boxed,
// so netip stays the authority in the test rather than on the hot path. The two
// directions are not symmetric — accepting a string netip does not read as local
// deletes a working node, while missing one only leaves a probe slot — and over
// this space the only mismatches are the ZONED spellings localV6 documents as out
// of scope. Unmap first: netip.Addr.IsLoopback unmaps itself, IsUnspecified does
// not, and a dialer does.
func TestPlaceholderNodeAgreesWithNetip(t *testing.T) {
	t.Parallel()

	const (
		alphabet = "0127.:fF%"
		maxLen   = 7 // reaches a dotted quad, "::" with groups, and a zone
	)
	buf := make([]byte, 0, maxLen)
	checked, zoned := 0, 0

	var walk func()
	walk = func() {
		if len(buf) > 0 {
			s := string(buf)
			addr, err := netip.ParseAddr(s)
			addr = addr.Unmap()
			want := err == nil && (addr.IsUnspecified() || addr.IsLoopback())
			if got := subscription.PlaceholderNode(plainCredential, s); got != want {
				switch {
				case got:
					t.Fatalf("accepted %q, which netip does not read as local", s)
				case strings.Contains(s, "%"):
					zoned++
				default:
					t.Fatalf("missed %q, which netip reads as local", s)
				}
			}
			checked++
		}
		if len(buf) == maxLen {
			return
		}
		for i := range len(alphabet) {
			buf = append(buf, alphabet[i])
			walk()
			buf = buf[:len(buf)-1]
		}
	}
	walk()

	if checked < 5_000_000 {
		t.Fatalf("enumeration collapsed to %d strings", checked)
	}
	if zoned == 0 {
		t.Fatal("no zoned address was reached: the space no longer covers the documented boundary")
	}
}
