package preprocess //nolint:testpackage // exercises the unexported processBody pipeline

import (
	"bytes"
	"context"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"domains.lst/sub-preprocessor/internal/filter"
	"domains.lst/sub-preprocessor/internal/geofeed"
	"github.com/rs/zerolog"
)

// benchGeofeed builds a real geofeed.NewLookup over ~500 synthetic entries so
// the geofeed filter stage performs a representative binary search. The last
// entry covers the 198.51.100.0/24 block used by the synthetic node servers and
// maps it to NL (an allowed country) so every node survives the filter and
// exercises the rewrite->buffer tail of the pipeline.
func benchGeofeed() geofeed.CountryLookup { //nolint:ireturn // bench helper returns the geofeed lookup interface
	entries := make([]geofeed.Entry, 0, 501)
	countries := []geofeed.CountryCode{{'D', 'E'}, {'U', 'S'}, {'J', 'P'}, {'G', 'B'}, {'F', 'R'}}
	for i := range 500 {
		prefix := netip.PrefixFrom(
			netip.AddrFrom4([4]byte{10, byte(i / 256), byte(i % 256), 0}),
			24,
		)
		entries = append(entries, geofeed.Entry{Prefix: prefix, Country: countries[i%len(countries)]})
	}
	entries = append(entries, geofeed.Entry{
		Prefix:  netip.MustParsePrefix("198.51.100.0/24"),
		Country: geofeed.CountryCode{'N', 'L'},
	})
	return geofeed.NewLookup(entries)
}

// benchBody builds an already-normalized subscription body of 100 nodes whose
// servers are bare IPv4 addresses, so resolver.Resolve short-circuits DNS and
// the benchmark performs no network I/O.
func benchBody() []byte {
	var buf bytes.Buffer
	for i := range 100 {
		if i > 0 {
			buf.WriteByte('\n')
		}
		buf.WriteString("vless://u@198.51.100.")
		buf.WriteString(strconv.Itoa(i + 1))
		buf.WriteString(":443#node")
		buf.WriteString(strconv.Itoa(i))
	}
	return buf.Bytes()
}

func newBenchProcessor(b *testing.B) *Processor {
	b.Helper()
	p, err := NewProcessor(context.Background(), zerolog.Nop(), Options{
		PreloadedGeofeed:  benchGeofeed(),
		PreloadedLoadedAt: time.Now(),
		WorkflowStages:    []string{"geofeed"},
		Annotate:          true,
	})
	if err != nil {
		b.Fatalf("NewProcessor: %v", err)
	}
	return p
}

// BenchmarkProcessBodyPipeline measures the network-free per-request hot loop:
// parse -> resolve (bare-IPv4 short circuit) -> geofeed filter -> annotate
// rewrite -> buffer, across 100 nodes. The resolved map is cleared each
// iteration to mirror a fresh request (no cross-request DNS cache reuse).
func BenchmarkProcessBodyPipeline(b *testing.B) {
	p := newBenchProcessor(b)
	body := benchBody()
	lookup, _ := p.GeofeedState()
	allowed := filter.ParseAllowed("NL")

	buf := &bytes.Buffer{}
	buf.Grow(64 << 10)
	resolved := p.resolver.GetResolvedMap()
	defer p.resolver.PutResolvedMap(resolved)
	stats := Stats{}
	pctx := &PipelineContext{
		Buffer:   buf,
		Lookup:   lookup,
		Allowed:  allowed,
		Resolved: resolved,
		Stats:    &stats,
	}

	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		buf.Reset()
		clear(resolved)
		stats = Stats{}
		pctx.IsFirstNode = true
		if err := p.processBody(ctx, body, pctx); err != nil {
			b.Fatalf("processBody: %v", err)
		}
		if stats.Kept != 100 {
			b.Fatalf("kept = %d, want 100 (all nodes should survive)", stats.Kept)
		}
	}
}
