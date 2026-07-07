package subscription

import (
	"bytes"
	"context"
	"encoding/base64"
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
func Parse(body []byte, yield func(Node) bool) {
	it := ioutil.NewLines(body)
	for {
		line := it.Next()
		if line == nil {
			return
		}
		if strings.Contains(ioutil.UnsafeString(line), "://") {
			if node, ok := parseNode(ioutil.UnsafeString(line)); ok {
				if !yield(node) {
					return
				}
			}
		}
	}
}

// parseNode extracts node fields from a URI string using a lightweight parser.
// It replaces url.Parse to avoid per-node heap allocations.
// One string alloc per call (for Node.Raw, reused by all string fields via substrings).
//
// Supported format: scheme://[userinfo@]host[:port][?query][#fragment]
func parseNode(line string) (Node, bool) {
	idx := strings.Index(line, "://")
	if idx <= 0 {
		return Node{}, false
	}

	scheme := Scheme(line[:idx])
	if scheme == SchemeVmess {
		return parseVmess(line, idx)
	}
	rest := line[idx+3:] // after "://"

	// Find end of authority section: '/', '?', '#', or end of string.
	// Explicit IndexByte calls are faster than IndexAny for short authority strings.
	authEnd := len(rest)
	if j := strings.IndexByte(rest, '/'); j >= 0 && j < authEnd {
		authEnd = j
	}
	if j := strings.IndexByte(rest, '?'); j >= 0 && j < authEnd {
		authEnd = j
	}
	if j := strings.IndexByte(rest, '#'); j >= 0 && j < authEnd {
		authEnd = j
	}

	authority := rest[:authEnd]
	if authority == "" {
		return Node{}, false
	}

	server, port := splitHostPort(authority)
	if server == "" {
		return Node{}, false
	}
	if port == "" {
		port = "443"
	}

	// Extract fragment (node name) from the original line.
	name := ""
	hashIdx := strings.LastIndexByte(line, '#')
	if hashIdx >= 0 {
		name = strings.TrimSpace(line[hashIdx+1:])
	} else {
		hashIdx = -1
	}
	if name == "" {
		name = server
	}

	return Node{Raw: line, Scheme: scheme, Name: name, Server: server, Port: port, FragmentIdx: hashIdx}, true
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

	// Handle IPv6: [::1]:port
	if authority[0] == '[' {
		if j := strings.IndexByte(authority, ']'); j >= 0 {
			host = authority[:j+1]
			if j+1 < len(authority) && authority[j+1] == ':' {
				port = authority[j+2:]
			}
			return host, port
		}
		return "", "" // malformed IPv6
	}

	// Regular host:port. LastIndexByte is safe here because IPv6 with
	// brackets was handled above.
	if j := strings.LastIndexByte(authority, ':'); j >= 0 {
		host = authority[:j]
		port = authority[j+1:]
		return host, port
	}

	return authority, ""
}

func Normalize(body []byte) []byte {
	body = bytes.TrimSpace(body)
	if bytes.Contains(body, doubleSlash) {
		return body
	}

	s := stripWhitespace(ioutil.UnsafeString(body))

	if decoded, err := base64.StdEncoding.DecodeString(s); err == nil && bytes.Contains(decoded, doubleSlash) {
		return bytes.TrimSpace(decoded)
	}
	if decoded, err := base64.RawStdEncoding.DecodeString(s); err == nil && bytes.Contains(decoded, doubleSlash) {
		return bytes.TrimSpace(decoded)
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
