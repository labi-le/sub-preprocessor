package subscription_test

import (
	"encoding/base64"
	"net/url"
	"strings"
	"testing"

	"domains.lst/sub-preprocessor/internal/subscription"
)

// realityTCP is the shape 85 of one measured post's 158 vless outbounds had.
// The dns block is not decoration: it carries a DoH URL, which is why Normalize
// has to sniff JSON before its "://" fast path.
const realityTCP = `[{"remarks":"node-a","dns":{"servers":["https://1.1.1.1/dns-query"]},"outbounds":[
{"tag":"proxy","protocol":"vless","settings":{"vnext":[{"address":"as.example.net","port":443,
"users":[{"id":"ab7d5ea9-6eca-47c3-b14b-67378fc2d7c2","encryption":"none","flow":"xtls-rprx-vision"}]}]},
"streamSettings":{"network":"tcp","security":"reality","realitySettings":{"serverName":"as.example.net",
"publicKey":"UO3EObgU3xUrhIGEE0gfCn5ZOz8YxNcwwW6ZaYzD3SA","shortId":"4e9b0c2d1a3f5768","fingerprint":"firefox"}}},
{"tag":"direct","protocol":"freedom"}]}]`

func queryOf(t *testing.T, uri string) url.Values {
	t.Helper()
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse %q: %v", uri, err)
	}

	return u.Query()
}

func normalizeOne(t *testing.T, body string) string {
	t.Helper()
	out := strings.TrimSpace(string(subscription.Normalize([]byte(body))))
	if out == "" {
		t.Fatal("Normalize returned an empty body")
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 node, got %d: %q", len(lines), out)
	}

	return lines[0]
}

