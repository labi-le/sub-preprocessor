package subscription

import (
	"bytes"
	"strings"
	"testing"
)

// xrayUnevenBody is the shape that exposed the reservation: one long link
// followed by outbounds that convert to nothing. The first outbound's remarks
// length and the direct count are both upstream-controlled.
func xrayUnevenBody(remarkBytes, directCount int) []byte {
	first := `{"remarks":"` + strings.Repeat("x", remarkBytes) + `","outbounds":[{"protocol":"vless","settings":{"vnext":[{"address":"198.18.0.1","port":443,"users":[{"id":"11111111-1111-4111-8111-111111111111"}]}]}}`

	return []byte(first + strings.Repeat(`,{"protocol":"freedom"}`, directCount) + `]}`)
}

func TestConvertXrayJSONBoundsReservation(t *testing.T) {
	base := Normalize(xrayUnevenBody(4096, 0))
	if !bytes.HasPrefix(base, []byte("vless://")) || bytes.Count(base, []byte("vless://")) != 1 {
		t.Fatalf("fixture did not convert to exactly one link: %q", base)
	}

	for _, direct := range []int{0, 10, 1000} {
		out := Normalize(xrayUnevenBody(4096, direct))
		if !bytes.Equal(out, base) {
			t.Fatalf("direct=%d changed the output: got %d bytes, want %d", direct, len(out), len(base))
		}
		// A freedom outbound renders nothing, so it must not buy capacity.
		// Without the cap the 1000-outbound document reserved 4 MiB for this
		// 4 KiB output; growth beyond the cap has to come from real links.
		if cap(out) > len(out)+maxXrayReservation {
			t.Fatalf("direct=%d reserved %d bytes of capacity for a %d-byte output", direct, cap(out), len(out))
		}
	}
}
