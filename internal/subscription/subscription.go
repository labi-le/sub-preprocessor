package subscription

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"domains.lst/sub-preprocessor/internal/fetch"
	"domains.lst/sub-preprocessor/internal/ioutil"
)

const maxSubscriptionSize = 10 << 20

// doubleSlash is the URI scheme delimiter. Kept as a package-level
// var []byte so the compiler can reference it as a static constant
// in bytes.Contains calls without allocating.
var doubleSlash = []byte("://")

// Scheme is a strict URI scheme type.
type Scheme string

// Schemes whose server, port or display name does NOT live where the generic
// URI parser looks for it. Everything else is handled by the scheme-agnostic
// authority/fragment path; these three each need a dedicated decoder:
//
//	ss      - the legacy form base64-encodes "method:pass@host:port" as the
//	          whole authority, so there is no host to read
//	ssr     - base64-encodes host, port AND the display name (a "remarks"
//	          query param) in one payload, and carries no URI fragment
//	mierus  - the port list lives in the query, never in the authority
//
// SchemeVmess is declared in vmess.go beside its own decoder.
const (
	SchemeSS    Scheme = "ss"
	SchemeSSR   Scheme = "ssr"
	SchemeMieru Scheme = "mierus"
)

type Node struct {
	Raw         string
	Scheme      Scheme
	Name        string
	Server      string
	Port        string
	FragmentIdx int // index of '#' in Raw, or -1 if not present
}

func Load(ctx context.Context, rawURL fetch.SubscriptionURL) ([]byte, error) {
	body, err := fetch.BytesWithType(ctx, rawURL, maxSubscriptionSize, fetch.FileTypeRaw)
	if err != nil {
		return nil, fmt.Errorf("fetch subscription: %w", err)
	}
	return Normalize(body), nil
}

// Parse parses subscription body lines as URI nodes.
// Non-URI lines are skipped. Only lines containing "://" are parsed.
// It calls yield for each parsed node. If yield returns false, parsing stops.
//
// It returns how many URI-shaped lines parseNode refused. Those lines never
// reach yield, so without this count they would be invisible: a source that
// starts answering with a truncated body or a corrupted vmess payload would
// simply report fewer nodes with nothing accounting for the difference.
func Parse(body []byte, yield func(Node) bool) (rejected int) {
	it := ioutil.NewLines(body)
	for {
		line := it.Next()
		if line == nil {
			return rejected
		}
		if strings.Contains(ioutil.UnsafeString(line), "://") {
			node, ok := parseNode(ioutil.UnsafeString(line))
			if !ok {
				rejected++
				continue
			}
			if !yield(node) {
				return rejected
			}
		}
	}
}

// schemeSep separates the URI scheme from the authority.
const schemeSep = "://"

// parseNode extracts node fields from a URI string using a lightweight parser.
// It replaces url.Parse to avoid per-node heap allocations.
// One string alloc per call (for Node.Raw, reused by all string fields via substrings).
//
// Supported format: scheme://[userinfo@]host[:port][?query][#fragment]
func parseNode(line string) (Node, bool) {
	idx := strings.Index(line, schemeSep)
	if idx <= 0 || !validScheme(line[:idx]) {
		return Node{}, false
	}

	scheme := Scheme(line[:idx])
	if scheme == SchemeVmess {
		return parseVmess(line, idx)
	}
	rest := line[idx+len(schemeSep):]

	// Find end of authority section: '/', '?', '#', or end of string.
	// Explicit IndexByte calls are faster than IndexAny for short authority strings.
	hash := strings.IndexByte(rest, '#')
	authEnd := len(rest)
	if j := strings.IndexByte(rest, '/'); j >= 0 && j < authEnd {
		authEnd = j
	}
	if j := strings.IndexByte(rest, '?'); j >= 0 && j < authEnd {
		authEnd = j
	}
	if hash >= 0 && hash < authEnd {
		authEnd = hash
	}

	authority := rest[:authEnd]
	server, port := splitHostPort(authority)

	switch scheme {
	case SchemeSS:
		// A SIP002 link keeps its host in the authority ("<b64userinfo>@host");
		// without an '@' this is the legacy all-base64 form.
		if strings.IndexByte(authority, '@') < 0 {
			host, decodedPort, ok := decodeSSLegacy(authority)
			if !ok {
				return Node{}, false
			}
			server, port = host, decodedPort
		}
	case SchemeSSR:
		return parseSSR(line, rest)
	case SchemeMieru:
		queryPort, ok := mieruPort(rest[authEnd:])
		if !ok {
			return Node{}, false
		}
		port = queryPort
	case "http", "https", "socks", "socks5", "socks5h":
		// An HTTP/SOCKS proxy node is host:port by definition, and mihomo
		// refuses a portless one (convert/converter.go:543-546). Accepting it
		// publishes any bare web URL in a source body — a Telegram channel
		// link, a panel notice — as a node.
		if port == "" {
			return Node{}, false
		}
	}

	if server == "" {
		return Node{}, false
	}
	if port == "" {
		port = "443"
	}

	// Extract fragment (node name): everything after the FIRST '#' at or
	// after the authority. A later '#' inside the fragment stays part of the
	// name; a '#' before the scheme (e.g. commentary) is never a fragment.
	name := ""
	hashIdx := -1
	if hash >= 0 {
		hashIdx = idx + len(schemeSep) + hash
		name = strings.TrimSpace(line[hashIdx+1:])
	}
	if name == "" {
		name = server
	}

	return Node{Raw: line, Scheme: scheme, Name: name, Server: server, Port: port, FragmentIdx: hashIdx}, true
}

