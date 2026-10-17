package subscription

import (
	"bytes"
	"strings"
	"testing"
)

const xrayVlessOutbound = `{"protocol":"vless","settings":{"vnext":[{"address":"198.18.0.1","port":443,"users":[{"id":"11111111-1111-4111-8111-111111111111"}]}]}}`

func xrayBody(outbounds ...string) []byte {
	return []byte(`{"remarks":"case","outbounds":[` + strings.Join(outbounds, ",") + `]}`)
}

// xrayUnevenBody is the shape that exposed the reservation: one long link
// followed by outbounds that convert to nothing. The first outbound's remarks
// length and the direct count are both upstream-controlled.
func xrayUnevenBody(remarkBytes, directCount int) []byte {
	var sb strings.Builder
	sb.WriteString(`{"remarks":"`)
	sb.WriteString(strings.Repeat("x", remarkBytes))
	sb.WriteString(`","outbounds":[`)
	sb.WriteString(xrayVlessOutbound)
	for range directCount {
		sb.WriteString(`,{"protocol":"freedom"}`)
	}
	sb.WriteString(`]}`)

	return []byte(sb.String())
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

// Panels do not agree on protocol casing; dispatch and the reservation count
// both have to resolve VLESS/Vless and reject FREEDOM without building a
// lowercased copy of the name.
func TestConvertXrayJSONProtocolCase(t *testing.T) {
	want := Normalize(xrayBody(xrayVlessOutbound))
	if !bytes.HasPrefix(want, []byte("vless://")) {
		t.Fatalf("fixture did not convert: %q", want)
	}

	for _, protocol := range []string{"VLESS", "Vless", "vLeSs"} {
		body := xrayBody(strings.Replace(xrayVlessOutbound, `"vless"`, `"`+protocol+`"`, 1))
		if got := Normalize(body); !bytes.Equal(got, want) {
			t.Fatalf("protocol %q: got %q, want %q", protocol, got, want)
		}
	}

	for _, protocol := range []string{"freedom", "FREEDOM", "Direct"} {
		body := xrayBody(xrayVlessOutbound, `{"protocol":"`+protocol+`"}`)
		if got := Normalize(body); !bytes.Equal(got, want) {
			t.Fatalf("%s outbound changed the output: %q", protocol, got)
		}
	}
}

// The count scan runs before dispatch, so both resolve the same protocol. An
// allocation per uppercase name would charge documents that convert nothing.
func TestConvertXrayJSONProtocolCaseAllocations(t *testing.T) {
	const outbounds = 1000

	direct := func(protocol string) []byte {
		entries := make([]string, outbounds)
		for i := range entries {
			entries[i] = `{"protocol":"` + protocol + `"}`
		}

		return xrayBody(entries...)
	}

	lowerBody, upperBody := direct("freedom"), direct("FREEDOM")
	lower := testing.AllocsPerRun(5, func() { Normalize(lowerBody) })
	upper := testing.AllocsPerRun(5, func() { Normalize(upperBody) })
	if upper > lower {
		t.Fatalf("uppercase protocol added %.0f allocs/op: %.0f vs %.0f", upper-lower, upper, lower)
	}
}
