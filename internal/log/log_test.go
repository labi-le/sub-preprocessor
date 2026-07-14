package log_test

import (
	"bytes"
	"strings"
	"testing"

	ilog "domains.lst/sub-preprocessor/internal/log"
	"github.com/rs/zerolog"
)

func TestSetLevel_LiveChange(t *testing.T) {
	prev := zerolog.GlobalLevel()
	t.Cleanup(func() { zerolog.SetGlobalLevel(prev) })
	// InitLogger("info") → Debug suppressed → SetLevel("debug") → Debug appears
	var buf bytes.Buffer
	logger := ilog.InitLogger("info")
	logger = logger.Output(&buf)
	logger.Debug().Msg("d1")
	if strings.Contains(buf.String(), "d1") {
		t.Fatal("debug should be suppressed at info level")
	}
	if err := ilog.SetLevel("debug"); err != nil {
		t.Fatalf("SetLevel: %v", err)
	}
	logger.Debug().Msg("d2")
	if !strings.Contains(buf.String(), "d2") {
		t.Fatal("debug should appear after SetLevel(debug)")
	}
}

func TestSetLevel_InvalidLevel(t *testing.T) {
	prev := zerolog.GlobalLevel()
	t.Cleanup(func() { zerolog.SetGlobalLevel(prev) })

	if err := ilog.SetLevel("not-a-level"); err == nil {
		t.Fatal("expected error for invalid level")
	}
	if zerolog.GlobalLevel() != prev {
		t.Fatalf("level should remain %v after invalid SetLevel, got %v", prev, zerolog.GlobalLevel())
	}
}

func TestInitLogger_ReturnsLogger(t *testing.T) {
	prev := zerolog.GlobalLevel()
	t.Cleanup(func() { zerolog.SetGlobalLevel(prev) })

	logger := ilog.InitLogger("warn")
	// zerolog.Logger is a value type; just verify it's usable
	_ = logger.With().Str("k", "v").Logger()
}

func TestSetLevel_GlobalAffectsNewLoggers(t *testing.T) {
	prev := zerolog.GlobalLevel()
	t.Cleanup(func() { zerolog.SetGlobalLevel(prev) })

	_ = ilog.InitLogger("info")
	if err := ilog.SetLevel("warn"); err != nil {
		t.Fatalf("SetLevel: %v", err)
	}
	// global level should now be warn
	if zerolog.GlobalLevel() != zerolog.WarnLevel {
		t.Fatalf("expected global level warn, got %v", zerolog.GlobalLevel())
	}
}