// validScheme reports whether s has the RFC 3986 scheme shape
// ALPHA *( ALPHA / DIGIT / "+" / "-" / "." ). The parser stays scheme-generic —
// it does not care WHICH scheme a line carries — but "everything before the
// first ://" is not a scheme: a subscription URL that starts answering with an
// HTML page or a Clash YAML document contains URLs too, and without this check
// `<a href="https://example.com">` parses as a node with scheme `<a href="https`
// and the source looks healthy while publishing markup.
func validScheme(s string) bool {
	if len(s) == 0 {
		return false
	}
	if c := s[0]; (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		alnum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !alnum && c != '+' && c != '-' && c != '.' {
			return false
		}
	}
	return true
}

// splitHostPort separates host and port from an authority string.
// Handles userinfo (user@host) and IPv6 ([::1]:port) formats.
func splitHostPort(authority string) (host, port string) {
	if j := strings.LastIndexByte(authority, '@'); j >= 0 {
		authority = authority[j+1:]
	}
	if authority == "" {
		return "", ""
	}

	// Handle IPv6: [::1]:port. The returned host carries no brackets so it
	// can feed the resolver and blocklist directly.
	if authority[0] == '[' {
		if j := strings.IndexByte(authority, ']'); j >= 0 {
			host = authority[1:j]
			if j+1 < len(authority) && authority[j+1] == ':' {
				port = authority[j+2:]
			}
			return host, port
		}
		return "", "" // malformed IPv6
	}

	if j := strings.LastIndexByte(authority, ':'); j >= 0 {
		// Multiple colons without brackets mean a bare IPv6 address, which
		// cannot carry a port; splitting at the last colon would truncate it.
		if strings.IndexByte(authority[:j], ':') >= 0 {
			return authority, ""
		}
		return authority[:j], authority[j+1:]
	}

	return authority, ""
}

func Normalize(body []byte) []byte {
	body = bytes.TrimSpace(body)
	if converted, ok := maybeXrayJSON(body); ok {
		return converted
	}
	if bytes.Contains(body, doubleSlash) {
		return body
	}

	s := stripWhitespace(ioutil.UnsafeString(body))

	if decoded, ok := decodeBase64Tolerant(s); ok {
		decoded = bytes.TrimSpace(decoded)
		if converted, convOK := maybeXrayJSON(decoded); convOK {
			return converted
		}
		if bytes.Contains(decoded, doubleSlash) {
			return decoded
		}
	}

	return body
}

func stripWhitespace(s string) string {
	for i := range len(s) {
		switch s[i] {
		case ' ', '\t', '\n', '\r':
			goto slow
		}
	}
	return s

slow:
	var b strings.Builder
	b.Grow(len(s))
	for i := range len(s) {
		switch s[i] {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
