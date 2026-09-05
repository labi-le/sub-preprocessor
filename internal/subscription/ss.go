package subscription

import (
	"encoding/base64"
	"strings"

	"domains.lst/sub-preprocessor/internal/ioutil"
)

// decodeSSLegacy reads the server and port of a pre-SIP002 ss:// link, whose
// whole authority is base64 of "method:password@host:port" — there is no host
// in the URI to read. mihomo takes the same branch whenever url.Port() comes
// back empty (convert/converter.go:397-407).
//
// RawStdEncoding is the only alphabet accepted, deliberately narrower than
// decodeBase64Tolerant. The shadowsocks spec writes the legacy form as
// "ss://BASE64-ENCODED-STRING-WITHOUT-PADDING#TAG"
// (https://shadowsocks.org/doc/configs.html) and mihomo decodes the authority
// with base64.RawStdEncoding alone (convert/converter.go:398), so a padded or
// url-safe payload is both off-spec and unusable by the only client we publish
// for: it would pass merge, spend a probe, yield no proxy, and leave its
// server:port in the 2h dead cache. The tolerant decoder stays right for
// vmess, whose mihomo counterpart is tolerant too.
//
// It reports false when the payload does not decode, carries no "@", or names
// no port: the generic path would then hand the base64 blob to the resolver as
// a hostname — an NXDOMAIN drop on both endpoints — or, for a portless
// "method:pass@host", publish the node under a fabricated 443 (mihomo re-parses
// the same payload and its structure decoder refuses the empty port,
// convert/converter.go:397-436).
func decodeSSLegacy(authority string) (server, port string, ok bool) {
	decoded, err := base64.RawStdEncoding.DecodeString(authority)
	if err != nil {
		return "", "", false
	}
	plain := ioutil.UnsafeString(decoded)
	if strings.IndexByte(plain, '@') < 0 {
		return "", "", false
	}
	// splitHostPort cuts at the LAST '@', so a password containing one is safe.
	server, port = splitHostPort(plain)
	if server == "" || port == "" {
		return "", "", false
	}
	return server, port, true
}
