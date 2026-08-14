package subscription

import "strings"

// nilUUIDDigits is the hex-digit count of the Nil UUID, dashes aside.
const nilUUIDDigits = 32

// mappedV4Prefix is the IPv4-mapped IPv6 prefix as netip.Addr.String writes it.
const mappedV4Prefix = "::ffff:"

// loopbackV4Prefix is the only start a dotted quad in the loopback /8 can have,
// and unspecifiedV4 is the only in-scope address in 0.0.0.0/8.
const (
	loopbackV4Prefix = "127."
	unspecifiedV4    = "0.0.0.0"
)

// loopbackHostname is the name RFC 6761 §6.3 reserves for the loopback, and
// asciiCaseBit lowercases an ASCII letter with an OR, which is how the name is
// compared without allocating a lowercased copy per node.
const (
	loopbackHostname = "localhost"
	asciiCaseBit     = 0x20
)

// v6Groups and maxV6GroupDigits are the IPv6 grammar netip accepts, shared by
// the group walk and its caller.
const (
	v6Groups         = 8
	maxV6GroupDigits = 4
)

// PlaceholderNode reports whether raw is a panel's stand-in for a subscription
// it no longer serves: a credential naming no account, or a server naming the
// machine that would dial it. The measured panel advertised an expire= still in
// the future, so the header gate cannot see that death — only the node can.
//
// It lives here because classify (which refuses such a body a live verdict) and
// stable.Merge (which refuses the node a probe slot) must not disagree: the
// worker never calls classify, so a second copy of the rule would let a URL
// classify dead while its nodes went on being probed. It takes the two fields
// it reads because the 88-byte Node spills past the register ABI; inlining is
// out of reach either way (cost 122 against a budget of 80).
func PlaceholderNode(raw, server string) bool {
	return nilCredential(raw) || localServer(server)
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

// localServer reports whether server names the machine that would dial it: the
// unspecified address (0.0.0.0, ::) or a loopback one. Such a node can never
// carry traffic, however good its credential, and it is worse than a dial
// failure: connect(0.0.0.0:p) lands on the loopback at p, so a node on the
// service's own listener port (both shipped configs listen on :8080) passes the
// worker's TCP precheck and spends a URLTest round dialling the worker.
//
// The whole loopback /8 is in scope, not just its first address, because the
// kernel routes all of it to the loopback interface (RFC 1122 host
// requirements), so a listener on :8080 answers any of it; the mappedV4Prefix
// form is in scope because a dialer unmaps it and connects over IPv4. The rest
// of 0.0.0.0/8 is NOT: a dialer refuses those rather than sending them home.
// Deliberately not a reserved-range test — the measured placeholder also sat on
// a routable address, and dropping private ranges would drop the LAN nodes a
// self-hosted source publishes.
func localServer(server string) bool {
	if server == "" {
		return false
	}
	switch server[0] {
	case '0':
		if localV4(server) {
			return true
		}
		// Only a zero group can begin a written-out IPv6 loopback: a group of
		// 1 has to be the last one.
		return strings.IndexByte(server, ':') >= 0 && localV6(server)
	case '1':
		return localV4(server)
	case ':':
		return localV6(server)
	}
	// Not an address shape, so the only thing left that this rule may judge is
	// the one reserved name. Reached for every hostname, which is why the test
	// below rejects on a length compare before touching a byte.
	return loopbackName(server)
}

// loopbackName reports whether server is the one NAME judgeable without a
// resolver. RFC 6761 §6.3 reserves "localhost" AND every name under
// ".localhost", and REQUIRES resolution to a loopback address, so this name is
// loopback by definition rather than by lookup; no other hostname is, so this
// is not a heuristic that could grow. It earns its place: measured 2026-08-14
// over the 98 configured source URLs with the worker's UA, 15 published nodes
// name it outright, so without this the rule is defeated by the most obvious
// spelling of what it exists to catch.
//
// Matched over the LAST label so a ".localhost" subdomain counts, ASCII
// case-insensitively as DNS names compare, and without lowercasing the string,
// which would allocate once per node. A trailing dot ("localhost.") leaves an
// empty last label and answers false: that is the absolute form, and treating
// it as a miss costs one probe slot while a wrong accept would delete a node.
func loopbackName(server string) bool {
	name := server
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[i+1:]
	}
	if len(name) != len(loopbackHostname) {
		return false
	}
	for i := range len(name) {
		if name[i]|asciiCaseBit != loopbackHostname[i] {
			return false
		}
	}
	return true
}

