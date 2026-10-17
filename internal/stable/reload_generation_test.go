package stable //nolint:testpackage // drives the Controller's published generation directly

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/geofeed"
	"domains.lst/sub-preprocessor/internal/preprocess"
	"domains.lst/sub-preprocessor/internal/reload"
)

// generationLookup preloads every Processor with a country table, so no test
// here touches the geofeed source.
type generationLookup struct{}

func (generationLookup) LookupCountry(netip.Addr) geofeed.CountryCode {
	return geofeed.CountryCode{'N', 'L'}
}

// taggedFilterer stands for one processor generation and never fetches, so the
// worker's background cycle is inert. Its tag is how the test tells WHICH
// processor a published spec carries.
type taggedFilterer struct{ tag string }

func (t taggedFilterer) FilterNodes(
	context.Context, preprocess.FilterRequest,
) ([]preprocess.NodeResult, preprocess.Stats, error) {
	return nil, preprocess.Stats{}, errors.New("tagged filterer: no fetch in tests")
}

//nolint:ireturn // implements Filterer; handing out the interface is the point
func (t taggedFilterer) Annotator() preprocess.Annotator { return nil }

const generationGeoYAML = "geo:\n" +
	"  geofeed:\n" +
	"    sources:\n" +
	"      - url: https://127.0.0.1:1/geofeed\n" +
	"        type: raw\n"

const generationSourcesYAML = generationGeoYAML +
	"subscriptions:\n" +
	"  sources:\n" +
	"    - name: alpha\n" +
	"      url: https://alpha.example/sub\n"

func generationProcessor(t *testing.T, cfg config.Config) *preprocess.Processor {
	t.Helper()

	opts := reload.OptionsFromConfig(cfg)
	opts.PreloadedGeofeed = preprocess.GeoState{Lookup: generationLookup{}, LoadedAt: time.Now().Add(-time.Hour)}
	p, err := preprocess.NewProcessor(t.Context(), zerolog.Nop(), opts)
	if err != nil {
		t.Fatalf("build processor: %v", err)
	}

	return p
}

// TestReloadRefusedConfigKeepsPublishedGeneration is the A -> refused B ->
// unrelated C regression: a config with no sources is refused, so the worker
// keeps A's sources, policy AND processor. C differs from B only in a
// processor-owned setting; if the refusal were committed as the applied config,
// C would look like a processor-only reload and publish B/C's processor against
// A's policy -- the mixed generation this contract forbids.
func TestReloadRefusedConfigKeepsPublishedGeneration(t *testing.T) {
	// The reloader applies the config's log level GLOBALLY, and zerolog gates
	// even a per-logger level by the global one: a reload left at the default
	// info level would hide the debug events the trace tests below count. Run
	// sequentially (parallel tests are paused until every sequential test
	// returns) and restore the level.
	prevLevel := zerolog.GlobalLevel()
	t.Cleanup(func() { zerolog.SetGlobalLevel(prevLevel) })

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
	}
	write(generationSourcesYAML)

	cfgA, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config A: %v", err)
	}
	procA := generationProcessor(t, cfgA)
	tags := map[*preprocess.Processor]string{procA: "A"}
	holder := reload.NewHolder(procA)

	ctl := NewController(t.Context(), NewHolder(), func() Filterer {
		return taggedFilterer{tag: tags[holder.Load()]}
	}, nil, nil, "", zerolog.Nop(), nil)
	defer ctl.Stop()

	if applied, applyErr := ctl.Apply(cfgA); applyErr != nil || !applied {
		t.Fatalf("Apply(A): applied=%v err=%v", applied, applyErr)
	}
	spec := ctl.checker.spec.Load()
	if spec.Filterer != (taggedFilterer{tag: "A"}) || len(spec.Sources) != 1 {
		t.Fatalf("startup generation: filterer=%v sources=%d, want A/1", spec.Filterer, len(spec.Sources))
	}

	r := reload.NewReloader(path, holder, zerolog.Nop(), cfgA, procA, ctl, nil)

	write(generationGeoYAML) // B: no sources, refused
	r.Reload(t.Context())
	if spec = ctl.checker.spec.Load(); spec.Filterer != (taggedFilterer{tag: "A"}) || len(spec.Sources) != 1 {
		t.Fatalf("after the refusal: filterer=%v sources=%d, want A/1", spec.Filterer, len(spec.Sources))
	}

	write(generationGeoYAML + "resolver:\n  timeout: 10s\n") // C: differs from B only in the resolver
	r.Reload(t.Context())
	spec = ctl.checker.spec.Load()
	if spec.Filterer != (taggedFilterer{tag: "A"}) || len(spec.Sources) != 1 {
		t.Fatalf("refused configuration's processor published with old A sources/policy after unrelated reload: filterer=%v sources=%d",
			spec.Filterer, len(spec.Sources))
	}
}
