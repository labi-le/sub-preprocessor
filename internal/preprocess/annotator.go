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

// Egress is what a node reported about itself through the cloudflare probe. The
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
// cloudflare step, and it is the one provider that cannot be a geo.Provider.
// cloudflare IS a geo-IP database like its siblings — the tag carries its
// loc= verdict, not the ip= line — but a Provider is handed an ADDRESS to look
// up, and this one is not asked about the address our resolver returned. It is
// asked about the address the traffic actually LEFT from, which only the node
// itself can report and only through a proxy that exists. That is the whole
// value of the provider, and it is why the answer is MEASURED post-probe
// rather than looked up: it is also the only provider that cannot answer on
// GET /, where no probe has run.
//
// So the step repeats what the node already said (req.Egress) instead of
// resolving anything, and cloudflare is absent from the provider map
// newAnnotator resolves names against: a name missing from that map is a
// wiring bug worth an Error log, and cloudflare would trip it on every
// processor build.
type annotStep struct {
	prov geo.Provider
}

// annotTag is one resolved annotation tag: a key and the ordered chain that
// resolves it (first step that answers wins). GEO is the only key the config
// loader accepts today, so key is what keeps Annotate rendering nothing for a
// spec built in code with any other tag — the chainless kind went with the IP
// tag, and the second provider-backed kind with the ASN one.
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
			if name == config.ProviderCloudflare {
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
		// GEO is the only tag config.validateAnnotate accepts, so a spec with
		// any other key renders nothing rather than a "[??]" of unknown kind.
		if t.key != config.TagGEO {
			continue
		}
		c := t.lookupCountry(ctx, req)
		// Nothing forbids a second GEO entry with a different chain; the
		// caller gets the leftmost tag that resolved, which is the one a
		// reader of the name takes for the node's country. That cross-entry
		// rule is the country FILTER's too: countryChainOrder concatenates
		// every GEO entry's chain, so no LOCAL database a tag resolved
		// through went unconsulted by the filter. asn and cloudflare are the
		// standing exceptions — neither is a local table (see countryChain),
		// so a tag either of them answered still names a country the filter
		// never asked about. Measured on `[{GEO,[cloudflare,geofeed]}]`, the
		// shipped chain's first two providers, with a geofeed that cannot
		// place the IP: countryChainOrder comes back empty, the node survives
		// exclude_countries=DE, and the traced egress publishes [GEO:DE].
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
	}
	rewrite.NodeName(dst, req.Node, scratch.String())
	return country
}

// lookupCountry walks the tag's chain and returns the first non-zero country;
// all-miss returns the zero code (rendered as ??). The cloudflare step answers
// only when the trace ran: an unmeasured egress is a miss, so a chain that
// names cloudflare first still annotates every node the probe skipped.
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
