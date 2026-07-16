package fetch_test

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"testing"

	"domains.lst/sub-preprocessor/internal/fetch"
)

func TestMaybeDecodeGzip(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	resp := &http.Response{Body: io.NopCloser(bytes.NewReader(buf.Bytes()))}
	rc, err := fetch.MaybeDecode(resp, fetch.FileTypeGzip)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "hello\n" {
		t.Fatalf("unexpected body: %q", body)
	}
}

func TestValidateFileTypeRejectsAuto(t *testing.T) {
	t.Parallel()

	err := fetch.ValidateFileType("auto")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestValidatePublicHTTPSURLRejectsHTTP(t *testing.T) {
	t.Parallel()

	err := fetch.ValidatePublicHTTPSURL(fetch.SubscriptionURL("http://example.com/test"))
	if err == nil || !strings.Contains(err.Error(), "only https") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewSafeHTTPClientDisablesProxy(t *testing.T) {
	t.Parallel()

	client := fetch.NewSafeHTTPClient()
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("unexpected transport type: %T", client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("expected proxy to be disabled")
	}
}

func TestTrustedPrefixesBypassGuard(t *testing.T) {
	// Mutates package-level guard state, so not parallel; reset after.
	t.Cleanup(func() { fetch.SetTrustedPrefixes(nil) })

	const fakeIP = "https://198.18.1.15/sub" // mihomo fake-ip, inside reserved 198.18.0.0/15

	if err := fetch.ValidatePublicHTTPSURL(fetch.SubscriptionURL(fakeIP)); err == nil {
		t.Fatal("reserved fake-ip must be rejected by default")
	}

	prefixes, err := fetch.ParsePrefixes([]string{"198.18.0.0/16"})
	if err != nil {
		t.Fatalf("ParsePrefixes: %v", err)
	}
	fetch.SetTrustedPrefixes(prefixes)

	if err = fetch.ValidatePublicHTTPSURL(fetch.SubscriptionURL(fakeIP)); err != nil {
		t.Fatalf("trusted fake-ip should pass, got: %v", err)
	}
	// A private IP outside the trusted range is still rejected.
	if err = fetch.ValidatePublicHTTPSURL(fetch.SubscriptionURL("https://10.0.0.1/sub")); err == nil {
		t.Fatal("private IP outside trusted range must still be rejected")
	}
}

func TestParsePrefixes(t *testing.T) {
	t.Parallel()

	if _, err := fetch.ParsePrefixes([]string{"not-a-cidr"}); err == nil {
		t.Fatal("expected error for invalid CIDR")
	}
	got, err := fetch.ParsePrefixes([]string{"", "198.18.0.0/16", "  "})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 parsed prefix, got %d", len(got))
	}
}
