package subscription_test

import (
	"encoding/base64"
	"testing"

	"domains.lst/sub-preprocessor/internal/subscription"
)

var benchPlaceholderSink bool

// benchServers are the server shapes the address rule has to answer, malformed
// all-zero strings included: those are where a prefilter looser than the parser
// behind it shows up as an allocation.
var benchServers = []string{
	"cdn.example.com", "0.tcp.example.com", "192.0.2.1", "2001:db8::1",
	"0.0.0.0", "::", "::1", "127.0.0.1", "00.00.00.00", "0.0.0.0.0", "0",
}

type placeholderBench struct{ name, raw, server string }

// BenchmarkPlaceholderNode prices the predicate on the shapes Merge feeds it:
// the vless bulk, an ssr line whose whole payload is one base64 blob, the
// credential rule, and each of benchServers.
func BenchmarkPlaceholderNode(b *testing.B) {
	b64 := base64.RawURLEncoding.EncodeToString
	ssrPayload := "192.0.2.7:443:origin:aes-256-cfb:plain:" + b64([]byte("secret")) +
		"/?obfsparam=" + b64([]byte("obfs.example.com")) + "&remarks=" + b64([]byte("alpha node 7"))

	const cred = "vless://a1b2c3d4-1111-4000-8000-000000000009@"

	cases := make([]placeholderBench, 0, 3+len(benchServers))
	cases = append(cases,
		placeholderBench{"vless", cred + "192.0.2.7:443?type=tcp#alpha node 7", "192.0.2.7"},
		placeholderBench{"ssr", "ssr://" + b64([]byte(ssrPayload)), "192.0.2.7"},
		placeholderBench{
			"nil credential",
			"vless://00000000-0000-0000-0000-000000000000@192.0.2.1:1111#notice",
			"192.0.2.1",
		})
	for _, server := range benchServers {
		cases = append(cases, placeholderBench{server, cred + server + ":443#n", server})
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				benchPlaceholderSink = subscription.PlaceholderNode(tc.raw, tc.server)
			}
		})
	}
}
