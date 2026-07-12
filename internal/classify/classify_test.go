package classify_test

import (
	"encoding/base64"
	"testing"

	"domains.lst/sub-preprocessor/internal/classify"
)

func TestBodyCountsNodes(t *testing.T) {
	t.Parallel()

	raw := "vless://u@1.1.1.1:443?security=reality#a\nvless://u@2.2.2.2:443#b\n"
	body := []byte(base64.StdEncoding.EncodeToString([]byte(raw)))

	got := classify.Body(body, "", 1000)
	if got.Nodes != 2 {
		t.Fatalf("Nodes = %d, want 2", got.Nodes)
	}
	if !got.Live() {
		t.Fatalf("expected Live for a 2-node body")
	}
}

func TestBodyPlainNotBase64(t *testing.T) {
	t.Parallel()

	body := []byte("vless://u@1.1.1.1:443#a\n")
	if got := classify.Body(body, "", 1000); got.Nodes != 1 || !got.Live() {
		t.Fatalf("plain body: got %+v, want 1 live node", got)
	}
}

func TestBodyExpiredNotLive(t *testing.T) {
	t.Parallel()

	body := []byte(base64.StdEncoding.EncodeToString([]byte("vless://u@1.1.1.1:443#a\n")))
	// expire=500 is before now=1000 → expired.
	got := classify.Body(body, "upload=0; download=0; total=0; expire=500", 1000)
	if !got.Expired {
		t.Fatalf("expected Expired for past expiry")
	}
	if got.Live() {
		t.Fatalf("expired body must not be Live")
	}
}

func TestBodyFutureExpiryLive(t *testing.T) {
	t.Parallel()

	body := []byte(base64.StdEncoding.EncodeToString([]byte("vless://u@1.1.1.1:443#a\n")))
	got := classify.Body(body, "expire=2000", 1000)
	if got.Expired || !got.Live() {
		t.Fatalf("future expiry: got %+v, want live non-expired", got)
	}
}

func TestBodyNoNodesNotLive(t *testing.T) {
	t.Parallel()

	if got := classify.Body([]byte("just some prose, no nodes"), "", 1000); got.Nodes != 0 || got.Live() {
		t.Fatalf("prose body: got %+v, want 0 nodes not live", got)
	}
}

func TestBodyRejectsHTMLLinks(t *testing.T) {
	t.Parallel()

	// An HTML page full of http(s):// links must not look like a subscription,
	// even though the generic node parser would accept those authorities.
	body := []byte(`<html><a href="https://kod.ru/article">x</a>` +
		`<link href="https://cdn.example.com:443/s.css"><a href="https://t.me/chan">y</a></html>`)
	if got := classify.Body(body, "", 1000); got.Nodes != 0 || got.Live() {
		t.Fatalf("HTML page must not classify as subscription, got %+v", got)
	}
}
