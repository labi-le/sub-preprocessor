package subscription

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"

	"domains.lst/sub-preprocessor/internal/ioutil"
)

// SchemeVmess identifies vmess:// nodes. Unlike vless/trojan/ss the server,
// port and display name live inside a base64-encoded JSON payload rather than
// a URI authority, so vmess needs a dedicated parser and relabeler.
const SchemeVmess Scheme = "vmess"

// parseVmess decodes a vmess:// share link whose payload after the scheme is
// base64 JSON of the form {"add":host,"port":port,"ps":name,...}.
func parseVmess(line string, schemeEnd int) (Node, bool) {
	m, ok := decodeVmessJSON(line[schemeEnd+3:])
	if !ok {
		return Node{}, false
	}

	server := jsonFieldString(m, "add")
	if server == "" {
		return Node{}, false
	}
	port := jsonFieldString(m, "port")
	if port == "" {
		port = "443"
	}
	name := jsonFieldString(m, "ps")
	if name == "" {
		name = server
	}

	return Node{Raw: line, Scheme: SchemeVmess, Name: name, Server: server, Port: port, FragmentIdx: -1}, true
}

// RewriteVmessName returns a vmess:// line identical to raw except its "ps"
// (display name) field is set to newName, re-encoding the JSON payload.
// Downstream consumers that key nodes by name (the mihomo prober) then see the
// intended label. It returns false when raw is not a decodable vmess payload.
func RewriteVmessName(raw, newName string) (string, bool) {
	_, payload, found := strings.Cut(raw, "://")
	if !found {
		return "", false
	}
	m, ok := decodeVmessJSON(payload)
	if !ok {
		return "", false
	}

	nameJSON, err := json.Marshal(newName)
	if err != nil {
		return "", false
	}
	m["ps"] = nameJSON

	out, err := json.Marshal(m)
	if err != nil {
		return "", false
	}

	// AppendEncode base64 directly after the "vmess://" scheme into one freshly
	// owned buffer, avoiding a separate EncodeToString allocation and the
	// string concatenation. The buffer is never mutated after this point, so
	// UnsafeString hands it back without a copy.
	const scheme = "vmess://"
	buf := make([]byte, 0, len(scheme)+base64.StdEncoding.EncodedLen(len(out)))
	buf = append(buf, scheme...)
	buf = base64.StdEncoding.AppendEncode(buf, out)
	return ioutil.UnsafeString(buf), true
}

// decodeVmessJSON strips an optional trailing fragment, base64-decodes the
// payload and unmarshals it into a raw-message map so unknown fields survive a
// round-trip. A JSON null or non-object payload is rejected as unusable.
func decodeVmessJSON(payload string) (map[string]json.RawMessage, bool) {
	if i := strings.IndexByte(payload, '#'); i >= 0 {
		payload = payload[:i]
	}
	decoded, ok := decodeBase64Tolerant(stripWhitespace(payload))
	if !ok {
		return nil, false
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(decoded, &m); err != nil || m == nil {
		return nil, false
	}
	return m, true
}

// decodeBase64Tolerant decodes s under whichever base64 flavour its producer
// used. Rather than trying up to four alphabets (each failed attempt still
// allocates a scratch buffer), it selects a single encoding from cheap
// structural cues: the URL alphabet iff s contains '-' or '_' (the only
// characters that distinguish it from std, since the shared A-Za-z0-9 run
// decodes identically either way), and the padded variant iff len(s) is a
// multiple of four (padded encodings require that; the unpadded/raw variants
// cover the remainder). This preserves the previous accept/reject set and the
// first-match precedence while doing at most one decode.
func decodeBase64Tolerant(s string) ([]byte, bool) {
	var enc *base64.Encoding
	switch {
	case len(s)%4 == 0 && strings.ContainsAny(s, "-_"):
		enc = base64.URLEncoding
	case len(s)%4 == 0:
		enc = base64.StdEncoding
	case strings.ContainsAny(s, "-_"):
		enc = base64.RawURLEncoding
	default:
		enc = base64.RawStdEncoding
	}
	if d, err := enc.DecodeString(s); err == nil {
		return d, true
	}
	return nil, false
}

// jsonFieldString reads a field as a string, accepting both JSON strings and
// bare numbers (vmess "port" appears in the wild as both "443" and 443).
func jsonFieldString(m map[string]json.RawMessage, key string) string {
	raw, ok := m[key]
	if !ok {
		return ""
	}
	// Fast path: an escape-free JSON string is its own content minus the
	// surrounding quotes, so alias the raw-message bytes directly instead of
	// letting json.Unmarshal allocate a decoded copy. The bytes are immutable
	// after unmarshalling and stay alive via the returned string.
	if n := len(raw); n >= 2 && raw[0] == '"' && raw[n-1] == '"' {
		if inner := raw[1 : n-1]; bytes.IndexByte(inner, '\\') < 0 {
			return ioutil.UnsafeString(inner)
		}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return strings.TrimSpace(ioutil.UnsafeString(raw))
}
