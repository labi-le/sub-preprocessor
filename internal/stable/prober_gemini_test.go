package stable

import (
	"testing"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
)

func TestGeminiBlocked(t *testing.T) {
	t.Parallel()

	const marker = "User location is not supported for the API use"
	if !geminiBlocked(`{"error":{"message":"User location is not supported for the API use."}}`, marker) {
		t.Fatal("geo-block body should be detected")
	}
	if geminiBlocked(`{"models":[{"name":"gemini-2.0-flash"}]}`, marker) {
		t.Fatal("a normal response body must not be flagged")
	}
	if geminiBlocked("anything", "") {
		t.Fatal("empty marker must never match")
	}
}

func TestGeminiURLAndEnabled(t *testing.T) {
	t.Parallel()

	p, err := NewMihomoProber(
		config.CheckConfig{ExpectedStatus: "204"},
		config.GeminiConfig{Enabled: true, Endpoint: "https://generativelanguage.googleapis.com/", Model: "gemini-2.0-flash"},
		"SECRET",
		zerolog.Nop(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !p.GeminiEnabled() {
		t.Fatal("should be enabled with a key")
	}
	want := "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.0-flash?key=SECRET"
	if got := p.geminiURL(); got != want {
		t.Fatalf("geminiURL = %q, want %q", got, want)
	}

	off, err := NewMihomoProber(config.CheckConfig{ExpectedStatus: "204"}, config.GeminiConfig{Enabled: true}, "", zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if off.GeminiEnabled() {
		t.Fatal("no key must disable the gate")
	}
}
