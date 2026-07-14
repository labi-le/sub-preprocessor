package subscription

import (
	"encoding/base64"
	"encoding/json"
	"strings"
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
	return "vmess://" + base64.StdEncoding.EncodeToString(out), true
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

// decodeBase64Tolerant tries the standard and URL base64 alphabets, padded and
// unpadded, because subscription and vmess producers are inconsistent about
// which they emit. Shared by Normalize and decodeVmessJSON.
func decodeBase64Tolerant(s string) ([]byte, bool) {
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		if d, err := enc.DecodeString(s); err == nil {
			return d, true
		}
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
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}
