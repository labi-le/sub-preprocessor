package fetch

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// UserAgent is sent on every outbound fetch. Exported so sibling packages
// (classify) present the same identity a real worker fetch would.
const UserAgent = "mihomo-geofeed-preprocessor/0.1"

type FileType string

type SubscriptionURL string

const (
	FileTypeRaw  FileType = "raw"
	FileTypeGzip FileType = "gzip"
)

const (
	defaultHTTPTimeout = 30 * time.Second
	maxRedirects       = 10
	defaultDialTimeout = 30 * time.Second
	errNonPublicTarget = "non-public target is not allowed"
	errOnlyHTTPS       = "only https URLs are allowed"
	errURLHostRequired = "url host is required"
	errURLUserinfo     = "url userinfo is not allowed"
)

const (
	// maxWireExpansion bounds how far a compressed body may inflate relative
	// to the bytes actually received. The geo datasets are text and land near
	// 5:1; a decompression bomb is orders of magnitude past that.
	maxWireExpansion = 20
	// expansionFloor is the output size below which the ratio is not checked.
	// The flate reader pulls its input in fixed blocks, so the start of any
	// stream is legitimately far ahead of the wire bytes consumed so far.
	expansionFloor = 1 << 20
)

var (
	errStoppedRedirects = fmt.Errorf("stopped after %d redirects", maxRedirects)
	errCompressionBomb  = errors.New("compressed body expands beyond the allowed ratio")
)

// StatusError reports a non-2xx HTTP response. Typed so callers can branch on
// the status code with errors.As (dbip month fallback checks 404) instead of
// parsing error text.
type StatusError struct {
	Code int
}

func (e *StatusError) Error() string {
	return "bad status: " + strconv.Itoa(e.Code) + " " + http.StatusText(e.Code)
}

// sharedClient is reused across fetches: the safe client is stateless apart
// from its connection pool, and rebuilding a Transport per request churns
// sockets and TLS handshakes.
var sharedClient = NewSafeHTTPClient()

func BytesWithType(ctx context.Context, rawURL SubscriptionURL, limit int64, fileType FileType) ([]byte, error) {
	if err := ValidatePublicHTTPSURL(rawURL); err != nil {
		return nil, err
	}
	if err := ValidateFileType(fileType); err != nil {
		return nil, err
	}

	client := sharedClient

	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, string(rawURL), nil)
	if errReq != nil {
		return nil, fmt.Errorf("create request: %w", errReq)
	}
	req.Header.Set("User-Agent", UserAgent)

	resp, errResp := client.Do(req)
	if errResp != nil {
		return nil, fmt.Errorf("do request: %w", errResp)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &StatusError{Code: resp.StatusCode}
	}

	reader, errDecode := MaybeDecode(resp, fileType)
	if errDecode != nil {
		return nil, fmt.Errorf("decode response: %w", errDecode)
	}
	defer reader.Close()

	body, errRead := io.ReadAll(io.LimitReader(reader, limit+1))
	if errRead != nil {
		return nil, fmt.Errorf("read response: %w", errRead)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("response too large: over %d bytes", limit)
	}

	return body, nil
}

func ValidateFileType(fileType FileType) error {
	switch fileType {
	case FileTypeRaw, FileTypeGzip:
		return nil
	default:
		return fmt.Errorf("unsupported file type: %q", fileType)
	}
}

// parseHTTPSURL validates and parses a well-formed https URL with a host and no
// userinfo. It does not restrict the target IP.
func parseHTTPSURL(rawURL SubscriptionURL) (*url.URL, error) {
	u, errURL := url.Parse(string(rawURL))
	if errURL != nil {
		return nil, fmt.Errorf("invalid url: %w", errURL)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return nil, errors.New(errOnlyHTTPS)
	}
	if u.Hostname() == "" {
		return nil, errors.New(errURLHostRequired)
	}
	if u.User != nil {
		return nil, errors.New(errURLUserinfo)
	}
	return u, nil
}

// ValidateHTTPSURL checks the URL is well-formed https with a host and no
// userinfo. It does NOT restrict the target IP; that (SSRF) policy belongs to
// the HTTP client's dialer (guarded vs unrestricted).
func ValidateHTTPSURL(rawURL SubscriptionURL) error {
	_, err := parseHTTPSURL(rawURL)
	return err
}

// ValidatePublicHTTPSURL is ValidateHTTPSURL plus an SSRF guard: a literal-IP
// host in a non-public range is rejected. Domain hosts pass here and are
// re-checked against their resolved IPs at dial time by the guarded client.
func ValidatePublicHTTPSURL(rawURL SubscriptionURL) error {
	u, err := parseHTTPSURL(rawURL)
	if err != nil {
		return err
	}
	if addr, errAddr := netip.ParseAddr(u.Hostname()); errAddr == nil && !isPublicIP(addr) {
		return errors.New(errNonPublicTarget)
	}
	return nil
}

// NewSafeHTTPClient returns a client with the full SSRF guard: https-only, no
// proxy, and resolved non-public IPs refused at dial time. Used for anything
// fetching user- or content-supplied URLs (the / endpoint, subscriptions).
func NewSafeHTTPClient() *http.Client { return newHTTPClient(true) }

