package cidrset_test

import (
	"fmt"
	"math/rand/v2"
	"net/netip"
	"strings"
	"testing"

	"domains.lst/sub-preprocessor/internal/cidrset"
)

// BenchmarkContains runs against a set the size of the shipped whitelist
// (~30k /24s). Contains sits on the per-node path of every subscription, so
// allocs/op is the number to watch here, not ns/op.
func BenchmarkContains(b *testing.B) {
	set, _ := cidrset.Parse(benchmarkBody(30_000))
	hit := netip.MustParseAddr("203.0.113.42")

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if !set.Contains(hit) {
			b.Fatal("the probe address must be in the set")
		}
	}
}

// benchmarkBody builds n pseudo-random /24 lines from a fixed seed so the
// merged shape is stable run to run, plus the /24 the probe address lives in.
func benchmarkBody(n int) []byte {
	rng := rand.New(rand.NewPCG(1, 2))
	lines := make([]string, 0, n+1)
	for range n {
		lines = append(lines, fmt.Sprintf("%d.%d.%d.0/24", 1+rng.IntN(223), rng.IntN(256), rng.IntN(256)))
	}
	lines = append(lines, "203.0.113.0/24")
	return []byte(strings.Join(lines, "\n") + "\n")
}
