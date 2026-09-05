package subscription

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"domains.lst/sub-preprocessor/internal/ioutil"
)

// SchemeVmess identifies vmess:// nodes. Unlike vless/trojan the server, port
// and display name live inside a base64-encoded JSON payload rather than a URI
// authority, so vmess needs a dedicated parser and relabeler. Legacy ss and ssr
// hide the same fields the same way and have their own decoders (ss.go/ssr.go);
// vmess is only the oldest of the three.
const SchemeVmess Scheme = "vmess"

// parseVmess decodes a vmess:// share link whose payload after the scheme is
// base64 JSON of the form {"add":host,"port":port,"ps":name,...}.
func parseVmess(line string, schemeEnd int) (Node, bool) {
	payload := line[schemeEnd+len(schemeSep):]
	doc, ok := decodeVmessPayload(payload)
	if !ok {
		return Node{}, false
	}
	fields, ok := vmessFields(doc)
	if !ok {
		return Node{}, false
	}

	server := jsonValueString(fields.add)
	if server == "" {
		return Node{}, false
	}
	port := jsonValueString(fields.port)
	if port == "" {
		port = "443"
	}
	// Every other scheme takes its name from the URI fragment; vmess normally
	// carries it in the payload's "ps", but a link that omits "ps" and labels
	// itself in the fragment is still naming the node, so prefer that over the
	// bare host. FragmentIdx stays -1: rewrite folds the vmess name back into
	// "ps" and emits no fragment, so there is nothing for it to point at.
	name := jsonValueString(fields.ps)
	if name == "" {
		if _, frag, found := strings.Cut(payload, "#"); found {
			name = strings.TrimSpace(frag)
		}
	}
	if name == "" {
		name = server
	}

	return Node{Raw: line, Scheme: SchemeVmess, Name: name, Server: server, Port: port, FragmentIdx: -1}, true
}

// RewriteVmessName returns a vmess:// line identical to raw except its "ps"
// (display name) field is set to newName, re-encoding the base64 payload.
// Downstream consumers that key nodes by name (the mihomo prober) then see the
// intended label. It returns false when raw is not a decodable vmess payload.
//
// The new name is spliced over the old value rather than marshalled from a
// map[string]json.RawMessage: that round trip was 89% of this function's
// alloc_objects on 2026-08-18 (json.Unmarshal 68%, json.Marshal 21%) and
// re-encoded the whole document to change one field. Splicing keeps every other
// byte — unknown fields, their order, their spelling — as the producer wrote it.
// vmessFields gates the payload, so the accept set stays the map decode's:
// TestVmessFieldsAgreeWithMapDecode pins the two together document by document.
func RewriteVmessName(raw, newName string) (string, bool) {
	return rewriteVmessName(raw, "", newName)
}

// RewriteVmessNameTagged is RewriteVmessName for a display name held as two
// parts — the annotation prefix and the cleaned node name — which the payload
// arms of rewrite.NodeName call instead of joining them first. The spliced
// name is byte-identical to RewriteVmessName(raw, tags+" "+cleanName), and to
// cleanName alone when tags is empty: only the join is skipped, so the tagged
// form is composed inside the name scratch rather than as one heap string per
// annotated node.
func RewriteVmessNameTagged(raw, tags, cleanName string) (string, bool) {
	return rewriteVmessName(raw, tags, cleanName)
}

func rewriteVmessName(raw, tags, cleanName string) (string, bool) {
	_, payload, found := strings.Cut(raw, schemeSep)
	if !found {
		return "", false
	}
	doc, ok := decodeVmessPayload(payload)
	if !ok {
		return "", false
	}
	fields, ok := vmessFields(doc)
	if !ok {
		return "", false
	}

	// Encoding the name first makes plain's capacity exact, and the scratch is
	// wide enough for a tagged label so the common name never reaches the heap.
	var scratch [nameScratch]byte
	nameJSON, ok := appendJSONName(scratch[:0], tags, cleanName)
	if !ok {
		return "", false
	}

	const psMember = `"ps":`
	plain := make([]byte, 0, len(doc)+len(psMember)+len(nameJSON)+len(","))
	if fields.psAt >= 0 {
		plain = append(plain, doc[:fields.psAt]...)
		plain = append(plain, nameJSON...)
		plain = append(plain, doc[fields.psAt+len(fields.ps):]...)
	} else {
		open := skipJSONSpace(doc, 0) + 1
		plain = append(plain, doc[:open]...)
		plain = append(plain, psMember...)
		plain = append(plain, nameJSON...)
		if doc[skipJSONSpace(doc, open)] != '}' {
			plain = append(plain, ',')
		}
		plain = append(plain, doc[open:]...)
	}

	// AppendEncode base64 directly after the "vmess://" scheme into one freshly
	// owned buffer, avoiding a separate EncodeToString allocation and the
	// string concatenation. The buffer is never mutated after this point, so
	// UnsafeString hands it back without a copy.
	const scheme = "vmess://"
	buf := make([]byte, 0, len(scheme)+base64.StdEncoding.EncodedLen(len(plain)))
	buf = append(buf, scheme...)
	buf = base64.StdEncoding.AppendEncode(buf, plain)
	return ioutil.UnsafeString(buf), true
}

