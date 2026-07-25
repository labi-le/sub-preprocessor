package stable //nolint:testpackage // exercises unexported stable internals

import (
	"testing"

	"domains.lst/sub-preprocessor/internal/config"
)

// Genuine bodies captured from api.openai.com/compliance/cookie_requirements
// through live proxy nodes: 403 for an egress OpenAI refuses, 200 otherwise.
const (
	chatgptBlockedBody = `{"error":{"message":"OpenAI's services are not available in your country.",` +
		`"type":"invalid_request_error","param":null,"code":"unsupported_country"}}`
	chatgptAllowedBody = `{"cookie_consent_required":true}`
	// What the keyless /v1/models endpoint returns instead: it never consults
	// the region, so classifying it as a block would drop every node.
	chatgptNoBearerBody = `{"error":{"message":"Missing bearer authentication in header",` +
		`"type":"invalid_request_error","param":null,"code":null}}`
)

func TestChatGPTBlockedClassifiesRealBodies(t *testing.T) {
	t.Parallel()

	const marker = "unsupported_country"

	if !markerBlocked(chatgptBlockedBody, marker) {
		t.Fatal("403 unsupported_country body must count as blocked")
	}
	if markerBlocked(chatgptAllowedBody, marker) {
		t.Fatal("200 cookie_consent_required body must not count as blocked")
	}
	if markerBlocked(chatgptNoBearerBody, marker) {
		t.Fatal("missing-bearer body must not count as blocked")
	}
	if markerBlocked(chatgptBlockedBody, "") {
		t.Fatal("empty marker must never block")
	}
}

// TestChatGPTURLTargetsComplianceEndpoint pins the gate to the compliance path.
// /v1/models is the tempting analogue of the Claude check but is measurably
// wrong here: keyless it answers 401 regardless of region, so the filter would
// silently keep every node.
func TestChatGPTURLTargetsComplianceEndpoint(t *testing.T) {
	t.Parallel()

	m := &MihomoProber{geo: config.GeoBlockConfig{
		ChatGPT: config.ChatGPTConfig{Endpoint: "https://api.openai.com/"},
	}}
	if got, want := m.chatgptURL(), "https://api.openai.com/compliance/cookie_requirements"; got != want {
		t.Fatalf("chatgptURL() = %q, want %q", got, want)
	}
}
