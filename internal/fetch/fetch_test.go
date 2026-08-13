package fetch_test

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// TestMaybeDecodeAcceptsRealGeofeedRatio pins the guard against the workload it
// actually runs on. The configured geofeed source measures 17.70:1 (4.47 MB on
// the wire, 79.16 MB inflated, 519k rows that repeat the same registrar URLs
// and timestamps), so a ratio anywhere near it is a live hazard: LoadAll treats
// a guard trip as one skipped source and returns the other source's few hundred
// lines with a NIL error, so the service would restart into a country database
// of ~300 ranges and geo-drop nearly every node. This fixture reproduces that
// shape -- highly repetitive CSV, not random bytes -- and must pass untouched.
func TestMaybeDecodeAcceptsRealGeofeedRatio(t *testing.T) {
	t.Parallel()

	var raw bytes.Buffer
	for i := 0; raw.Len() < 16<<20; i++ {
		fmt.Fprintf(&raw, "203.0.%d.%d/24,DE,DE-BE,Berlin,10115,ripe,https://rdap.db.ripe.net/ip/203.0.113.0,2026-07-28T00:00:00Z,true,true,true,0.95,\n",
			(i/256)%256, i%256)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	ratio := float64(raw.Len()) / float64(gz.Len())
	if ratio < 15 {
		t.Fatalf("fixture must be at least as compressible as the real feed, got %.1f:1", ratio)
	}

	resp := &http.Response{Body: io.NopCloser(bytes.NewReader(gz.Bytes()))}
	rc, err := fetch.MaybeDecode(resp, fetch.FileTypeGzip)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	n, err := io.Copy(io.Discard, rc)
	if err != nil {
		t.Fatalf("a %.1f:1 geofeed must not trip the bomb guard: %v (inflated %d of %d)", ratio, err, n, raw.Len())
	}
	if n != int64(raw.Len()) {
		t.Fatalf("inflated %d bytes, want the whole %d", n, raw.Len())
	}
}

// TestMaybeDecodeGzipBombIsCutOff: the caller's size limit bounds the INFLATED
// body, so a few KB of crafted gzip is otherwise free to allocate the whole
// 256 MiB geo cap (three of those load concurrently). The guard compares
// output against the wire bytes actually consumed and must fail the read long
// before the archive finishes inflating — while leaving a normal body alone.
func TestMaybeDecodeGzipBombIsCutOff(t *testing.T) {
	t.Parallel()

	const inflated = 32 << 20
	var bomb bytes.Buffer
	zw := gzip.NewWriter(&bomb)
	zeros := make([]byte, 1<<16)
	for written := 0; written < inflated; written += len(zeros) {
		if _, err := zw.Write(zeros); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if bomb.Len() > inflated/100 {
		t.Fatalf("test fixture is not a bomb: %d wire bytes for %d inflated", bomb.Len(), inflated)
	}

	resp := &http.Response{Body: io.NopCloser(bytes.NewReader(bomb.Bytes()))}
	rc, err := fetch.MaybeDecode(resp, fetch.FileTypeGzip)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	n, err := io.Copy(io.Discard, rc)
	if err == nil {
		t.Fatalf("gzip bomb must fail the read; inflated %d bytes with no error", n)
	}
	if n >= inflated/4 {
		t.Fatalf("bomb must be cut off early: inflated %d of %d bytes", n, inflated)
	}

	// Incompressible data crosses the same size floor without approaching the
	// ratio, so the guard must let it through untouched.
	plain := make([]byte, 2<<20)
	state := uint32(1)
	for i := range plain {
		state = state*1664525 + 1013904223
		plain[i] = byte(state >> 24)
	}
	var normal bytes.Buffer
	zw = gzip.NewWriter(&normal)
	if _, err = zw.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err = zw.Close(); err != nil {
		t.Fatal(err)
	}

	resp = &http.Response{Body: io.NopCloser(bytes.NewReader(normal.Bytes()))}
	rc, err = fetch.MaybeDecode(resp, fetch.FileTypeGzip)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("normal gzip body must decode: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("normal gzip body corrupted: got %d bytes, want %d", len(got), len(plain))
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

// The three corpora below are shared by every validator test here, so a case
// added for one is checked by all of them.
var (
	// validateNonPublicCorpus is the SSRF reject set. The first group is
	// canonical literals netip.ParseAddr answers for; the second is the same
	// targets in the encodings it does NOT answer for, which inet_aton does
	// (measured: getaddrinfo resolves all four to 127.0.0.1 under
	// CGO_ENABLED=1, where the pure-Go resolver reports no such host).
	validateNonPublicCorpus = []string{
		"https://10.0.0.1/sub",     // private
		"https://127.0.0.1/sub",    // loopback
		"https://198.18.1.15/sub",  // reserved (mihomo fake-ip range)
		"https://169.254.169.254/", // link-local (cloud metadata)
		// IPv4-in-IPv6 encodings Unmap does not normalise, in order: NAT64
		// well-known, NAT64 local-use, 6to4, Teredo, deprecated
		// IPv4-compatible. Each embeds 127.0.0.1 and reaches it wherever the
		// matching gateway or tunnel exists. Then three non-global IPv6
		// ranges: discard-only, deprecated site-local, unique local.
		"https://[64:ff9b::7f00:1]/",
		"https://[64:ff9b:1::7f00:1]/",
		"https://[2002:7f00:1::]/",
		"https://[2001:0:1::1]/",
		"https://[::7f00:1]/",
		"https://[100::1]/",
		"https://[fec0::1]/",
		"https://[fd00::1]/",
		// 127.0.0.1 as one 32-bit decimal, as hex, as the two-part short form,
		// and with an octal first part; then 192.168.0.1 all-octal, which is here
		// because the reach is not only loopback and because the docs name it.
		"https://2130706433/sub",
		"https://0x7f000001/sub",
		"https://127.1/sub",
		"https://0177.0.0.1/sub",
		"https://0300.0250.0.1/sub",
		// A public address in the same non-canonical forms is refused too: the
		// gate answers the question it can answer (this is not a canonical
		// literal) rather than re-implementing inet_aton's arithmetic.
		"https://16843009/sub",
		"https://0x01010101/sub",
	}
	// validateAcceptCorpus passes every gate, and the numeric-looking hostnames
	// are the point of the last three: a host is only refused when inet_aton
	// would read the WHOLE of it as a number.
	validateAcceptCorpus = []string{
		"https://1.1.1.1/sub",
		"https://[2606:4700::1111]/sub",
		"https://sub.example.com/api/v1/client/subscribe?token=abc",
		"https://12345.example.com/sub",
		"https://cafe.beef/sub",
		"https://0x7f000001.example.com/sub",
	}
	// validateMalformedCorpus fails a gate other than the IP one: scheme, host,
	// userinfo, and a control byte url.Parse itself refuses.
	validateMalformedCorpus = []string{
		"http://example.com/test",
		"HTTPS://example.com/ok",
		"https://user@example.com/",
		"https://user:pw@example.com/",
		"https:///sub",
		"/relative/only",
		"https://ex\x7fample.com/sub",
	}
)

func TestValidatePublicHTTPSURLRejectsNonPublicIP(t *testing.T) {
	t.Parallel()

	for _, u := range validateNonPublicCorpus {
		if err := fetch.ValidatePublicHTTPSURL(fetch.SubscriptionURL(u)); err == nil {
			t.Errorf("%s: non-public or non-canonical IP host must be rejected", u)
		}
	}
	for _, u := range validateAcceptCorpus {
		if err := fetch.ValidatePublicHTTPSURL(fetch.SubscriptionURL(u)); err != nil {
			t.Errorf("%s: should pass, got: %v", u, err)
		}
	}
}

// TestValidatePublicParsedHTTPSURLMatchesTheStringForm is the differential test
// the parsed entry point needs: crawl.candidate reaches the SSRF gate through it
// instead of through the string form, so a verdict that moved between the two
// would move a gate that stands in front of content-supplied URLs. The error
// TEXT is compared, not the pass/fail bit, because the reject line embeds it.
func TestValidatePublicParsedHTTPSURLMatchesTheStringForm(t *testing.T) {
	t.Parallel()

	corpus := make([]string, 0,
		len(validateNonPublicCorpus)+len(validateAcceptCorpus)+len(validateMalformedCorpus))
	corpus = append(corpus, validateNonPublicCorpus...)
	corpus = append(corpus, validateAcceptCorpus...)
	corpus = append(corpus, validateMalformedCorpus...)

	for _, raw := range corpus {
		fromString := fetch.ValidatePublicHTTPSURL(fetch.SubscriptionURL(raw))
		u, parseErr := url.Parse(raw)
		if parseErr != nil {
			// url.Parse owns this verdict: the parsed form is unreachable for
			// input that does not parse, which is why candidate keeps its own
			// parse error.
			if fromString == nil {
				t.Errorf("%q: url.Parse rejected it but the string form accepted it", raw)
			}
			continue
		}
		if got, want := errText(fetch.ValidatePublicParsedHTTPSURL(u)), errText(fromString); got != want {
			t.Errorf("%q: parsed form = %q, string form = %q", raw, got, want)
		}
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
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
