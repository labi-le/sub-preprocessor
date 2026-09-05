package subscription_test

import (
	"testing"

	"domains.lst/sub-preprocessor/internal/subscription"
)

func TestParseMieruTakesPortFromQuery(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		line       string
		wantServer string
		wantPort   string
	}{
		{"single port", "mierus://user:pass@1.2.3.4?port=2999&protocol=TCP#Mieru", "1.2.3.4", "2999"},
		// A mieru port may be a range; it is only ever a dedupe and
		// dead-cache key here, so a usable one is kept verbatim rather than
		// parsed.
		{"port range", "mierus://user:pass@1.2.3.4?port=9998-9999&protocol=UDP#R", "1.2.3.4", "9998-9999"},
		{"first of several ports", "mierus://u@h.example?port=2999&port=3000&protocol=TCP&protocol=UDP", "h.example", "2999"},
		// mihomo's per-port loop skips only the pair whose port it cannot use,
		// so an unusable FIRST port still leaves one working proxy on 3000 —
		// verified against v1.19.27, which converts each of these links to a
		// single mieru proxy that adapter.ParseProxy accepts.
		{"empty first port of several", "mierus://u@1.2.3.4?port=&port=3000&protocol=TCP&protocol=UDP", "1.2.3.4", "3000"},
		{"non-numeric first port of several", "mierus://u@1.2.3.4?port=abc&port=3000&protocol=TCP&protocol=UDP", "1.2.3.4", "3000"},
		// A range is not Atoi'd at all (it becomes "port-range"), so it
		// survives the same skip.
		{"empty first port before a range", "mierus://u@1.2.3.4?port=&port=9998-9999&protocol=TCP&protocol=UDP", "1.2.3.4", "9998-9999"},
		{"non-numeric first port before a range", "mierus://u@1.2.3.4?port=abc&port=9998-9999&protocol=TCP&protocol=UDP", "1.2.3.4", "9998-9999"},
		// 99999 clears the converter (Atoi succeeds) and dies one layer
		// later, at adapter.ParseProxy's 1..65535 check — leaving the same
		// single live proxy on 3000, and the same fabricated key if taken.
		{"out-of-range first port of several", "mierus://u@1.2.3.4?port=99999&port=3000&protocol=TCP&protocol=UDP", "1.2.3.4", "3000"},
		{"query after a path", "mierus://u@1.2.3.4/x?port=2999&protocol=TCP#P", "1.2.3.4", "2999"},
		{"query port wins over an authority port", "mierus://u@1.2.3.4:8080?port=2999&protocol=TCP", "1.2.3.4", "2999"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			node := mustParseOne(t, tc.line)
			if node.Scheme != subscription.SchemeMieru {
				t.Errorf("scheme: got %q, want %q", node.Scheme, subscription.SchemeMieru)
			}
			if node.Server != tc.wantServer {
				t.Errorf("server: got %q, want %q", node.Server, tc.wantServer)
			}
			if node.Port != tc.wantPort {
				t.Errorf("port: got %q, want %q", node.Port, tc.wantPort)
			}
		})
	}
}

// TestParseMieruRejects: mihomo expands a mierus:// link into one proxy per
// "port" paired with the "protocol" at the same index, drops the link when the
// lists do not line up, and turns each port into a number the mieru adapter
// then bounds-checks — so keeping any of these would only spend probe budget
// on nodes that can never be selected, under a fabricated dead-cache and
// dedupe key.
func TestParseMieruRejects(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		line string
	}{
		{"no query at all", "mierus://user:pass@1.2.3.4#N"},
		{"no port", "mierus://user:pass@1.2.3.4?protocol=TCP#N"},
		{"no protocol", "mierus://user:pass@1.2.3.4?port=2999#N"},
		{"more ports than protocols", "mierus://u@1.2.3.4?port=2999&port=3000&protocol=TCP"},
		{"more protocols than ports", "mierus://u@1.2.3.4?port=2999&protocol=TCP&protocol=UDP"},
		{"port only in the fragment", "mierus://u@1.2.3.4#?port=2999&protocol=TCP"},
		// The pair counts line up here, so only the VALUE check rejects it;
		// without one the node is published on the generic 443 default, a port
		// it does not have. mihomo agrees: this single-pair form is the one
		// valueless-port link that converts to zero proxies, hence "format
		// invalid".
		{"empty port value", "mierus://u@1.2.3.4?port=&protocol=TCP"},
		// A multi-digit port opening with '0' is refused by the shared
		// portNumber gate because mihomo's structure decoder parses ports with
		// base-0 strconv (structure.go:143): "0443" would read as octal 291 and
		// "08" would not decode at all. The mieru converter's own Atoi is base
		// 10, so this is a deliberate over-reach of the shared gate, never a
		// loosening.
		{"leading-zero port", "mierus://u@1.2.3.4?port=0443&protocol=TCP"},
		{"leading-zero port in a range", "mierus://u@1.2.3.4?port=0443-0444&protocol=TCP"},
		{"every port value empty", "mierus://u@1.2.3.4?port=&port=&protocol=TCP&protocol=UDP"},
		{"every port value non-numeric", "mierus://u@1.2.3.4?port=abc&port=def&protocol=TCP&protocol=UDP"},
		// A '-' sends the value past the converter untouched as "port-range",
		// so these three reach adapter.ParseProxy, which refuses them:
		// "invalid port-range format" twice, then "begin port must be less
		// than or equal to end port".
		{"non-numeric range", "mierus://u@1.2.3.4?port=abc-def&protocol=TCP"},
		{"half-open range", "mierus://u@1.2.3.4?port=9998-&protocol=TCP"},
		{"descending range", "mierus://u@1.2.3.4?port=9999-9998&protocol=TCP"},
		// Atoi takes 99999 and 0; the adapter's 1..65535 window does not.
		{"port above the port space", "mierus://u@1.2.3.4?port=99999&protocol=TCP"},
		{"zero port", "mierus://u@1.2.3.4?port=0&protocol=TCP"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rejectOne(t, tc.line)
		})
	}
}
