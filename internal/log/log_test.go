package log_test

import (
	"bytes"
	"strings"
	"testing"

	ilog "domains.lst/sub-preprocessor/internal/log"
	"github.com/rs/zerolog"
)

func TestSetLevel_LiveChange(t *testing.T) {
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
	if err := ilog.SetLevel("not-a-level"); err == nil {
		t.Fatal("expected error for invalid level")
	}
	// level should remain unchanged (debug from previous test or info)
}

func TestInitLogger_ReturnsLogger(t *testing.T) {
	logger := ilog.InitLogger("warn")
	// zerolog.Logger is a value type; just verify it's usable
	_ = logger.With().Str("k", "v").Logger()
}

func TestSetLevel_GlobalAffectsNewLoggers(t *testing.T) {
	_ = ilog.InitLogger("info")
	if err := ilog.SetLevel("warn"); err != nil {
		t.Fatalf("SetLevel: %v", err)
	}
	// global level should now be warn
	if zerolog.GlobalLevel() != zerolog.WarnLevel {
		t.Fatalf("expected global level warn, got %v", zerolog.GlobalLevel())
	}
	// reset
	_ = ilog.SetLevel("info")
}
