package subscription

import (
	"encoding/base64"
	"strings"

	"domains.lst/sub-preprocessor/internal/ioutil"
)

// ssrQueryHint sizes the array an ssr query is parsed into: mihomo reads
// obfsparam, protoparam, remarks and group, and payloads in the wild add
// udpport and uot.
const ssrQueryHint = 8

// ssrURLSafe maps the std base64 alphabet onto the url-safe one. An ssr query
// carries base64 values ("remarks", "obfsparam", "protoparam", "group") that
// the query parser would mangle in the std alphabet: '+' decodes to a space.
// mihomo runs the same substitution before parsing (convert/base64.go:36).
var ssrURLSafe = strings.NewReplacer("+", "-", "/", "_")

// parseSSR decodes an ssr:// share link. Its payload is base64 of
// "host:port:protocol:method:obfs:password/?query", so nothing the node needs —
// not even the host — is readable from the URI itself; the generic authority
// path reads the base64 blob as a hostname and every ssr node dies in DNS.
func parseSSR(line, payload string) (Node, bool) {
	var scratch [ssrQueryHint]queryPair
	head, query, ok := decodeSSR(payload, scratch[:0])
	if !ok {
		return Node{}, false
	}

	server, rest, _ := strings.Cut(head, ":")
	port, _, _ := strings.Cut(rest, ":")
	// The port is the one head field adapter.ParseProxy decodes itself
	// (strconv.ParseInt through the structure decoder), so a non-numeric one
	// converts to a mapping the prober cannot build: the node merges, is
	// published, is skipped behind a single "skipped unparsable proxies" log
	// line, and still books "host:<garbage>" into the 2h dead cache.
	//
	// The 1..65535 window is deliberately stricter than that decode, which
	// takes 0, -1 and 70000 too — NewShadowSocksR only JoinHostPorts the int
	// and has no range check of its own, unlike mieru's validateMieruOption.
	// Nothing can dial those, so the honest reject costs no reachable node and
	// keeps Entry.Addr a real server:port.
	_, portOK := portNumber(port)
	if server == "" || !portOK {
		return Node{}, false
	}

	name := ssrRemarks(query)
	if name == "" {
		name = server
	}

	// Unlike vmess, FragmentIdx keeps its generic meaning: an ssr fragment is
	// plain text in Raw, not part of the payload. Relabeling still goes through
	// RewriteSSRName, which drops it.
	return Node{
		Raw:         line,
		Scheme:      SchemeSSR,
		Name:        name,
		Server:      server,
		Port:        port,
		FragmentIdx: strings.IndexByte(line, '#'),
	}, true
}

// RewriteSSRName returns an ssr:// line identical to raw except its "remarks"
// query parameter — the display name, which lives inside the base64 payload
// rather than a URI fragment — is set to newName. It returns false when raw is
// not a decodable ssr payload.
//
// The result carries no fragment and is encoded with the unpadded url-safe
// alphabet on purpose: mihomo base64-decodes EVERYTHING after "ssr://",
// fragment included, and reads "remarks" with RawURLEncoding, so a fragment or
// a '=' pad (which query escaping turns into "%3D") makes the link
// unconvertible.
func RewriteSSRName(raw, newName string) (string, bool) {
	_, payload, found := strings.Cut(raw, schemeSep)
	if !found {
		return "", false
	}
	var scratch [ssrQueryHint]queryPair
	head, query, ok := decodeSSR(payload, scratch[:0])
	if !ok {
		return "", false
	}

	query = query.set("remarks", base64.RawURLEncoding.EncodeToString([]byte(newName)))

	// rawLen is exact for a query whose values are all base64, which is every
	// ssr query: nothing in that alphabet escapes, so plain never grows.
	const sep = "/?"
	plain := make([]byte, 0, len(head)+len(sep)+query.rawLen())
	plain = append(plain, head...)
	plain = append(plain, sep...)
	plain = query.appendEncoded(plain)

	const scheme = "ssr://"
	buf := make([]byte, 0, len(scheme)+base64.RawURLEncoding.EncodedLen(len(plain)))
	buf = append(buf, scheme...)
	buf = base64.RawURLEncoding.AppendEncode(buf, plain)
	return ioutil.UnsafeString(buf), true
}

// decodeSSR splits a decoded ssr payload into its colon-separated head and the
// PARSED query following "/?". The "/?", the exactly-6-fields and the net/url
// query grammar are all mihomo's
// (convert/converter.go:483-504) and are mirrored deliberately: a node we keep
// but the prober cannot convert burns probe budget and can never be published,
// which is worse than an honest reject.
//
// Parsing here rather than in each caller is what makes accepting a node and
// relabelling it ONE decision: RewriteSSRName reuses these values, so it can no
// longer refuse a payload parseSSR let through. The query is mapped onto the
// url-safe alphabet first, exactly as mihomo does, so a '+' inside a base64
// value survives instead of decoding to a space.
//
// The optional trailing "#name" is stripped before decoding. mihomo does not do
// that and fails on such a link, but both of our output paths re-emit ssr nodes
// through RewriteSSRName, which drops the fragment.
func decodeSSR(payload string, scratch queryList) (head string, query queryList, ok bool) {
	if i := strings.IndexByte(payload, '#'); i >= 0 {
		payload = payload[:i]
	}
	decoded, ok := decodeBase64Tolerant(stripWhitespace(payload))
	if !ok {
		return "", nil, false
	}
	head, rawQuery, ok := strings.Cut(ioutil.UnsafeString(decoded), "/?")
	if !ok {
		return "", nil, false
	}
	// An ssr head is exactly "host:port:protocol:method:obfs:password", and 5
	// separators is 6 fields — counted rather than Split to avoid the slice.
	const ssrHeadSeparators = 5
	if strings.Count(head, ":") != ssrHeadSeparators {
		return "", nil, false
	}
	query, ok = scratch.parse(ssrURLSafeQuery(rawQuery))
	if !ok {
		return "", nil, false
	}
	return head, query, true
}

// ssrRemarks returns the decoded "remarks" value, the display name an ssr link
// carries instead of a fragment. mihomo decodes it with RawURLEncoding after
// mapping the std alphabet onto the url-safe one — a mapping decodeSSR has
// already applied — so both alphabets are accepted; an undecodable value yields
// "" and the caller falls back to the host, as a name is never worth rejecting
// a reachable node over.
func ssrRemarks(query queryList) string {
	decoded, ok := decodeBase64Tolerant(query.get("remarks"))
	if !ok {
		return ""
	}
	return strings.TrimSpace(ioutil.UnsafeString(decoded))
}

func ssrURLSafeQuery(query string) string {
	if !strings.ContainsAny(query, "+/") {
		return query
	}
	return ssrURLSafe.Replace(query)
}