// nameScratch bounds the stack buffer a relabel display name is composed in:
// JSON-encoded for the vmess splice, raw for the ssr remarks. A relabelled
// name is the upstream one behind a "[GEO:xx][SPD:nM] " prefix, which the
// shipped tags keep well inside this.
const nameScratch = 128

// appendJSONString appends s as a JSON string. The escape-free path is the
// point: json.Marshal allocates an encodeState and a returned copy, and a
// display name — even an emoji one — is almost always a string it would emit
// verbatim between quotes. jsonPlainString decides that, and
// TestAppendJSONStringMatchesMarshal pins the two forms together.
func appendJSONString(dst []byte, s string) ([]byte, bool) {
	if jsonPlainString(s) {
		dst = append(dst, '"')
		dst = append(dst, s...)
		return append(dst, '"'), true
	}
	encoded, err := json.Marshal(s)
	if err != nil {
		return dst, false
	}
	return append(dst, encoded...), true
}

// appendJSONName appends the JSON encoding of the display name a payload arm
// publishes: cleanName alone, or tags+" "+cleanName when tags is non-empty.
// rewrite.NodeName used to hand that join over as one string, which escaped to
// the heap per annotated node; writing the two parts here keeps the tagged
// form in the caller's scratch whenever both are jsonPlainString — every real
// name. A part json.Marshal would escape falls back to marshalling the join,
// byte-identical to the old path and costing the concat only there.
func appendJSONName(dst []byte, tags, cleanName string) ([]byte, bool) {
	if tags == "" {
		return appendJSONString(dst, cleanName)
	}
	if jsonPlainString(tags) && jsonPlainString(cleanName) {
		dst = append(dst, '"')
		dst = append(dst, tags...)
		dst = append(dst, ' ')
		dst = append(dst, cleanName...)
		return append(dst, '"'), true
	}
	return appendJSONString(dst, tags+" "+cleanName)
}

