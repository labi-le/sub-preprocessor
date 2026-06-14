package fetch

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

const userAgent = "mihomo-geofeed-preprocessor/0.1"

type FileType string

const (
	FileTypeRaw  FileType = "raw"
	FileTypeGzip FileType = "gzip"
)

func BytesWithType(ctx context.Context, rawURL string, limit int64, fileType FileType) ([]byte, error) {
	if err := ValidatePublicHTTPSURL(rawURL); err != nil {
		return nil, err
	}
	if err := ValidateFileType(fileType); err != nil {
		return nil, err
	}

	client := newSafeHTTPClient()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("bad status: %s", resp.Status)
	}

	reader, err := maybeDecode(rawURL, resp, fileType)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
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

func ValidatePublicHTTPSURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("only https URLs are allowed")
	}
	if u.Hostname() == "" {
		return fmt.Errorf("url host is required")
	}
	if u.User != nil {
		return fmt.Errorf("url userinfo is not allowed")
	}
	if addr, err := netip.ParseAddr(u.Hostname()); err == nil && !isPublicIP(addr) {
		return fmt.Errorf("non-public target is not allowed")
	}
	return nil
}

func newSafeHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	transport.Proxy = nil
	dialer := &net.Dialer{Timeout: 30 * time.Second}

	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}

		if ip, err := netip.ParseAddr(host); err == nil {
			if !isPublicIP(ip) {
				return nil, fmt.Errorf("non-public target is not allowed")
			}
			return dialer.DialContext(ctx, network, addr)
		}

		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}

		for _, ip := range ips {
			if !isPublicIP(ip) {
				continue
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		}

		return nil, fmt.Errorf("non-public target is not allowed")
	}

	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("stopped after 10 redirects")
			}
			return ValidatePublicHTTPSURL(req.URL.String())
		},
	}
}

func maybeDecode(rawURL string, resp *http.Response, fileType FileType) (io.ReadCloser, error) {
	if fileType == FileTypeRaw {
		return resp.Body, nil
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return nil, err
	}
	return &readCloser{Reader: zr, closeFn: zr.Close}, nil
}

func isPublicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return false
	}
	if ip.Is6() {
		if private6Prefix.Contains(ip) || linkLocal6Prefix.Contains(ip) {
			return false
		}
	}
	return true
}

var (
	private6Prefix   = netip.MustParsePrefix("fc00::/7")
	linkLocal6Prefix = netip.MustParsePrefix("fe80::/10")
)

type readCloser struct {
	io.Reader
	closeFn func() error
}

func (r *readCloser) Close() error {
	return r.closeFn()
}
