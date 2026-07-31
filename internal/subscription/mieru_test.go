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
		// dead-cache key here, so it is kept verbatim rather than parsed.
		{"port range", "mierus://user:pass@1.2.3.4?port=9998-9999&protocol=UDP#R", "1.2.3.4", "9998-9999"},
		{"first of several ports", "mierus://u@h.example?port=2999&port=3000&protocol=TCP&protocol=UDP", "h.example", "2999"},
		// mihomo's per-port loop skips only the pair whose port fails
		// strconv.Atoi, so a valueless FIRST port still leaves one working
		// proxy on 3000 — verified against v1.19.27, which converts this link
		// to a single mieru proxy named "1.2.3.4:3000/UDP".
		{"empty first port of several", "mierus://u@1.2.3.4?port=&port=3000&protocol=TCP&protocol=UDP", "1.2.3.4", "3000"},
		// A range is not Atoi'd at all (it becomes "port-range"), so it
		// survives the same skip.
		{"empty first port before a range", "mierus://u@1.2.3.4?port=&port=9998-9999&protocol=TCP&protocol=UDP", "1.2.3.4", "9998-9999"},
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
// lists do not line up, and strconv.Atoi's each port — so keeping any of these
// would only spend probe budget on nodes that can never be selected, under a
// fabricated dead-cache and dedupe key.
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
		{"every port value empty", "mierus://u@1.2.3.4?port=&port=&protocol=TCP&protocol=UDP"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rejectOne(t, tc.line)
		})
	}
}
