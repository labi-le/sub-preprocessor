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
// "port" paired with the "protocol" at the same index and drops the link when
// the lists do not line up, so keeping these would only spend probe budget on
// nodes that can never be selected.
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
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rejectOne(t, tc.line)
		})
	}
}
