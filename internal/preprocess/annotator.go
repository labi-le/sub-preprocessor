package preprocess

import (
	"bytes"
	"context"
	"net/netip"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/geo"
	"domains.lst/sub-preprocessor/internal/geofeed"
	"domains.lst/sub-preprocessor/internal/rewrite"
	"domains.lst/sub-preprocessor/internal/subscription"
	"github.com/rs/zerolog"
)

const (
	decimalBase = 10
	hundred     = 100
)

// annotTag is one resolved annotation tag: a key (GEO/IP/ASN) and, for
// provider-backed tags (GEO/ASN), the ordered provider chain that resolves it
// (first provider that answers wins).
type annotTag struct {
	key       string
	providers []geo.Provider
}

// annotator builds the ordered [KEY:VAL] tag prefix for a node's chosen IP and
// writes the relabeled node via rewrite.NodeName. A nil annotator means
// annotation is disabled (the raw node is emitted verbatim).
type annotator struct {
	tags []annotTag
}

// newAnnotator builds an annotator from the ordered tag specs, resolving each
// spec's provider chain against the map of providers the processor actually
// built. A referenced-but-missing name is impossible by the lazy-build rule in
// NewProcessor, so it is treated as a wiring bug: logged and skipped rather
// than panicking, degrading one provider instead of the service. It returns
// nil when no tags are configured.
func newAnnotator(logger zerolog.Logger, specs []config.AnnotateSpec, providers map[string]geo.Provider) *annotator {
	if len(specs) == 0 {
		return nil
	}
	tags := make([]annotTag, 0, len(specs))
	for _, s := range specs {
		t := annotTag{key: s.Tag}
		for _, name := range s.Providers {
			prov, ok := providers[name]
			if !ok || prov == nil {
				logger.Error().Str("tag", s.Tag).Str("provider", name).
					Msg("annotate provider referenced but not built; skipping")
				continue
			}
			t.providers = append(t.providers, prov)
		}
		tags = append(tags, t)
	}
	return &annotator{tags: tags}
}

// annotate writes node to buf with the configured tag prefix folded into its
// name. tagBuf is a caller-owned scratch buffer reused across nodes to keep the
// prefix assembly allocation-light.
func (a *annotator) annotate(ctx context.Context, buf, tagBuf *bytes.Buffer, node subscription.Node, ip netip.Addr) {
	tagBuf.Reset()
	for _, t := range a.tags {
		switch t.key {
		case config.TagGEO:
			c := t.lookupCountry(ctx, ip)
			tagBuf.WriteString("[GEO:")
			if c == (geofeed.CountryCode{}) {
				tagBuf.WriteString("??")
			} else {
				tagBuf.WriteByte(c[0])
				tagBuf.WriteByte(c[1])
			}
			tagBuf.WriteByte(']')
		case config.TagIP:
			tagBuf.WriteString("[IP:")
			writeIPv4(tagBuf, ip)
			tagBuf.WriteByte(']')
		case config.TagASN:
			name := t.lookupASN(ctx, ip)
			tagBuf.WriteString("[ASN:")
			if name == "" {
				tagBuf.WriteString("??")
			} else {
				tagBuf.WriteString(name)
			}
			tagBuf.WriteByte(']')
		}
	}
	rewrite.NodeName(buf, node, tagBuf.String())
}

// lookupCountry walks the tag's provider chain and returns the first non-zero
// country; all-miss returns the zero code (rendered as ??).
func (t *annotTag) lookupCountry(ctx context.Context, ip netip.Addr) geofeed.CountryCode {
	for _, prov := range t.providers {
		if c := prov.Lookup(ctx, ip).Country; c != (geofeed.CountryCode{}) {
			return c
		}
	}
	return geofeed.CountryCode{}
}

// lookupASN walks the tag's provider chain and returns the first non-empty AS
// name; all-miss returns "" (rendered as ??).
func (t *annotTag) lookupASN(ctx context.Context, ip netip.Addr) string {
	for _, prov := range t.providers {
		if name := prov.Lookup(ctx, ip).ASN; name != "" {
			return name
		}
	}
	return ""
}

func writeIPv4(b *bytes.Buffer, ip netip.Addr) {
	ip4 := ip.As4()
	writeOctet(b, ip4[0])
	b.WriteByte('.')
	writeOctet(b, ip4[1])
	b.WriteByte('.')
	writeOctet(b, ip4[2])
	b.WriteByte('.')
	writeOctet(b, ip4[3])
}

func writeOctet(b *bytes.Buffer, n byte) {
	switch {
	case n >= hundred:
		b.WriteByte('0' + n/hundred)
		b.WriteByte('0' + (n/decimalBase)%decimalBase)
		b.WriteByte('0' + n%decimalBase)
	case n >= decimalBase:
		b.WriteByte('0' + n/decimalBase)
		b.WriteByte('0' + n%decimalBase)
	default:
		b.WriteByte('0' + n)
	}
}