// NewUnrestrictedHTTPClient returns a client that still forbids proxies and
// non-https redirects, but does NOT restrict the target IP. Used by the crawler
// only: it fetches t.me through a local fake-ip tunnel and follows links from
// scraped pages, where the IP guard is intentionally off (blind, no reflection).
func NewUnrestrictedHTTPClient() *http.Client { return newHTTPClient(false) }

func newHTTPClient(guardIPs bool) *http.Client {
	dialer := &net.Dialer{Timeout: defaultDialTimeout}

	transport := &http.Transport{
		DisableCompression: true,
		Proxy:              nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if !guardIPs {
				return dialer.DialContext(ctx, network, addr)
			}
			host, port, errDial := net.SplitHostPort(addr)
			if errDial != nil {
				return nil, fmt.Errorf("split host port: %w", errDial)
			}
			if ip, errIP := netip.ParseAddr(host); errIP == nil {
				if !isPublicIP(ip) {
					return nil, errors.New(errNonPublicTarget)
				}
				return dialer.DialContext(ctx, network, addr)
			}
			ips, errIP := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if errIP != nil {
				return nil, fmt.Errorf("lookup net ip: %w", errIP)
			}
			for _, ip := range ips {
				if !isPublicIP(ip) {
					continue
				}
				return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			}
			return nil, errors.New(errNonPublicTarget)
		},
	}

	validate := ValidatePublicHTTPSURL
	if !guardIPs {
		validate = ValidateHTTPSURL
	}
	return &http.Client{
		Timeout:   defaultHTTPTimeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errStoppedRedirects
			}
			return validate(SubscriptionURL(req.URL.String()))
		},
	}
}

// MaybeDecode wraps the response body in the reader for fileType. A gzip
// stream is additionally guarded against decompression bombs: the caller's
// size limit bounds the inflated body, which a ~200 KB crafted archive would
// otherwise reach in full before anything noticed.
func MaybeDecode(resp *http.Response, fileType FileType) (io.ReadCloser, error) {
	if fileType == FileTypeRaw {
		return resp.Body, nil
	}
	wire := &wireCounter{r: resp.Body}
	zr, errZip := gzip.NewReader(wire)
	if errZip != nil {
		return nil, fmt.Errorf("gzip reader: %w", errZip)
	}
	return &expansionGuard{zr: zr, wire: wire}, nil
}

// wireCounter tallies the compressed bytes pulled off the network so the
// expansion guard can compare inflated output against real input.
type wireCounter struct {
	r io.Reader
	n int64
}

// Read must pass the underlying error through untouched: io.Copy compares it
// against io.EOF with ==, so wrapping would turn a normal end-of-stream into a
// copy failure. Same for the two methods on expansionGuard below.
func (w *wireCounter) Read(p []byte) (int, error) {
	n, err := w.r.Read(p)
	w.n += int64(n)
	return n, err //nolint:wrapcheck // io.Reader contract, see above
}

// expansionGuard fails a gzip stream as soon as its output runs past
// maxWireExpansion times the wire bytes consumed, so a bomb is cut off in the
// first few MiB rather than at the caller's (much larger) decompressed cap.
type expansionGuard struct {
	zr   *gzip.Reader
	wire *wireCounter
	out  int64
}

func (g *expansionGuard) Read(p []byte) (int, error) {
	n, err := g.zr.Read(p)
	g.out += int64(n)
	if g.out > expansionFloor && g.out > g.wire.n*maxWireExpansion {
		return n, errCompressionBomb
	}
	return n, err //nolint:wrapcheck // io.Reader contract, see wireCounter.Read
}

func (g *expansionGuard) Close() error { return g.zr.Close() } //nolint:wrapcheck // io.Closer contract

// reservedPrefixes are non-public ranges not covered by the netip.Addr
// classification methods, which only know ::1, fc00::/7, fe80::/10, ff00::/8
// and ::.
//
// The IPv6 half matters because Unmap normalises only the ::ffff:a.b.c.d form:
// every other IPv4-in-IPv6 encoding would otherwise read as a public address
// while a NAT64 gateway, 6to4 relay or Teredo tunnel on the host delivers the
// embedded IPv4 target. Blanket-rejecting the globally routed 6to4 prefix
// costs a legitimate 6to4-addressed host, which is the right default here.
var reservedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"), // CGN shared space
	netip.MustParsePrefix("192.0.0.0/24"),  // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),   // class E, incl. limited broadcast

	netip.MustParsePrefix("::/96"),          // deprecated IPv4-compatible
	netip.MustParsePrefix("64:ff9b::/96"),   // NAT64 well-known
	netip.MustParsePrefix("64:ff9b:1::/48"), // NAT64 local-use
	netip.MustParsePrefix("100::/64"),       // discard-only
	netip.MustParsePrefix("2001::/32"),      // Teredo
	netip.MustParsePrefix("2002::/16"),      // 6to4
	netip.MustParsePrefix("fec0::/10"),      // deprecated site-local
}

func isPublicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return false
	}
	for _, p := range reservedPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}
