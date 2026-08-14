package subscription

import (
	"net/netip"
	"strings"
)

// nilUUIDDigits is the hex-digit count of the Nil UUID, dashes aside.
const nilUUIDDigits = 32

// PlaceholderNode reports whether raw is a panel's stand-in for a subscription
// it no longer serves. Such a body answers 200 with one unusable node, and in
// the measured case advertises an expire= still in the future, so the header
// gate cannot see the death and the body would otherwise read live.
//
// It lives here because two callers must agree on it: classify refuses to call
// such a body a live subscription, and the stable worker's Merge refuses it a
// probe slot. The worker never calls classify, so a second copy of the rule
// would let a URL classify dead while its nodes went on being probed.
//
// It takes the two fields it reads rather than the 88-byte Node, which spills
// past the register ABI's nine words on every node. Inlining is out of reach
// either way: cost 122 against a budget of 80.
func PlaceholderNode(raw, server string) bool {
	return nilCredential(raw) || unspecifiedServer(server)
}

// nilCredential reports whether the URI userinfo is the RFC 9562 §5.9 Nil UUID,
// which by definition names no account. All 32 zeros are required: a short
// all-zero credential is a legal password, not evidence of a dead panel.
func nilCredential(raw string) bool {
	_, auth, ok := strings.Cut(raw, schemeSep)
	if !ok {
		return false
	}
	// The credential is all '0' and '-' or nothing, so its first byte settles
	// the ~15/16 of real UUIDs that cannot be Nil before the scans below.
	if auth == "" || (auth[0] != '0' && auth[0] != '-') {
		return false
	}
	// Authority ends at the first '/', '?' or '#'; explicit IndexByte calls
	// beat IndexAny on strings this short (parseNode).
	end := len(auth)
	if j := strings.IndexByte(auth, '/'); j >= 0 && j < end {
		end = j
	}
	if j := strings.IndexByte(auth, '?'); j >= 0 && j < end {
		end = j
	}
	if j := strings.IndexByte(auth, '#'); j >= 0 && j < end {
		end = j
	}
	// splitHostPort takes the last '@' as the userinfo boundary; match it, so
	// this reads the credential the parsed Server actually came after.
	at := strings.LastIndexByte(auth[:end], '@')
	if at <= 0 {
		return false
	}
	zeros := 0
	for j := range at {
		switch auth[j] {
		case '0':
			zeros++
		case '-':
		default:
			return false
		}
	}
	return zeros >= nilUUIDDigits
}

// unspecifiedServer reports whether server is 0.0.0.0 or ::, an address no
// client can dial. Deliberately not a reserved-range test: the measured
// placeholder sits on a routable address, which such a test would miss.
//
// Nor is such a node a harmless dial failure: connect(0.0.0.0:p) lands on
// 127.0.0.1:p, so a placeholder whose port matches the service's own listener
// passes the worker's TCP precheck and spends a URLTest round on itself.
func unspecifiedServer(server string) bool {
	// netip.ParseAddr heap-boxes its error, and a digit-leading HOSTNAME
	// ("0.tcp.example.com" is a common tunnel shape) passed a server[0] gate:
	// 1000 such nodes cost 48 KB and 1000 allocs. Every byte of an address
	// IsUnspecified accepts is '0', ':' or '.', and a zoned address is never
	// unspecified, so any other byte answers without parsing (agrees with
	// netip over 5.4M enumerated strings).
	if server == "" {
		return false
	}
	for i := range len(server) {
		if c := server[i]; c != '0' && c != ':' && c != '.' {
			return false
		}
	}
	addr, err := netip.ParseAddr(server)
	return err == nil && addr.IsUnspecified()
}