// The DoH URL inside the config means the body contains "://". Normalize used to
// return such a body untouched, so ordering the JSON branch after that check
// silently disabled conversion for exactly the configs seen in production.
func TestNormalizeConvertsXrayJSONDespiteEmbeddedURL(t *testing.T) {
	t.Parallel()

	if !strings.Contains(realityTCP, "://") {
		t.Fatal("fixture must contain :// or it does not pin the ordering")
	}
	uri := normalizeOne(t, realityTCP)
	if !strings.HasPrefix(uri, "vless://ab7d5ea9-6eca-47c3-b14b-67378fc2d7c2@as.example.net:443?") {
		t.Fatalf("unexpected authority: %q", uri)
	}
	q := queryOf(t, uri)
	for key, want := range map[string]string{
		"type": "tcp", "security": "reality", "encryption": "none",
		"flow": "xtls-rprx-vision", "sni": "as.example.net",
		"pbk": "UO3EObgU3xUrhIGEE0gfCn5ZOz8YxNcwwW6ZaYzD3SA",
		"sid": "4e9b0c2d1a3f5768", "fp": "firefox",
	} {
		if got := q.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if !strings.HasSuffix(uri, "#node-a") {
		t.Errorf("remarks must become the node name: %q", uri)
	}
}

// Xray renamed plain TCP to "raw". mihomo's share-link handler has no "raw"
// case, so passing it through leaves network="raw" and adapter.ParseProxy
// refuses the node.
func TestNormalizeMapsRawTransportToTCP(t *testing.T) {
	t.Parallel()

	body := `{"outbounds":[{"protocol":"vless","settings":{"vnext":[{"address":"1.2.3.4","port":8443,
"users":[{"id":"u-1"}]}]},"streamSettings":{"network":"raw","security":"tls","tlsSettings":{"serverName":"a.example"}}}]}`
	if got := queryOf(t, normalizeOne(t, body)).Get("type"); got != "tcp" {
		t.Fatalf("type = %q, want tcp", got)
	}
}

func TestNormalizeCarriesWSAndGRPCTransportFields(t *testing.T) {
	t.Parallel()

	ws := `{"outbounds":[{"protocol":"vless","settings":{"vnext":[{"address":"1.2.3.4","port":25129,
"users":[{"id":"u-1"}]}]},"streamSettings":{"network":"ws","wsSettings":{"path":"/xray","headers":{"host":"cdn.example"}}}}]}`
	q := queryOf(t, normalizeOne(t, ws))
	if got := q.Get("path"); got != "/xray" {
		t.Errorf("ws path = %q", got)
	}
	// Lowercase "host" on purpose: Xray configs are inconsistent about the key's
	// case, and a case-sensitive lookup would dial with the wrong Host header.
	if got := q.Get("host"); got != "cdn.example" {
		t.Errorf("ws host = %q, want cdn.example (case-insensitive header lookup)", got)
	}

	grpc := `{"outbounds":[{"protocol":"vless","settings":{"vnext":[{"address":"1.2.3.4","port":6437,
"users":[{"id":"u-1"}]}]},"streamSettings":{"network":"grpc","security":"reality",
"grpcSettings":{"serviceName":"grpc"},"realitySettings":{"publicKey":"pk","shortId":"sid1"}}}]}`
	q = queryOf(t, normalizeOne(t, grpc))
	if got := q.Get("serviceName"); got != "grpc" {
		t.Errorf("serviceName = %q", got)
	}
	if got := q.Get("pbk"); got != "pk" {
		t.Errorf("pbk = %q", got)
	}
}

func TestNormalizeCarriesXHTTPFields(t *testing.T) {
	t.Parallel()

	body := `{"outbounds":[{"protocol":"vless","settings":{"vnext":[{"address":"a.example","port":443,
"users":[{"id":"u-1"}]}]},"streamSettings":{"network":"xhttp","security":"tls",
"xhttpSettings":{"mode":"auto","path":"/assets","host":"h.example"},"tlsSettings":{"serverName":"a.example","fingerprint":"firefox"}}}]}`
	q := queryOf(t, normalizeOne(t, body))
	for key, want := range map[string]string{"type": "xhttp", "mode": "auto", "path": "/assets", "host": "h.example", "fp": "firefox"} {
		if got := q.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

// An unbracketed IPv6 authority reads as a portless IPv6 host, so the port is
// lost and mihomo refuses the link.
func TestNormalizeBracketsIPv6Authority(t *testing.T) {
	t.Parallel()

	body := `{"outbounds":[{"protocol":"vless","settings":{"vnext":[{"address":"2001:db8::1","port":443,
"users":[{"id":"u-1"}]}]},"streamSettings":{"network":"tcp"}}]}`
	uri := normalizeOne(t, body)
	if !strings.HasPrefix(uri, "vless://u-1@[2001:db8::1]:443?") {
		t.Fatalf("IPv6 authority must be bracketed: %q", uri)
	}
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Port() != "443" {
		t.Errorf("port = %q, want 443", u.Port())
	}
}

// Every outbound of a multi-config document is tagged "proxy" by Xray, so the
// tag cannot name the node — it would collapse them all to one name.
func TestNormalizeNamesEachNodeDistinctly(t *testing.T) {
	t.Parallel()

	body := `[{"outbounds":[{"tag":"proxy","protocol":"vless","settings":{"vnext":[{"address":"a.example","port":443,"users":[{"id":"u-1"}]}]},"streamSettings":{"network":"tcp"}}]},
{"outbounds":[{"tag":"proxy","protocol":"vless","settings":{"vnext":[{"address":"b.example","port":443,"users":[{"id":"u-2"}]}]},"streamSettings":{"network":"tcp"}}]}]`
	lines := strings.Split(strings.TrimSpace(string(subscription.Normalize([]byte(body)))), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(lines))
	}
	if !strings.HasSuffix(lines[0], "#a.example") || !strings.HasSuffix(lines[1], "#b.example") {
		t.Fatalf("names must fall back to the server address: %q", lines)
	}
}

func TestNormalizeConvertsBase64WrappedXrayJSON(t *testing.T) {
	t.Parallel()

	encoded := base64.StdEncoding.EncodeToString([]byte(realityTCP))
	if got := normalizeOne(t, encoded); !strings.HasPrefix(got, "vless://") {
		t.Fatalf("base64-wrapped config must convert: %q", got)
	}
}

// The false return of the converter is what keeps every existing JSON-serving
// source classified exactly as before. Without it, bodies that hold no vless
// outbound would start coming back empty instead of unchanged.
func TestNormalizeLeavesNonXrayJSONUntouched(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"clash-ish":   `{"proxies":[{"name":"a","type":"vmess","server":"1.2.3.4","port":443}]}`,
		"empty array": `[]`,
		"no vless":    `{"outbounds":[{"protocol":"freedom","tag":"direct"}]}`,
		"truncated":   `[{"outbounds":[{"protocol":"vless"`,
		"vnext without id": `{"outbounds":[{"protocol":"vless","settings":{"vnext":[{"address":"a.example","port":443,` +
			`"users":[{"encryption":"none"}]}]},"streamSettings":{"network":"tcp"}}]}`,
	} {
		if got := string(subscription.Normalize([]byte(body))); got != strings.TrimSpace(body) {
			t.Errorf("%s: body must pass through unchanged, got %q", name, got)
		}
	}
}
