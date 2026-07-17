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
)

const (
	decimalBase = 10
	hundred     = 100
)

// annotTag is one resolved annotation tag: a key (GEO/IP/ASN) and, for
// provider-backed tags (GEO/ASN), the geo provider that resolves it.
type annotTag struct {
	key      string
	provider geo.Provider
}

// annotator builds the ordered [KEY:VAL] tag prefix for a node's chosen IP and
// writes the relabeled node via rewrite.NodeName. A nil annotator means
// annotation is disabled (the raw node is emitted verbatim).
type annotator struct {
	tags []annotTag
}

// newAnnotator builds an annotator from the ordered tag specs, wiring each
// provider-backed tag to the shared geofeed/ASN providers. It returns nil when
// no tags are configured.
func newAnnotator(specs []config.AnnotateSpec, geofeedProv, asnProv geo.Provider) *annotator {
	if len(specs) == 0 {
		return nil
	}
	tags := make([]annotTag, 0, len(specs))
	for _, s := range specs {
		t := annotTag{key: s.Tag}
		switch s.Provider {
		case config.ProviderGeofeed:
			t.provider = geofeedProv
		case config.ProviderASN:
			t.provider = asnProv
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
			c := t.provider.Lookup(ctx, ip).Country
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
			name := t.provider.Lookup(ctx, ip).ASN
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
