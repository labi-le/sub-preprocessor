package subscription

import (
	"strings"

	"domains.lst/sub-preprocessor/internal/ioutil"
)

// decodeSSLegacy reads the server and port of a pre-SIP002 ss:// link, whose
// whole authority is base64 of "method:password@host:port" — there is no host
// in the URI to read. mihomo takes the same branch whenever url.Port() comes
// back empty (convert/converter.go:397-407).
//
// It reports false when the payload does not decode or carries no "@": the
// generic path would then hand the base64 blob to the resolver as a hostname,
// which books an NXDOMAIN drop and loses the node on both endpoints.
func decodeSSLegacy(authority string) (server, port string, ok bool) {
	decoded, ok := decodeBase64Tolerant(authority)
	if !ok {
		return "", "", false
	}
	plain := ioutil.UnsafeString(decoded)
	if strings.IndexByte(plain, '@') < 0 {
		return "", "", false
	}
	// splitHostPort cuts at the LAST '@', so a password containing one is safe.
	server, port = splitHostPort(plain)
	if server == "" {
		return "", "", false
	}
	return server, port, true
}
