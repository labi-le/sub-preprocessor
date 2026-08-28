package fetch

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

const hwidValue = "abcdef0123456789"

// hwidHeaderRecorder points sharedClient at a local listener while the fetched
// URL keeps a public-looking host, which BytesWithTypeHWID insists on. The swap
// is package-global, so no test using it may call t.Parallel.
func hwidHeaderRecorder(t *testing.T) *http.Header {
	t.Helper()

	seen := &http.Header{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = r.Header.Clone()
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)

	originalClient := sharedClient
	sharedClient = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, srv.Listener.Addr().String())
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // test server presents an httptest-internal cert
	}}
	t.Cleanup(func() { sharedClient = originalClient })

	return seen
}

func TestBytesWithTypeHWIDSendsTheHeaderVerbatim(t *testing.T) {
	seen := hwidHeaderRecorder(t)

	body, err := BytesWithTypeHWID(t.Context(), "https://example.com/sub", maxBodyBytes, FileTypeRaw, hwidValue)
	if err != nil {
		t.Fatalf("BytesWithTypeHWID: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("body = %q, want %q", body, "ok")
	}
	if got := seen.Get("X-Hwid"); got != hwidValue {
		t.Fatalf("X-Hwid = %q, want %q", got, hwidValue)
	}
}

func TestBytesWithTypeHWIDOmitsTheHeaderWhenEmpty(t *testing.T) {
	seen := hwidHeaderRecorder(t)

	if _, err := BytesWithTypeHWID(t.Context(), "https://example.com/sub", maxBodyBytes, FileTypeRaw, ""); err != nil {
		t.Fatalf("BytesWithTypeHWID: %v", err)
	}
	if _, ok := (*seen)["X-Hwid"]; ok {
		t.Fatalf("x-hwid present with an empty value; the panel must see no header at all")
	}
}

func TestBytesWithTypeSendsNoHWIDHeader(t *testing.T) {
	seen := hwidHeaderRecorder(t)

	if _, err := BytesWithType(t.Context(), "https://example.com/sub", maxBodyBytes, FileTypeRaw); err != nil {
		t.Fatalf("BytesWithType: %v", err)
	}
	if _, ok := (*seen)["X-Hwid"]; ok {
		t.Fatalf("x-hwid reached the wire from the plain entry point")
	}
}
