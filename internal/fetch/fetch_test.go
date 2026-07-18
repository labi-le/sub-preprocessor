package fetch_test

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

// TestStatusErrorMessageAndAs guards the typed non-2xx error: callers branch on
// the code via errors.As (dbip month fallback checks 404), and the message must
// keep the historical "bad status: ..." text.
func TestStatusErrorMessageAndAs(t *testing.T) {
	t.Parallel()

	var err error = &fetch.StatusError{Code: http.StatusNotFound}
	if got, want := err.Error(), "bad status: 404 Not Found"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}

	wrapped := fmt.Errorf("do request: %w", err)
	var statusErr *fetch.StatusError
	if !errors.As(wrapped, &statusErr) {
		t.Fatal("errors.As must find *fetch.StatusError through wrapping")
	}
	if statusErr.Code != http.StatusNotFound {
		t.Fatalf("Code = %d, want 404", statusErr.Code)
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

func TestValidateHTTPSURLAllowsAnyIP(t *testing.T) {
	t.Parallel()

	// ValidateHTTPSURL is scheme-only: a private/literal IP host passes (the
	// IP/SSRF policy lives in the client's dialer, not this validator).
	if err := fetch.ValidateHTTPSURL(fetch.SubscriptionURL("https://10.0.0.1/sub")); err != nil {
		t.Fatalf("private IP host should pass scheme-only validation, got: %v", err)
	}
	if err := fetch.ValidateHTTPSURL(fetch.SubscriptionURL("http://example.com/")); err == nil {
		t.Fatal("http scheme must be rejected")
	}
	if err := fetch.ValidateHTTPSURL(fetch.SubscriptionURL("https://user@example.com/")); err == nil {
		t.Fatal("userinfo must be rejected")
	}
}

func TestValidatePublicHTTPSURLRejectsNonPublicIP(t *testing.T) {
	t.Parallel()

	for _, u := range []string{
		"https://10.0.0.1/sub",     // private
		"https://127.0.0.1/sub",    // loopback
		"https://198.18.1.15/sub",  // reserved (mihomo fake-ip range)
		"https://169.254.169.254/", // link-local (cloud metadata)
	} {
		if err := fetch.ValidatePublicHTTPSURL(fetch.SubscriptionURL(u)); err == nil {
			t.Errorf("%s: non-public IP host must be rejected", u)
		}
	}
	if err := fetch.ValidatePublicHTTPSURL(fetch.SubscriptionURL("https://1.1.1.1/sub")); err != nil {
		t.Fatalf("public IP host should pass, got: %v", err)
	}
}

func TestNewUnrestrictedHTTPClientDisablesProxy(t *testing.T) {
	t.Parallel()

	client := fetch.NewUnrestrictedHTTPClient()
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("unexpected transport type: %T", client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("expected proxy to be disabled even on the unrestricted client")
	}
}

func TestGuardBlocksLoopbackUnrestrictedAllows(t *testing.T) {
	t.Parallel()

	// httptest binds 127.0.0.1 — a non-public (loopback) target.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// The guarded client refuses to dial the loopback address.
	if _, err := fetch.NewSafeHTTPClient().Get(srv.URL); err == nil {
		t.Fatal("safe client must refuse to dial a loopback (non-public) target")
	}
	// The unrestricted client reaches it.
	resp, err := fetch.NewUnrestrictedHTTPClient().Get(srv.URL)
	if err != nil {
		t.Fatalf("unrestricted client should reach loopback, got: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}
