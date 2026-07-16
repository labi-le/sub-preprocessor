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

var errStoppedRedirects = fmt.Errorf("stopped after %d redirects", maxRedirects)

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
		return nil, fmt.Errorf("bad status: %s", resp.Status)
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

func MaybeDecode(resp *http.Response, fileType FileType) (io.ReadCloser, error) {
	if fileType == FileTypeRaw {
		return resp.Body, nil
	}
	zr, errZip := gzip.NewReader(resp.Body)
	if errZip != nil {
		return nil, fmt.Errorf("gzip reader: %w", errZip)
	}
	return zr, nil
}

// reservedPrefixes are non-public ranges not covered by the netip.Addr
// classification methods: CGN shared space, IETF protocol assignments,
// benchmarking, and class E (incl. limited broadcast).
var reservedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
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
