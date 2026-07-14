package stable //nolint:testpackage // exercises unexported stable internals

import (
	"testing"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
)

func TestClaudeBlocked(t *testing.T) {
	t.Parallel()

	const marker = "Request not allowed"
	if !claudeBlocked(`{"error":{"type":"forbidden","message":"Request not allowed"}}`, marker) {
		t.Fatal("geo-block body should be detected")
	}
	if claudeBlocked(`{"error":{"type":"authentication_error","message":"x-api-key header is required"}}`, marker) {
		t.Fatal("a keyless auth error from an allowed region must not be flagged")
	}
	if claudeBlocked(`{"data":[{"id":"claude-sonnet-4-5"}]}`, marker) {
		t.Fatal("a normal response body must not be flagged")
	}
	if claudeBlocked("anything", "") {
		t.Fatal("empty marker must never match")
	}
}

func TestClaudeURL(t *testing.T) {
	t.Parallel()

	p, err := NewMihomoProber(
		config.CheckConfig{ExpectedStatus: "204"},
		config.GeminiConfig{},
		"",
		config.ClaudeConfig{Endpoint: "https://api.anthropic.com/"},
		zerolog.Nop(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := p.claudeURL(), "https://api.anthropic.com/v1/models"; got != want {
		t.Fatalf("claudeURL = %q, want %q", got, want)
	}
}
