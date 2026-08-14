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
	// errNonCanonicalIPHost covers an IPv4 literal written the way inet_aton
	// accepts and netip.ParseAddr does not, which would otherwise pass the IP
	// gate as a domain name.
	errNonCanonicalIPHost = "non-canonical ip literal host is not allowed"
)

const (
	maxIPv4Parts   = 4
	baseOctal      = 8
	baseDecimal    = 10
	baseHex        = 16
	firstHexLetter = 10
)

const (
	// maxWireExpansion bounds how far a compressed body may inflate relative
	// to the bytes actually received.
	//
	// Measured, not guessed: the configured geofeed source (GeoFeed-Harvester's
	// geofeed.csv.gz) is 4.47 MB on the wire and 79.16 MB inflated -- 17.70:1,
	// because every one of its 519k rows repeats the same registrar URLs and
	// timestamps. An earlier 20:1 guard left that 13% of headroom, and the cost
	// of crossing it is not a rejected download: LoadAll counts the failure as
	// one skipped source and returns the OTHER source's 310 lines with a nil
	// error, so a restart would come up serving ~300 country ranges instead of
	// 519k and geo-drop nearly every node. 100:1 keeps the guard useful --
	// a real bomb is 400:1 and up, and the all-zero fixture in
	// TestMaybeDecodeGzipBombIsCutOff measures ~1000:1 -- with room for the
	// feed to grow more repetitive, which it does as it adds rows.
	maxWireExpansion = 100
	// expansionFloor is the output size below which the ratio is not checked.
	// The flate reader pulls its input in fixed blocks, so the start of any
	// stream is legitimately far ahead of the wire bytes consumed so far.
	expansionFloor = 1 << 20
	// maxEagerBody caps what an announced Content-Length may allocate before a
	// single byte has arrived. On GET / the URL is user input, so the
	// announcement is hostile input: sized straight off it, a peer that
	// announces the cap and then sends nothing costs limit bytes per request.
	// 256 KiB is the first power of two past the corpus p90 (198272 B) and
	// covers 133 of the 145 configured sources that announce a length; the 12
	// above it reach their size in at most four growth steps. The vassago
	// instance's 52 answering sources are a different distribution — measured
	// 2026-08-14, median 283358 B, p90 1794886 B, 24 of 52 under this ceiling —
	// and it stays one constant anyway: the ceiling bounds a HOSTILE
	// announcement on GET /, which both instances serve, and a configured
	// source above it pays only a bounded doubling chain per cycle.
	maxEagerBody = 256 << 10
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

	hint := int64(0)
	if fileType == FileTypeRaw {
		hint = resp.ContentLength
	}
	body, errRead := readBody(reader, limit, hint)
	if errRead != nil {
		return nil, fmt.Errorf("read response: %w", errRead)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("response too large: over %d bytes", limit)
	}

	return body, nil
}

// readBody reads r under limit, one byte past it so the caller can tell a body
// that overruns the cap from one that just fills it.
//
// hint is the announced body length, 0 or negative when the response did not
// state one. Announced and at or under maxEagerBody it buys ONE exact
// allocation where io.ReadAll starts at 512 bytes and grows geometrically,
// spending ~2.3x the body across ~30 allocations on a 3 MB one; a larger
// announcement is worth less, since the doubling below spends a chain of its
// own. DisableCompression is what makes the header usable at all — a raw fetch
// gets identity encoding, so ContentLength counts exactly the bytes read;
// measured over the configured sources, 145 of the 147 answering 200 state one
// and none of them disagreed with its own body; the 2 that state nothing frame
// the response with HTTP/2 DATA frames instead.
//
// The header is only a claim, and on GET / it is a claim by whoever chose
// subscription_url, so it is trusted for maxEagerBody up front and past that
// only as far as the peer keeps delivering: the buffer never exceeds the ceiling
// or twice what has arrived, whichever is larger. A body may also run past its
// announcement, so the buffer keeps one spare byte to notice and grows for the
// remainder rather than truncating. Only limit bounds what is read.
func readBody(r io.Reader, limit, hint int64) ([]byte, error) {
	if hint <= 0 || hint > limit {
		return io.ReadAll(io.LimitReader(r, limit+1)) //nolint:wrapcheck // caller wraps
	}

	buf := make([]byte, min(hint, maxEagerBody)+1)
	n := int64(0)
	for {
		got, err := r.Read(buf[n:])
		n += int64(got)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return buf[:n], nil
			}
			return nil, err //nolint:wrapcheck // caller wraps
		}
		if n < int64(len(buf)) {
			continue
		}
		if n > limit {
			return buf[:n], nil
		}
		buf = growBody(buf, hint, limit)
	}
}

