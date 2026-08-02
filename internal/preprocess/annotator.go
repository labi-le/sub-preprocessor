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

// Egress is what a node reported about itself through the geotrace probe. The
// zero value means the probe never ran — on `GET /` it never does, and in the
// worker only nodes that survived the latency probe are traced.
type Egress struct {
	IP      netip.Addr
	Country geofeed.CountryCode
}

// Valid reports whether the trace answered. A trace that answered pins the
// address the node actually leaves from; without one the caller has nothing
// but the address the resolver returned.
func (e Egress) Valid() bool { return e.IP.IsValid() }

// AnnotateRequest is one node to annotate. IP is the address the tags describe
// — the caller picks it (the traced egress when there is one, the resolved
// address otherwise), because only the caller knows which of the two it wants
// published. Prefix is written ahead of the configured tags verbatim, for tags
// the caller renders itself (`[SPD:60M] `).
type AnnotateRequest struct {
	Node   subscription.Node
	IP     netip.Addr
	Egress Egress
	Prefix string
}

// Annotator renders the tag prefix into a node's name.
type Annotator interface {
	// Annotate writes the annotated node line to dst and returns the country
	// the GEO chain resolved (the zero code when nothing resolved it, or when
	// no GEO tag is configured). scratch is caller-owned and reused across
	// nodes to keep prefix assembly allocation-light.
	Annotate(ctx context.Context, dst, scratch *bytes.Buffer, req AnnotateRequest) geofeed.CountryCode
}

// annotStep is one link of a tag's resolution chain. A nil prov marks the
// geotrace step: it resolves nothing, it repeats what the node said about
// itself, so there is no geo.Provider behind it — a Provider looks an address
// up, and no address lookup can tell where a proxy's traffic leaves from.
//
// That is also why geotrace is absent from the provider map newAnnotator
// resolves names against: a name missing from that map is a wiring bug worth
// an Error log, and geotrace would trip it on every processor build.
type annotStep struct {
	prov geo.Provider
}

// annotTag is one resolved annotation tag: a key (GEO/IP/ASN) and, for
// provider-backed tags (GEO/ASN), the ordered chain that resolves it (first
// step that answers wins).
type annotTag struct {
	key   string
	chain []annotStep
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
			if name == config.ProviderGeoTrace {
				t.chain = append(t.chain, annotStep{})
				continue
			}
			prov, ok := providers[name]
			if !ok || prov == nil {
				logger.Error().Str("tag", s.Tag).Str("provider", name).
					Msg("annotate provider referenced but not built; skipping")
				continue
			}
			t.chain = append(t.chain, annotStep{prov: prov})
		}
		tags = append(tags, t)
	}
	return &annotator{tags: tags}
}

func (a *annotator) Annotate(
	ctx context.Context,
	dst, scratch *bytes.Buffer,
	req AnnotateRequest,
) geofeed.CountryCode {
	scratch.Reset()
	scratch.WriteString(req.Prefix)
	var country geofeed.CountryCode
	for _, t := range a.tags {
		switch t.key {
		case config.TagGEO:
			c := t.lookupCountry(ctx, req)
			// Nothing forbids a second GEO entry with a different chain; the
			// caller gets the leftmost tag that resolved, which is the one a
			// reader of the name takes for the node's country.
			if country == (geofeed.CountryCode{}) {
				country = c
			}
			scratch.WriteString("[GEO:")
			if c == (geofeed.CountryCode{}) {
				scratch.WriteString("??")
			} else {
				scratch.WriteByte(c[0])
				scratch.WriteByte(c[1])
			}
			scratch.WriteByte(']')
		case config.TagIP:
			scratch.WriteString("[IP:")
			writeIP(scratch, req.IP)
			scratch.WriteByte(']')
		case config.TagASN:
			name := t.lookupASN(ctx, req.IP)
			scratch.WriteString("[ASN:")
			if name == "" {
				scratch.WriteString("??")
			} else {
				scratch.WriteString(name)
			}
			scratch.WriteByte(']')
		}
	}
	rewrite.NodeName(dst, req.Node, scratch.String())
	return country
}

// lookupCountry walks the tag's chain and returns the first non-zero country;
// all-miss returns the zero code (rendered as ??). The geotrace step answers
// only when the trace ran: an unmeasured egress is a miss, so a chain that
// names geotrace first still annotates every node the probe skipped.
func (t *annotTag) lookupCountry(ctx context.Context, req AnnotateRequest) geofeed.CountryCode {
	for _, step := range t.chain {
		if step.prov == nil {
			if req.Egress.Valid() {
				return req.Egress.Country
			}
			continue
		}
		if c := step.prov.Lookup(ctx, req.IP).Country; c != (geofeed.CountryCode{}) {
			return c
		}
	}
	return geofeed.CountryCode{}
}

// lookupASN walks the tag's chain and returns the first non-empty AS name;
// all-miss returns "" (rendered as ??). The geotrace step is always a miss
// here: cdn-cgi/trace reports an address and a location, never an AS.
func (t *annotTag) lookupASN(ctx context.Context, ip netip.Addr) string {
	for _, step := range t.chain {
		if step.prov == nil {
			continue
		}
		if name := step.prov.Lookup(ctx, ip).ASN; name != "" {
			return name
		}
	}
	return ""
}

// writeIP renders ip digit by digit for the v4 family, which is every address
// the IPv4-only resolver produces. The branch is not cosmetic: As4 PANICS on a
// real IPv6 address, and a traced egress can be one.
func writeIP(b *bytes.Buffer, ip netip.Addr) {
	if !ip.Is4() && !ip.Is4In6() {
		b.WriteString(ip.String())
		return
	}
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
