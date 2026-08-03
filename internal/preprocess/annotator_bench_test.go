package preprocess //nolint:testpackage // measures the unexported annotator directly

import (
	"bytes"
	"context"
	"net/netip"
	"testing"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/geo"
	"domains.lst/sub-preprocessor/internal/geofeed"
	"domains.lst/sub-preprocessor/internal/subscription"
	"github.com/rs/zerolog"
)

// BenchmarkAnnotate measures the annotator alone — tag-list walk, prefix
// assembly, upstream-tag strip and fragment rewrite — over the shipped shape:
// ONE tag, GEO, which since the ASN tag was retired is the only one the loader
// accepts. The providers are fakes returning a constant, so no geo database
// lookup is inside the number, and the tag list is fixed HERE rather than in a
// processor fixture. That is the whole point of this benchmark:
// BenchmarkProcessBodyPipeline takes its annotate list from newBenchProcessor,
// and `5d06fb6` edited that list, which moved the pipeline number 14% with the
// production code held constant. A move in THIS number is the annotate code —
// except across the ASN removal, which dropped this fixture's second tag too;
// see the AGENTS.md bench note for the control that separates the two.
func BenchmarkAnnotate(b *testing.B) {
	a := newAnnotator(zerolog.Nop(), []config.AnnotateSpec{
		{Tag: config.TagGEO, Providers: []string{config.ProviderGeofeed}},
	}, map[string]geo.Provider{
		config.ProviderGeofeed: fakeProvider{name: "geofeed", info: geo.Info{Country: geofeed.CountryCode{'N', 'L'}}},
	})
	if a == nil {
		b.Fatal("newAnnotator returned nil")
	}

	// The upstream name carries a tag of its own so StripKnownTags does the
	// work a relabel really pays for, not the empty-prefix short circuit.
	var node subscription.Node
	subscription.Parse([]byte("vless://u@198.51.100.1:443#[GEO:RU] Old Name"), func(n subscription.Node) bool {
		node = n

		return false
	})
	if node.Scheme == "" {
		b.Fatal("no node parsed")
	}

	req := AnnotateRequest{Node: node, IP: netip.MustParseAddr("198.51.100.1")}
	var dst, scratch bytes.Buffer
	dst.Grow(256)
	scratch.Grow(64)

	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		dst.Reset()
		a.Annotate(ctx, &dst, &scratch, req)
	}
}
