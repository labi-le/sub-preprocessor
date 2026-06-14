package subscription

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"domains.lst/sub-preprocessor/internal/fetch"
)

const maxSubscriptionSize = 10 << 20

type Node struct {
	Raw    string
	Scheme string
	Name   string
	Server string
	Port   string
}

func Load(ctx context.Context, rawURL string) ([]Node, error) {
	body, err := fetch.BytesWithType(ctx, rawURL, maxSubscriptionSize, fetch.FileTypeRaw)
	if err != nil {
		return nil, fmt.Errorf("fetch subscription: %w", err)
	}
	return Parse(Normalize(body))
}

// Parse parses subscription body lines as URI nodes.
// Non-URI lines are skipped. Only lines containing "://" are parsed.
func Parse(body []byte) ([]Node, error) {
	nlCount := bytes.Count(body, []byte{'\n'})
	nodes := make([]Node, 0, nlCount)

	remain := body
	for {
		idx := bytes.IndexByte(remain, '\n')
		var line []byte
		if idx < 0 {
			line = bytes.TrimSpace(remain)
		} else {
			line = bytes.TrimSpace(remain[:idx])
			remain = remain[idx+1:]
		}

		if len(line) == 0 || line[0] == '#' {
			if idx < 0 {
				break
			}
			continue
		}

		if !bytes.Contains(line, []byte("://")) {
			if idx < 0 {
				break
			}
			continue
		}

		if node, ok := parseNode(string(line)); ok {
			nodes = append(nodes, node)
		}

		if idx < 0 {
			break
		}
	}

	if len(nodes) == 0 {
		return nil, errors.New("no supported URI nodes found")
	}
	return nodes, nil
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

	scheme := line[:idx]
	rest := line[idx+3:] // after "://"

	// Find end of authority section: '/', '?', '#', or end of string.
	authEnd := len(rest)
	for _, sep := range []byte{'/', '?', '#'} {
		if j := strings.IndexByte(rest, sep); j >= 0 && j < authEnd {
			authEnd = j
		}
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
	if j := strings.LastIndexByte(line, '#'); j >= 0 {
		name = strings.TrimSpace(line[j+1:])
	}
	if name == "" {
		name = server
	}

	return Node{Raw: line, Scheme: scheme, Name: name, Server: server, Port: port}, true
}

// splitHostPort separates host and port from an authority string.
// Handles userinfo (user@host) and IPv6 ([::1]:port) formats.
func splitHostPort(authority string) (host, port string) {
	// Strip userinfo.
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
	if bytes.Contains(body, []byte("://")) {
		return body
	}

	compact := unsafe.String(unsafe.SliceData(body), len(body))
	compact = strings.ReplaceAll(compact, "\n", "")
	compact = strings.ReplaceAll(compact, "\r", "")
	compact = strings.ReplaceAll(compact, "\t", "")
	compact = strings.ReplaceAll(compact, " ", "")

	if decoded, err := base64.StdEncoding.DecodeString(compact); err == nil && bytes.Contains(decoded, []byte("://")) {
		return bytes.TrimSpace(decoded)
	}
	if decoded, err := base64.RawStdEncoding.DecodeString(compact); err == nil && bytes.Contains(decoded, []byte("://")) {
		return bytes.TrimSpace(decoded)
	}

	return body
}