// jsonPlainString reports whether json.Marshal emits s as itself between
// quotes. Deliberately conservative — a false negative only costs the marshal —
// so it demands printable ASCII outside the bytes encoding/json escapes ('"',
// '\\' and the three HTML ones), and for non-ASCII valid UTF-8 that is neither
// U+2028 nor U+2029, the two runes Marshal escapes anyway.
func jsonPlainString(s string) bool {
	const del = 0x7f
	for i := 0; i < len(s); {
		if c := s[i]; c < utf8.RuneSelf {
			if c < ' ' || c == del || c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' {
				return false
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if (r == utf8.RuneError && size == 1) || r == '\u2028' || r == '\u2029' {
			return false
		}
		i += size
	}

	return true
}

// decodeVmessJSON is the reference decode the field walker and the name splice
// are pinned against (vmess_internal_test.go). Nothing on the parse or rewrite
// path comes through here: the map form copies every field of a document whose ~16
// fields three are read from, at a measured 55 allocations and 3050 B per node
// against a ~400 B line.
func decodeVmessJSON(payload string) (map[string]json.RawMessage, bool) {
	decoded, ok := decodeVmessPayload(payload)
	if !ok {
		return nil, false
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(decoded, &m); err != nil || m == nil {
		return nil, false
	}
	return m, true
}

// decodeVmessPayload strips an optional trailing fragment and base64-decodes
// what is left.
func decodeVmessPayload(payload string) ([]byte, bool) {
	if i := strings.IndexByte(payload, '#'); i >= 0 {
		payload = payload[:i]
	}
	return decodeBase64Tolerant(stripWhitespace(payload))
}

// vmessScalars holds the three fields parseVmess needs, each a raw JSON view
// into the decoded payload rather than a copy of it. psAt is where that view
// starts, which is what lets RewriteVmessName splice a new name over it; it is
// -1 when the document carries no "ps".
type vmessScalars struct {
	add, port, ps []byte
	psAt          int
}

// vmessFields reads add/port/ps off a decoded vmess payload.
//
// json.Valid runs first, so the walk below only ever sees a document
// encoding/json would have accepted and needs no error paths of its own: the
// accept set is the map decode's, minus nothing. Key matching stays
// BYTE-EXACT because mihomo's own vmess decode is a map decode reading
// values["add"] (common/convert/converter.go:274) — a struct decode would match
// "Add" case-insensitively and keep a node mihomo converts to nothing, which
// costs a probe and a dead-cache entry.
//
// A repeated key resolves to the LAST occurrence, as it does in a map decode.
func vmessFields(doc []byte) (vmessScalars, bool) {
	out := vmessScalars{psAt: -1}
	if !json.Valid(doc) {
		return out, false
	}
	i := skipJSONSpace(doc, 0)
	if i == len(doc) || doc[i] != '{' {
		return out, false
	}

	for i++; ; {
		i = skipJSONSpace(doc, i)
		if i == len(doc) || doc[i] == '}' {
			return out, true
		}
		keyEnd := endOfJSONString(doc, i)
		key := doc[i:keyEnd]
		i = skipJSONSpace(doc, skipJSONSpace(doc, keyEnd)+1) // past ':'
		valEnd := endOfJSONValue(doc, i)
		switch {
		case jsonKeyEquals(key, "add"):
			out.add = doc[i:valEnd]
		case jsonKeyEquals(key, "port"):
			out.port = doc[i:valEnd]
		case jsonKeyEquals(key, "ps"):
			out.ps, out.psAt = doc[i:valEnd], i
		}
		i = skipJSONSpace(doc, valEnd)
		if i < len(doc) && doc[i] == ',' {
			i++
		}
	}
}

func skipJSONSpace(doc []byte, i int) int {
	for ; i < len(doc); i++ {
		switch doc[i] {
		case ' ', '\t', '\r', '\n':
		default:
			return i
		}
	}
	return i
}

// endOfJSONString returns the index just past the closing quote of the string
// starting at doc[i].
func endOfJSONString(doc []byte, i int) int {
	for i++; i < len(doc); i++ {
		switch doc[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return len(doc)
}

// endOfJSONValue returns the index just past the value starting at doc[i].
// Nested objects and arrays are skipped whole, so a "ps" inside a transport
// sub-object is not mistaken for the node's own display name.
func endOfJSONValue(doc []byte, i int) int {
	switch doc[i] {
	case '"':
		return endOfJSONString(doc, i)
	case '{', '[':
		depth := 0
		for ; i < len(doc); i++ {
			switch doc[i] {
			case '"':
				i = endOfJSONString(doc, i) - 1
			case '{', '[':
				depth++
			case '}', ']':
				if depth--; depth == 0 {
					return i + 1
				}
			}
		}
		return len(doc)
	}
	for ; i < len(doc); i++ {
		switch doc[i] {
		case ',', '}', ']', ' ', '\t', '\r', '\n':
			return i
		}
	}
	return len(doc)
}

// jsonKeyEquals reports whether the quoted key token names want. An escaped key
// is legal and a map decode unescapes it before the lookup, so "\u0061dd" has
// to keep reading as "add"; that form costs one allocation and appears in no
// real payload, while the escape-free comparison costs none.
func jsonKeyEquals(token []byte, want string) bool {
	inner := token[1 : len(token)-1]
	if bytes.IndexByte(inner, '\\') < 0 {
		return string(inner) == want
	}
	var key string
	return json.Unmarshal(token, &key) == nil && key == want
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

// jsonValueString reads a raw JSON value as a string, accepting JSON strings
// and bare numbers (vmess "port" appears in the wild as both "443" and 443).
// An absent field is a nil value and reads as "". A bare number is returned
// verbatim without reaching the decoder — a number can never unmarshal into a
// string, so the numeric form would pay a heap &UnmarshalTypeError for
// nothing. null, bool, object and array values are not text mihomo would ever
// decode into a server or port, so they read as "" too (null already does:
// json.Unmarshal accepts it as a no-op into a string, which is how an absent
// field reads as "").
func jsonValueString(raw []byte) string {
	if len(raw) == 0 {
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
	// A bare number is vmess's other "port" spelling (443 alongside "443"),
	// and it can never decode into a string: json.Unmarshal answers the
	// number-into-string case with a heap &UnmarshalTypeError, so the raw-text
	// return has to run BEFORE the Unmarshal — every real numeric node paid
	// that doomed decode when it ran first. The document is json.Valid by the
	// time this runs, so the first byte decides what the token is; null,
	// "true", an object and an array still fall through to the Unmarshal
	// below, which keeps them reading as "" rather than literal text.
	if c := raw[0]; c == '-' || (c >= '0' && c <= '9') {
		return strings.TrimSpace(ioutil.UnsafeString(raw))
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return ""
}
