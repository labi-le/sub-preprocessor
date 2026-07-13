package stable

import (
	"testing"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
)

func TestBuildNodeFilters(t *testing.T) {
	t.Parallel()

	prober, err := NewMihomoProber(config.CheckConfig{ExpectedStatus: "204"}, config.GeminiConfig{}, "KEY", zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}

	if fs := buildNodeFilters(nil, prober, nil, zerolog.Nop()); len(fs) != 0 {
		t.Fatalf("no names -> no filters, got %d", len(fs))
	}

	fs := buildNodeFilters([]string{"gemini", "bogus"}, prober, nil, zerolog.Nop())
	if len(fs) != 1 {
		t.Fatalf("gemini + unknown -> 1 filter, got %d", len(fs))
	}
	if fs[0].name() != "gemini" {
		t.Fatalf("expected gemini filter, got %q", fs[0].name())
	}
}