// localV4 reports whether s is the unspecified address or a dotted quad in the
// loopback /8. String equality answers the unspecified case because that is the
// only in-scope address in 0.0.0.0/8, so every malformed all-zero spelling dies
// on a word compare instead of in netip.ParseAddr, which heap-boxes its error
// for 48 B a call — once per node, in Merge and in classify.Body.
func localV4(s string) bool {
	if s == unspecifiedV4 {
		return true
	}
	if !strings.HasPrefix(s, loopbackV4Prefix) {
		return false
	}
	_, ok := dottedQuad(s)
	return ok
}

// dottedQuad parses s under netip's IPv4 grammar exactly: four decimal groups,
// no leading zero in a multi-digit one, none above 255.
func dottedQuad(s string) ([4]byte, bool) {
	const (
		base10     = 10
		maxOctet   = 255
		quadGroups = 4
	)
	var quad [4]byte
	for g := range quadGroups {
		if g > 0 {
			if s == "" || s[0] != '.' {
				return quad, false
			}
			s = s[1:]
		}
		v, n := 0, 0
		for n < len(s) && s[n] >= '0' && s[n] <= '9' {
			if n == 1 && v == 0 {
				return quad, false
			}
			if v = v*base10 + int(s[n]-'0'); v > maxOctet {
				return quad, false
			}
			n++
		}
		if n == 0 {
			return quad, false
		}
		quad[g], s = byte(v), s[n:]
	}
	return quad, s == ""
}

// localV6 reports whether s spells the unspecified address or the IPv6
// loopback: zero groups with at most one "::" and an optional final group of 1,
// or mappedV4Prefix before a local IPv4 address. That covers every spelling
// netip.Addr.String emits for those, which is the boundary — a zoned form, a
// hex-written mapping or the deprecated embedded-quad form answers false and
// keeps its probe slot, the pre-gate behaviour, because the error this predicate
// must not make is dropping a working node.
func localV6(s string) bool {
	if rest, ok := cutMappedV4(s); ok {
		return localV4(rest)
	}
	i, groups, elided := 0, 0, false
	if s[0] == ':' {
		if len(s) < 2 || s[1] != ':' {
			return false
		}
		i, elided = 2, true
	}
	for i < len(s) {
		end, one, ok := zeroOrOneGroup(s, i)
		if !ok {
			return false
		}
		i, groups = end, groups+1
		// The loopback's 1 sits in the last group; anything after it is a
		// different address.
		if one && i < len(s) {
			return false
		}
		if i == len(s) {
			break
		}
		if i, elided, ok = v6Separator(s, i, elided); !ok {
			return false
		}
		if i == len(s) { // a trailing "::"
			break
		}
	}
	if elided {
		return groups < v6Groups // netip: the :: must expand to at least one group.
	}
	return groups == v6Groups
}

// zeroOrOneGroup reads the IPv6 group at s[i:], reporting where it ends, whether
// its value is 1, and whether it is a group this rule can accept at all: only 0
// and 1 can appear in :: or ::1, so any other value ends the walk immediately.
func zeroOrOneGroup(s string, i int) (end int, one, ok bool) {
	start := i
	for i < len(s) && (s[i] == '0' || s[i] == '1') {
		i++
	}
	if n := i - start; n == 0 || n > maxV6GroupDigits {
		return i, false, false
	}
	group := s[start:i]
	for len(group) > 1 && group[0] == '0' { // leading zeros are legal padding
		group = group[1:]
	}
	return i, group == "1", group == "0" || group == "1"
}

// v6Separator consumes the ':' or '::' after a group, reporting the new index,
// whether the one elision has now been spent, and whether the syntax holds.
func v6Separator(s string, i int, elided bool) (int, bool, bool) {
	if s[i] != ':' {
		return i, elided, false
	}
	if i++; i == len(s) {
		return i, elided, false // a single trailing colon
	}
	if s[i] != ':' {
		return i, elided, true
	}
	if elided {
		return i, elided, false // netip: multiple :: in one address
	}
	return i + 1, true, true
}

// cutMappedV4 strips mappedV4Prefix, matching its hex case-insensitively as
// netip does.
func cutMappedV4(s string) (string, bool) {
	if len(s) <= len(mappedV4Prefix) {
		return "", false
	}
	for i := range len(mappedV4Prefix) {
		if c, want := s[i], mappedV4Prefix[i]; c != want && (want != 'f' || c != 'F') {
			return "", false
		}
	}
	return s[len(mappedV4Prefix):], true
}
