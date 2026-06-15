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
