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

const userAgent = "mihomo-geofeed-preprocessor/0.1"

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

func BytesWithType(ctx context.Context, rawURL SubscriptionURL, limit int64, fileType FileType) ([]byte, error) {
	if err := ValidatePublicHTTPSURL(rawURL); err != nil {
		return nil, err
	}
	if err := ValidateFileType(fileType); err != nil {
		return nil, err
	}

	client := NewSafeHTTPClient()

	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, string(rawURL), nil)
	if errReq != nil {
		return nil, fmt.Errorf("create request: %w", errReq)
	}
	req.Header.Set("User-Agent", userAgent)

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

func ValidatePublicHTTPSURL(rawURL SubscriptionURL) error {
	u, errURL := url.Parse(string(rawURL))
	if errURL != nil {
		return fmt.Errorf("invalid url: %w", errURL)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return errors.New(errOnlyHTTPS)
	}
	if u.Hostname() == "" {
		return errors.New(errURLHostRequired)
	}
	if u.User != nil {
		return errors.New(errURLUserinfo)
	}
	if addr, errAddr := netip.ParseAddr(u.Hostname()); errAddr == nil && !isPublicIP(addr) {
		return errors.New(errNonPublicTarget)
	}
	return nil
}

func NewSafeHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: defaultDialTimeout}

	transport := &http.Transport{
		DisableCompression: true,
		Proxy:              nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
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

	return &http.Client{
		Timeout:   defaultHTTPTimeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errStoppedRedirects
			}
			return ValidatePublicHTTPSURL(SubscriptionURL(req.URL.String()))
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

func isPublicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() {
		return false
	}
	return !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsMulticast() &&
		!ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsUnspecified()
}
