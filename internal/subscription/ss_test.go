package subscription_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"domains.lst/sub-preprocessor/internal/subscription"
)

// ssLegacyLine builds the pre-SIP002 form, whose whole authority is base64 of
// "method:password@host:port". The guard keeps a fixture from silently testing
// something else: the authority ends at the first '/', '?' or '#', so a payload
// carrying one is truncated before it is ever decoded.
func ssLegacyLine(t *testing.T, enc *base64.Encoding, plain, fragment string) string {
	t.Helper()
	payload := enc.EncodeToString([]byte(plain))
	if strings.ContainsAny(payload, "/?#") {
		t.Fatalf("fixture %q encodes to %q, which the authority scan truncates", plain, payload)
	}
	return "ss://" + payload + fragment
}

func TestParseSSLegacyDecodesAuthority(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		enc        *base64.Encoding
		plain      string
		fragment   string
		wantServer string
		wantPort   string
		wantName   string
	}{
		{"unpadded std alphabet", base64.RawStdEncoding, "aes-256-gcm:pass@1.2.3.4:8388", "#Legacy", "1.2.3.4", "8388", "Legacy"},
		// A payload carrying '+' proves the decoder is std and not url-safe:
		// URLEncoding would refuse the character outright.
		{"payload using the std-only '+'", base64.RawStdEncoding, "aes-256-gcm:pa~@1.2.3.4:8388", "#Legacy", "1.2.3.4", "8388", "Legacy"},
		{"no fragment names the node after the decoded host", base64.RawStdEncoding, "aes-256-gcm:pass@example.net:8388", "", "example.net", "8388", "example.net"},
		{"password containing @ splits at the last one", base64.RawStdEncoding, "aes-256-gcm:p@ss@1.2.3.4:8388", "#N", "1.2.3.4", "8388", "N"},
		{"ipv6 host", base64.RawStdEncoding, "aes-256-gcm:pass@[2001:db8::1]:8388", "#N", "2001:db8::1", "8388", "N"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			line := ssLegacyLine(t, tc.enc, tc.plain, tc.fragment)
			node := mustParseOne(t, line)
			if node.Scheme != subscription.SchemeSS {
				t.Errorf("scheme: got %q, want %q", node.Scheme, subscription.SchemeSS)
			}
			if node.Server != tc.wantServer {
				t.Errorf("server: got %q, want %q", node.Server, tc.wantServer)
			}
			if node.Port != tc.wantPort {
				t.Errorf("port: got %q, want %q", node.Port, tc.wantPort)
			}
			if node.Name != tc.wantName {
				t.Errorf("name: got %q, want %q", node.Name, tc.wantName)
			}
			if node.Raw != line {
				t.Errorf("raw: got %q, want %q", node.Raw, line)
			}
		})
	}
}

// TestParseSSLegacyRejects: an ss link whose authority is neither a host nor a
// decodable "…@host" is rejected instead of handing a base64 blob to the
// resolver, which used to book it as an NXDOMAIN drop and lose the node.
//
// The alphabet cases are the narrowing: the spec writes the legacy authority
// unpadded (https://shadowsocks.org/doc/configs.html) and mihomo decodes it
// with RawStdEncoding alone, so accepting a padded or url-safe payload would
// only publish a node no client we serve can convert — one guaranteed probe
// failure plus 2h of dead-cache pollution under its server:port.
//
// The portless case is the same story one field further on: a payload decoding
// to "method:pass@host" has no port, and the generic path used to fabricate
// one (443) for a node no client converts. mihomo re-parses the same payload
// and its structure decoder refuses the empty port (convert/converter.go:397-436).
func TestParseSSLegacyRejects(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		line string
	}{
		{"undecodable payload", "ss://!!!not-base64!!!#N"},
		{"std alphabet with padding", ssLegacyLine(t, base64.StdEncoding, "aes-256-gcm:pass@1.2.3.4:8388", "#N")},
		{"url-safe alphabet", ssLegacyLine(t, base64.RawURLEncoding, "aes-256-gcm:pa~@1.2.3.4:8388", "#N")},
		{"decoded payload carries no @", ssLegacyLine(t, base64.RawStdEncoding, "aes-256-gcm:pass", "#N")},
		{"decoded payload carries no host", ssLegacyLine(t, base64.RawStdEncoding, "aes-256-gcm:pass@:8388", "#N")},
		{"decoded payload carries no port", ssLegacyLine(t, base64.RawStdEncoding, "aes-256-gcm:pass@example.net", "#N")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rejectOne(t, tc.line)
		})
	}
}

// TestParseSSUppercaseSchemeDecodes pins scheme normalization: mihomo lowercases
// the scheme before dispatching (convert/converter.go:35), so an "SS://" line
// must reach the legacy decoder, not the generic authority path that would
// publish the base64 blob as a hostname.
func TestParseSSUppercaseSchemeDecodes(t *testing.T) {
	t.Parallel()

	line := "SS://" + base64.RawStdEncoding.EncodeToString([]byte("aes-256-gcm:pass@1.2.3.4:8388")) + "#Up"
	node := mustParseOne(t, line)
	if node.Scheme != subscription.SchemeSS {
		t.Errorf("scheme: got %q, want %q", node.Scheme, subscription.SchemeSS)
	}
	if node.Server != "1.2.3.4" {
		t.Errorf("server: got %q, want 1.2.3.4", node.Server)
	}
	if node.Port != "8388" {
		t.Errorf("port: got %q, want 8388", node.Port)
	}
	if node.Name != "Up" {
		t.Errorf("name: got %q, want Up", node.Name)
	}
}

// TestParseSSSIP002UsesAuthority: the modern form keeps its host in the URI, so
// it must stay on the generic path and never be base64-decoded.
func TestParseSSSIP002UsesAuthority(t *testing.T) {
	t.Parallel()

	line := "ss://" + base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:pass")) + "@1.2.3.4:8388?uot=1#SIP002"
	node := mustParseOne(t, line)
	if node.Server != "1.2.3.4" {
		t.Errorf("server: got %q, want 1.2.3.4", node.Server)
	}
	if node.Port != "8388" {
		t.Errorf("port: got %q, want 8388", node.Port)
	}
	if node.Name != "SIP002" {
		t.Errorf("name: got %q, want SIP002", node.Name)
	}
	if want := strings.IndexByte(line, '#'); node.FragmentIdx != want {
		t.Errorf("fragmentIdx: got %d, want %d", node.FragmentIdx, want)
	}
}

// TestParseSSSIP002WithoutPortRejected: a SIP002-shaped link with no port is
// the one ss form the '@' told us nothing about. mihomo branches on the port
// (convert/converter.go:396-407), so for this link it takes the LEGACY branch
// and RawStd-decodes the bare host — a dotted quad is not base64, the decode
// fails, and the line is dropped (measured: 0 mappings + "format invalid";
// the same link with ":8388" yields 1). Keeping it meant defaulting the port
// to 443 and publishing, probing and dead-caching a node under a port it does
// not have.
func TestParseSSSIP002WithoutPortRejected(t *testing.T) {
	t.Parallel()

	userinfo := base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:pass"))
	for _, line := range []string{
		"ss://" + userinfo + "@1.2.3.5#N",
		"ss://" + userinfo + "@example.net?uot=1#N",
		"ss://" + userinfo + "@[2001:db8::1]#N",
	} {
		t.Run(line, func(t *testing.T) {
			t.Parallel()
			rejectOne(t, line)
		})
	}
}
