package stable_test

import (
	"testing"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/common/convert"

	"domains.lst/sub-preprocessor/internal/subscription"
)

// The share links subscription.Normalize synthesizes from Xray JSON are only
// worth anything if THIS path accepts them: convert.ConvertsV2Ray drops an
// unparseable line with `continue` and returns no error, so a mapping bug would
// surface as a missing node rather than a failure — which is why every case
// below asserts the mapping COUNT and not just the absence of an error.
//
// The reality cases found the reassuring half: adapter.ParseProxy validates
// both the x25519 public key and the short ID (non-hex or odd length is
// refused), so a dropped or mangled pbk/sid fails loudly here instead of
// quietly degrading to plain TLS. A converted reality node that reaches the
// prober has had both structurally validated, which is what makes a probe
// failure on it evidence about the NODE rather than about this mapping.

// realityPubKey / realityShortID are structurally valid on purpose: mihomo
// rejects malformed values, so placeholder strings would fail the parse for a
// reason that has nothing to do with the mapping under test.
const (
	realityPubKey  = "pHQke94AM1SmUNRlA7CNyXL4dK9-O1mTZoNVJKVFTk0"
	realityShortID = "1dbf8a58bc15d4b0"
)

// mihomoProxy runs one Xray config document through the production path and
// returns the single proxy map it must yield.
func mihomoProxy(t *testing.T, body string) map[string]any {
	t.Helper()

	payload := subscription.Normalize([]byte(body))
	mappings, err := convert.ConvertsV2Ray(payload)
	if err != nil {
		t.Fatalf("ConvertsV2Ray: %v (payload %q)", err, payload)
	}
	if len(mappings) != 1 {
		t.Fatalf("got %d mappings, want 1; payload %q", len(mappings), payload)
	}
	px, parseErr := adapter.ParseProxy(mappings[0])
	if parseErr != nil {
		t.Fatalf("ParseProxy: %v (payload %q)", parseErr, payload)
	}
	t.Cleanup(func() { _ = px.Close() })

	return mappings[0]
}

func wantString(t *testing.T, m map[string]any, key, want string) {
	t.Helper()

	if got, _ := m[key].(string); got != want {
		t.Errorf("%s = %q, want %q", key, got, want)
	}
}

func wantOpt(t *testing.T, m map[string]any, optsKey, field, want string) {
	t.Helper()

	opts, ok := m[optsKey].(map[string]any)
	if !ok {
		t.Fatalf("%s missing from proxy map: %v", optsKey, m)
	}
	if got, _ := opts[field].(string); got != want {
		t.Errorf("%s.%s = %q, want %q", optsKey, field, got, want)
	}
}

func TestXrayTCPRealitySurvivesMihomoParse(t *testing.T) {
	t.Parallel()

	m := mihomoProxy(t, `{"outbounds":[{"protocol":"vless","settings":{"vnext":[{"address":"1.2.3.4","port":443,
"users":[{"id":"ab7d5ea9-6eca-47c3-b14b-67378fc2d7c2","flow":"xtls-rprx-vision"}]}]},
"streamSettings":{"network":"tcp","security":"reality","realitySettings":{"serverName":"tesla.com",
"publicKey":"`+realityPubKey+`","shortId":"`+realityShortID+`","fingerprint":"edge"}}}]}`)

	wantString(t, m, "servername", "tesla.com")
	wantString(t, m, "client-fingerprint", "edge")
	wantString(t, m, "flow", "xtls-rprx-vision")
	wantOpt(t, m, "reality-opts", "public-key", realityPubKey)
	wantOpt(t, m, "reality-opts", "short-id", realityShortID)
}

// Xray renamed plain TCP to "raw" and mihomo has no "raw" case, so an
// unmapped name would reach adapter.ParseProxy as network="raw".
func TestXrayRawTransportSurvivesMihomoParse(t *testing.T) {
	t.Parallel()

	m := mihomoProxy(t, `{"outbounds":[{"protocol":"vless","settings":{"vnext":[{"address":"1.2.3.4","port":443,
"users":[{"id":"u-1"}]}]},"streamSettings":{"network":"raw","security":"reality",
"realitySettings":{"serverName":"a.example","publicKey":"`+realityPubKey+`","shortId":"`+realityShortID+`"}}}]}`)

	wantString(t, m, "network", "tcp")
	wantOpt(t, m, "reality-opts", "public-key", realityPubKey)
}

func TestXrayWSSurvivesMihomoParse(t *testing.T) {
	t.Parallel()

	m := mihomoProxy(t, `{"outbounds":[{"protocol":"vless","settings":{"vnext":[{"address":"1.2.3.4","port":25129,
"users":[{"id":"u-1"}]}]},"streamSettings":{"network":"ws","wsSettings":{"path":"/xray","headers":{"Host":"cdn.example"}}}}]}`)

	wantString(t, m, "network", "ws")
	wantOpt(t, m, "ws-opts", "path", "/xray")
}

func TestXrayGRPCSurvivesMihomoParse(t *testing.T) {
	t.Parallel()

	m := mihomoProxy(t, `{"outbounds":[{"protocol":"vless","settings":{"vnext":[{"address":"1.2.3.4","port":6437,
"users":[{"id":"u-1"}]}]},"streamSettings":{"network":"grpc","security":"reality",
"grpcSettings":{"serviceName":"grpc"},"realitySettings":{"publicKey":"`+realityPubKey+`","shortId":"`+realityShortID+`"}}}]}`)

	wantString(t, m, "network", "grpc")
	wantOpt(t, m, "grpc-opts", "grpc-service-name", "grpc")
}

func TestXrayXHTTPSurvivesMihomoParse(t *testing.T) {
	t.Parallel()

	m := mihomoProxy(t, `{"outbounds":[{"protocol":"vless","settings":{"vnext":[{"address":"a.example","port":443,
"users":[{"id":"u-1"}]}]},"streamSettings":{"network":"xhttp","security":"tls",
"xhttpSettings":{"mode":"auto","path":"/assets"},"tlsSettings":{"serverName":"a.example","fingerprint":"firefox"}}}]}`)

	wantString(t, m, "network", "xhttp")
	wantOpt(t, m, "xhttp-opts", "path", "/assets")
	if m["tls"] != true {
		t.Errorf("tls = %v, want true", m["tls"])
	}
}

func TestXrayHysteria2SurvivesMihomoParse(t *testing.T) {
	t.Parallel()

	m := mihomoProxy(t, `{"outbounds":[{"protocol":"hysteria","settings":{"address":"1.2.3.4","port":443,"version":2},
"streamSettings":{"network":"hysteria","hysteriaSettings":{"version":2,"auth":"68b4b16e-2759-47a4-8a69-33118edf5ce6"},
"security":"tls","tlsSettings":{"serverName":"a.example","alpn":["h3"]}}}]}`)

	wantString(t, m, "type", "hysteria2")
	wantString(t, m, "server", "1.2.3.4")
	wantString(t, m, "password", "68b4b16e-2759-47a4-8a69-33118edf5ce6")
	wantString(t, m, "sni", "a.example")
	alpn, ok := m["alpn"].([]string)
	if !ok || len(alpn) != 1 || alpn[0] != "h3" {
		t.Errorf("alpn = %v, want [h3]", m["alpn"])
	}
}
