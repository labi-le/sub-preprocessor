package stable //nolint:testpackage // exercises unexported stable internals

import (
	"testing"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
)

func TestBuildNodeFilters(t *testing.T) {
	t.Parallel()

	prober, err := NewMihomoProber(config.CheckConfig{ExpectedStatus: "204"}, config.GeminiConfig{}, "KEY", config.ClaudeConfig{}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}

	if fs := buildNodeFilters(nil, prober, nil, zerolog.Nop()); len(fs) != 0 {
		t.Fatalf("no names -> no filters, got %d", len(fs))
	}

	fs := buildNodeFilters([]string{"gemini", "claude", "bogus"}, prober, nil, zerolog.Nop())
	if len(fs) != 2 {
		t.Fatalf("gemini + claude + unknown -> 2 filters, got %d", len(fs))
	}
	if fs[0].name() != "gemini" {
		t.Fatalf("expected gemini filter first, got %q", fs[0].name())
	}
	if fs[1].name() != "claude" {
		t.Fatalf("expected claude filter second, got %q", fs[1].name())
	}
}
