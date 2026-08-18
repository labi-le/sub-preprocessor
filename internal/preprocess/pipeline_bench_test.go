package preprocess //nolint:testpackage // exercises the unexported processBody pipeline

import (
	"bytes"
	"context"
	"net/netip"
	"strconv"
	"testing"
	"time"
	"unsafe"

	"domains.lst/sub-preprocessor/internal/cidrset"
	"domains.lst/sub-preprocessor/internal/config"
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
		PreloadedGeofeed: GeoState{Lookup: benchGeofeed(), LoadedAt: time.Now()},
		IPFilters:        []config.IPFilterSpec{{Type: config.FilterCountry, Provider: config.ProviderGeofeed}},
		Annotate:         []config.AnnotateSpec{{Tag: config.TagGEO, Providers: []string{config.ProviderGeofeed}}},
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
//
// It annotates exactly ONE tag, GEO via geofeed, and that count is a FIXTURE
// (newBenchProcessor's Annotate list), not a property of the pipeline: editing
// it moves this benchmark's baseline with no production code involved. It has.
// `5d06fb6` dropped a second `{Tag: IP}` entry from that list, and holding
// production code at `23df10f` while taking only that fixture moves the numbers
// 18686 -> 15933 ns/op and 4642 -> 1600 B/op. AGENTS.md's bench notes carry the
// full four-tree measurement, including the +280 ns/op once attributed to the
// production change and since shown to be indistinguishable from the per-binary
// link floor. Nothing here can attribute a move to the annotator, which is
// what BenchmarkAnnotate (annotator_bench_test.go) is for. Its tag list is
// fixed in ITS OWN file, so a move there is normally the code — the ASN
// removal is the one round where that fixture changed too (two tags down to
// one), and AGENTS.md records the control that separates the two. THIS
// benchmark's fixture was already GEO-only and did not move that round.
func BenchmarkProcessBodyPipeline(b *testing.B) {
	p := newBenchProcessor(b)
	body := benchBody()
	lookup := p.GeofeedState().Lookup
	allowed := filter.ParseAllowed("NL")

	buf := &bytes.Buffer{}
	buf.Grow(64 << 10)
	resolved := p.resolver.GetResolvedMap()
	defer p.resolver.PutResolvedMap(resolved)
	stats := Stats{}
	sink := &bufferSink{buf: buf, annotator: p.annotator}
	pctx := &PipelineContext{
		sink:     sink,
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
		sink.wrote = false
		if err := p.processBody(ctx, body, pctx); err != nil {
			b.Fatalf("processBody: %v", err)
		}
		if stats.Kept != 100 {
			b.Fatalf("kept = %d, want 100 (all nodes should survive)", stats.Kept)
		}
	}
}

// The two benchmarks below drive the sink the /stable.txt worker uses —
// sliceSink, which clones each survivor's line — where BenchmarkProcessBodyPipeline
// above drives the "/" endpoint's rendering sink. Both bodies are bare-IPv4
// nodes so no DNS is touched, which is also the corpus's majority shape: of
// 73256 nodes measured across all 163 configured sources on 2026-08-14, 53066
// (72%) carry a literal IP and 20190 carry a hostname over 8327 unique names.
//
// The country filter is armed the way the worker arms it — every country
// allowed, a deny list non-empty — so filter.Permitted runs per node instead of
// early-returning on a full allow set with an empty deny set.
const (
	benchSliceNodes   = 11000 // the largest configured source: 3.06 MB, ~11k nodes
	benchSmallNodes   = 140   // the median 38 KB body
	benchSmallSources = 157   // sources answering non-empty in one cycle
)

// benchIPBody builds an already-normalized body of nodes whose server is a bare
// address inside benchGeofeed's NL block, at the corpus's measured 267 B/node.
func benchIPBody(nodes int) []byte {
	var buf bytes.Buffer
	buf.Grow(nodes * 280)
	for i := range nodes {
		if i > 0 {
			buf.WriteByte('\n')
		}
		buf.WriteString("vless://b831381d-6324-4d53-ad4f-8cda48b30811@198.51.100.")
		buf.WriteString(strconv.Itoa(i%254 + 1))
		buf.WriteString(":443?security=reality&sni=www.example.org&fp=chrome")
		buf.WriteString("&pbk=UO3EObgU3xUrhIGEE0gfCn5ZOz8YxNcwwW6ZaYzD3SA")
		buf.WriteString("&sid=4e9b0c2d1a3f5768&type=tcp&flow=xtls-rprx-vision#Node ")
		buf.WriteString(strconv.Itoa(i))
	}
	return buf.Bytes()
}

func benchProcessBodySlice(b *testing.B, bodies [][]byte, wantKept int) {
	b.Helper()
	p := newBenchProcessor(b)
	lookup := p.GeofeedState().Lookup
	resolved := p.resolver.GetResolvedMap()
	defer p.resolver.PutResolvedMap(resolved)
	ctx := context.Background()

	b.ReportAllocs()
	for b.Loop() {
		kept := 0
		for _, body := range bodies {
			clear(resolved)
			stats := Stats{}
			sink := &sliceSink{}
			pctx := &PipelineContext{
				sink:     sink,
				Lookup:   lookup,
				Allowed:  filter.All(),
				Denied:   filter.ParseAllowed("RU,CN"),
				Resolved: resolved,
				Stats:    &stats,
			}
			if err := p.processBody(ctx, body, pctx); err != nil {
				b.Fatalf("processBody: %v", err)
			}
			kept += len(sink.nodes)
		}
		if kept != wantKept {
			b.Fatalf("kept = %d, want %d", kept, wantKept)
		}
	}
}

func BenchmarkProcessBodySlice_LargestSource(b *testing.B) {
	benchProcessBodySlice(b, [][]byte{benchIPBody(benchSliceNodes)}, benchSliceNodes)
}

func BenchmarkProcessBodySlice_ManySmallSources(b *testing.B) {
	bodies := make([][]byte, benchSmallSources)
	for i := range bodies {
		bodies[i] = benchIPBody(benchSmallNodes)
	}
	benchProcessBodySlice(b, bodies, benchSmallSources*benchSmallNodes)
}

// The benchmarks below drive the /stable.txt worker's sink at the two shapes
// the shipped instances have, measured live 2026-08-14 over every configured
// source of each (config/sources.yaml + the crawler's private.yaml, and
// config-vassago/sources.yaml):
//
//	instance          answering   lines    nodes   IP-stage survivors
//	config/             148/161    71512    69163    66495  (1.08x)
//	config-vassago/      52/54    108742   100664    15904  (6.84x)
//
// The multiplier is lines/survivors: what reserve() asks for against what
// lands in the slice. Both fixtures carry the measured share of lines that
// parse to no node at all (3.3% and 7.4%) — the reservation is per LINE, so a
// junk line inflates it exactly as a surviving one does.
//
// The drop stage is the cidr allow-list at both shapes, so the two differ ONLY
// in the survival ratio. Production's permissive instance configures no cidr
// filter and loses its 3.9% to DNS failures instead, which the sink cannot
// distinguish: a node dropped at any IP stage never reaches emit. Servers are
// bare IPv4 throughout, so no DNS and no network is touched.
const (
	benchPermissiveSources = 148
	benchPermissiveLines   = 483 // 71512/148
	benchPermissiveJunk    = 33  // per 1000 lines
	benchPermissiveKept    = 961 // per 1000 node lines
	benchFilteringSources  = 52
	benchFilteringLines    = 2091 // 108742/52
	benchFilteringJunk     = 74
	benchFilteringKept     = 158
	// bywarm-merged, the largest body either instance fetches (2.88 MB) and one
	// both configure: 9519 lines, 9487 nodes, 8964 survivors on config/ against
	// 1524 on config-vassago. One body, two instances, 5.9x apart.
	benchLargestLines          = 9519
	benchLargestJunk           = 3
	benchLargestKeptPermissive = 945
	benchLargestKeptFiltering  = 161
)

// benchCIDRProcessor builds the processor the sink benchmarks share: one
// offline cidr allow-list covering 198.51.100.0/24, the block benchShapeBody
// puts surviving nodes in.
func benchCIDRProcessor(b *testing.B) *Processor {
	b.Helper()
	set, skipped := cidrset.Parse([]byte("198.51.100.0/24\n"))
	if skipped != 0 || set.Len() != 1 {
		b.Fatalf("fixture allow-list parsed to %d ranges, %d lines skipped", set.Len(), skipped)
	}
	p, err := NewProcessor(context.Background(), zerolog.Nop(), Options{
		PreloadedGeofeed: GeoState{Lookup: benchGeofeed(), LoadedAt: time.Now()},
		IPFilters: []config.IPFilterSpec{
			{Type: config.FilterCIDR, RefreshInterval: 24 * time.Hour},
			{Type: config.FilterCountry, Provider: config.ProviderGeofeed},
		},
		cidrLoad: func(context.Context) (cidrset.Set, int, error) { return set, 0, nil },
	})
	if err != nil {
		b.Fatalf("NewProcessor: %v", err)
	}
	return p
}

// benchShapeBody builds one source body: junkPerMille of every 1000 lines
// carry no URI at all, and keptPerMille of every 1000 nodes sit inside the
// allow-list block while the rest sit in 203.0.113.0/24, outside it.
func benchShapeBody(lines, junkPerMille, keptPerMille int) (body []byte, kept int) {
	var buf bytes.Buffer
	buf.Grow(lines * 300)
	node := 0
	for i := range lines {
		if i > 0 {
			buf.WriteByte('\n')
		}
		if i%1000 < junkPerMille {
			buf.WriteString("plain junk: no scheme separator anywhere on this line")
			continue
		}
		block := "203.0.113."
		if node%1000 < keptPerMille {
			block = "198.51.100."
			kept++
		}
		node++
		buf.WriteString("vless://b831381d-6324-4d53-ad4f-8cda48b30811@")
		buf.WriteString(block)
		buf.WriteString(strconv.Itoa(i%254 + 1))
		buf.WriteString(":443?security=reality&sni=www.example.org&fp=chrome")
		buf.WriteString("&pbk=UO3EObgU3xUrhIGEE0gfCn5ZOz8YxNcwwW6ZaYzD3SA")
		buf.WriteString("&sid=4e9b0c2d1a3f5768&type=tcp&flow=xtls-rprx-vision#Node ")
		buf.WriteString(strconv.Itoa(i))
	}
	return buf.Bytes(), kept
}

// benchCollectSurvivors runs one cycle's sources through the sink the worker
// takes, reporting the dead capacity the returned slices still hold — the
// quantity B/op cannot show, since an over-reservation is one allocation
// however much of it is never written.
func benchCollectSurvivors(b *testing.B, bodies [][]byte, wantKept int) {
	b.Helper()
	p := benchCIDRProcessor(b)
	lookup := p.GeofeedState().Lookup
	resolved := p.resolver.GetResolvedMap()
	defer p.resolver.PutResolvedMap(resolved)
	ctx := context.Background()

	dead, tail := 0, 0
	b.ReportAllocs()
	for b.Loop() {
		kept, deadCap, tailCap := 0, 0, 0
		for _, body := range bodies {
			clear(resolved)
			stats := Stats{}
			sink := &sliceSink{}
			pctx := &PipelineContext{
				sink:     sink,
				Lookup:   lookup,
				Allowed:  filter.All(),
				Resolved: resolved,
				Stats:    &stats,
			}
			if err := p.processBody(ctx, body, pctx); err != nil {
				b.Fatalf("processBody: %v", err)
			}
			nodes := sink.fit()
			kept += len(nodes)
			deadCap += cap(nodes) - len(nodes)
			tailCap += cap(sink.arena) - len(sink.arena)
		}
		if kept != wantKept {
			b.Fatalf("kept = %d, want %d", kept, wantKept)
		}
		dead, tail = deadCap, tailCap
	}
	b.ReportMetric(float64(dead)*float64(unsafe.Sizeof(NodeResult{})), "deadB/cycle")
	b.ReportMetric(float64(tail), "tailB/cycle")
}

func benchShapeBodies(sources, lines, junk, kept int) ([][]byte, int) {
	bodies := make([][]byte, sources)
	body, keptOne := benchShapeBody(lines, junk, kept)
	for i := range bodies {
		bodies[i] = body
	}
	return bodies, keptOne * sources
}

func BenchmarkCollectSurvivors_Permissive(b *testing.B) {
	bodies, kept := benchShapeBodies(benchPermissiveSources, benchPermissiveLines,
		benchPermissiveJunk, benchPermissiveKept)
	benchCollectSurvivors(b, bodies, kept)
}

func BenchmarkCollectSurvivors_Filtering(b *testing.B) {
	bodies, kept := benchShapeBodies(benchFilteringSources, benchFilteringLines,
		benchFilteringJunk, benchFilteringKept)
	benchCollectSurvivors(b, bodies, kept)
}

func BenchmarkCollectSurvivors_LargestSourcePermissive(b *testing.B) {
	bodies, kept := benchShapeBodies(1, benchLargestLines, benchLargestJunk, benchLargestKeptPermissive)
	benchCollectSurvivors(b, bodies, kept)
}

func BenchmarkCollectSurvivors_LargestSourceFiltering(b *testing.B) {
	bodies, kept := benchShapeBodies(1, benchLargestLines, benchLargestJunk, benchLargestKeptFiltering)
	benchCollectSurvivors(b, bodies, kept)
}