// growBody doubles buf, stopping at hint+1 so a peer that delivered everything
// it announced ends up with an exactly sized buffer, and at limit+1 so the read
// never runs past the cap plus its detection byte.
//
// Growth copies, so old and new are live at once, and only the LAST allocation
// may exceed half of limit+1: an announcement just under the cap that then
// overruns otherwise lands a buffer next to the ceiling and peaks at ~2x it —
// measured 20.98 MB under a 10 MiB cap against 15.74 MB with this clamp, where
// io.ReadAll (go1.26.5, still the unannounced path) peaks at 33.97 MB building
// its chunk list and copying it into a right-sized slice. The exact hint
// landing survives below that half, which every configured source body of both
// instances is (largest 4.31 MB), and no step is more than twice what has
// arrived: half is only reachable from a buffer already a quarter of the
// ceiling.
func growBody(buf []byte, hint, limit int64) []byte {
	// Every step but the last is clamped to this share of the ceiling, which is
	// what bounds the peak at 1.5x rather than 2x: the pair that is briefly live
	// is one whole buffer plus one at most half that size.
	const nonFinalStepDivisor = 2

	ceiling := limit + 1
	half := (ceiling + 1) / nonFinalStepDivisor
	size := int64(len(buf))
	next := min(size+size, ceiling)
	switch {
	case hint+1 <= half && size < hint+1:
		next = min(next, hint+1)
	case next < ceiling:
		next = min(next, half)
	}
	out := make([]byte, next)
	copy(out, buf)
	return out
}

func ValidateFileType(fileType FileType) error {
	switch fileType {
	case FileTypeRaw, FileTypeGzip:
		return nil
	default:
		return fmt.Errorf("unsupported file type: %q", fileType)
	}
}

// checkHTTPSURL is parseHTTPSURL's verdict on an already-parsed URL. The three
// rejections and their order are the only copy of them.
func checkHTTPSURL(u *url.URL) error {
	if !strings.EqualFold(u.Scheme, "https") {
		return errors.New(errOnlyHTTPS)
	}
	if u.Hostname() == "" {
		return errors.New(errURLHostRequired)
	}
	if u.User != nil {
		return errors.New(errURLUserinfo)
	}
	return nil
}

// parseHTTPSURL validates and parses a well-formed https URL with a host and no
// userinfo. It does not restrict the target IP.
func parseHTTPSURL(rawURL SubscriptionURL) (*url.URL, error) {
	u, errURL := url.Parse(string(rawURL))
	if errURL != nil {
		return nil, fmt.Errorf("invalid url: %w", errURL)
	}
	if err := checkHTTPSURL(u); err != nil {
		return nil, err
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
	u, err := url.Parse(string(rawURL))
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	return ValidatePublicParsedHTTPSURL(u)
}

// ValidatePublicParsedHTTPSURL is ValidatePublicHTTPSURL for a caller holding
// the parsed URL, so the string is not parsed a second time. Same gates, same
// order, same error values; the one verdict it cannot return is the parse
// failure, which belongs to whoever parsed.
func ValidatePublicParsedHTTPSURL(u *url.URL) error {
	if err := checkHTTPSURL(u); err != nil {
		return err
	}
	host := u.Hostname()
	if addr, errAddr := netip.ParseAddr(host); errAddr == nil {
		if !isPublicIP(addr) {
			return errors.New(errNonPublicTarget)
		}
		return nil
	}
	// netip.ParseAddr answers only for the canonical forms, so 2130706433,
	// 0x7f000001, 127.1 and 0177.0.0.1 reach here as if they were domain names —
	// and getaddrinfo's inet_aton resolves every one of them to 127.0.0.1
	// (measured under CGO_ENABLED=1; the pure-Go resolver says "no such host",
	// so today only the build flag stands between this gate and a loopback
	// fetch). The crawler's client has no dial-time guard, so this gate is the
	// only one a channel-supplied URL passes.
	if numericIPHost(host) {
		return errors.New(errNonCanonicalIPHost)
	}
	return nil
}

// numericIPHost reports whether host is an IPv4 address written in one of the
// forms inet_aton accepts and netip.ParseAddr does not. A canonical literal is
// not this function's business: netip.ParseAddr has already claimed it.
func numericIPHost(host string) bool {
	for parts := 1; ; parts++ {
		dot := strings.IndexByte(host, '.')
		part := host
		if dot >= 0 {
			part = host[:dot]
		}
		if parts > maxIPv4Parts || !inetAtonPart(part) {
			return false
		}
		if dot < 0 {
			return true
		}
		host = host[dot+1:]
	}
}

// inetAtonPart reports whether s is one of inet_aton's number forms: 0x-prefixed
// hex, 0-prefixed octal, or decimal. The value range is deliberately unchecked —
// a part out of range makes inet_aton fail, so the host is then a name, and
// refusing it costs a garbage hostname nobody serves subscriptions from.
func inetAtonPart(s string) bool {
	if s == "" {
		return false
	}
	digits, base := s, baseDecimal
	if s[0] == '0' {
		if len(s) > 1 && (s[1] == 'x' || s[1] == 'X') {
			digits, base = s[2:], baseHex
			if digits == "" {
				return false
			}
		} else {
			digits, base = s[1:], baseOctal
		}
	}
	for i := range len(digits) {
		if hexDigitValue(digits[i]) >= base {
			return false
		}
	}
	return true
}

// hexDigitValue returns c's value as a hex digit, or baseHex when c is not one,
// which is out of range for every base and so rejects.
func hexDigitValue(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + firstHexLetter
	case c >= 'A' && c <= 'F':
		return int(c-'A') + firstHexLetter
	}
	return baseHex
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
