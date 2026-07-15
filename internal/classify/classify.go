// Package classify decides whether a URL serves a usable Mihomo-compatible
// subscription. It reuses the project's SSRF-safe HTTP client and the same
// body normalizer/parser the preprocessor uses, so a "live subscription"
// verdict here means the same thing the stable worker would see.
package classify

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"domains.lst/sub-preprocessor/internal/fetch"
	"domains.lst/sub-preprocessor/internal/subscription"
)

// maxSubscriptionSize mirrors the worker's cap (internal/subscription's
// unexported maxSubscriptionSize, 10 MiB) so a "live" verdict here matches
// what the worker would accept; keep the two values in sync.
const maxSubscriptionSize = 10 << 20

// proxySchemes are the URI schemes a Mihomo-compatible subscription is built
// from. Restricting to these rejects HTML pages whose http(s):// links the
// generic node parser would otherwise accept.
var proxySchemes = map[string]bool{
	"vless": true, "vmess": true, "ss": true, "ssr": true, "trojan": true,
	"tuic": true, "hysteria": true, "hysteria2": true, "hy2": true, "anytls": true,
}

// Result reports what a fetched body looks like.
type Result struct {
	Nodes   int  // parseable scheme:// nodes after base64 normalization
	Expired bool // subscription-userinfo advertised an expiry in the past
}

// Live reports a usable subscription: at least one node and not past expiry.
func (r Result) Live() bool { return r.Nodes > 0 && !r.Expired }

// Body classifies an already-fetched subscription body. subUserinfo is the raw
// `subscription-userinfo` response header (may be empty); now is the reference
// unix time for the expiry comparison.
func Body(body []byte, subUserinfo string, now int64) Result {
	var r Result
	if exp, ok := parseExpire(subUserinfo); ok && exp > 0 && exp < now {
		r.Expired = true
	}
	subscription.Parse(subscription.Normalize(body), func(n subscription.Node) bool {
		// Only real proxy schemes count. parseNode is deliberately generic and
		// even defaults a missing port to 443, so an HTML page full of
		// https:// links would otherwise look like a subscription. Schemes are
		// case-insensitive (RFC 3986), so lowercase before the lookup.
		if n.Server != "" && proxySchemes[strings.ToLower(string(n.Scheme))] {
			r.Nodes++
		}
		return true
	})
	return r
}

// parseExpire extracts expire=<unix> from a subscription-userinfo header value
// such as "upload=0; download=0; total=0; expire=1786085295".
func parseExpire(h string) (int64, bool) {
	for part := range strings.SplitSeq(h, ";") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(part), "expire="); ok {
			v = strings.TrimSpace(v)
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				return n, true
			}
			// Some panels emit a float ("expire=1786085295.0"); truncate.
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return int64(f), true
			}
			return 0, false
		}
	}
	return 0, false
}

// StatusError reports that the origin answered with a definitive non-2xx
// status: the host is alive and the URL is NOT a subscription (as opposed to
// transport errors, whose verdict is unknown).
type StatusError struct {
	Code   int
	Status string
}

func (e *StatusError) Error() string { return "bad status: " + e.Status }

// URL fetches rawURL with the SSRF-safe client and classifies the response.
// A non-2xx status is returned as *StatusError (definitively not a
// subscription); any other error means the verdict is undetermined.
func URL(ctx context.Context, client *http.Client, rawURL fetch.SubscriptionURL) (Result, error) {
	if err := fetch.ValidatePublicHTTPSURL(rawURL); err != nil {
		return Result{}, fmt.Errorf("validate url: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, string(rawURL), nil)
	if err != nil {
		return Result{}, fmt.Errorf("create request: %w", err)
	}
	// Present the same identity a worker fetch would, so the verdict matches
	// what the worker sees (some panels vary the response by User-Agent).
	req.Header.Set("User-Agent", fetch.UserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Result{}, &StatusError{Code: resp.StatusCode, Status: resp.Status}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSubscriptionSize+1))
	if err != nil {
		return Result{}, fmt.Errorf("read response: %w", err)
	}
	if int64(len(body)) > maxSubscriptionSize {
		return Result{}, fmt.Errorf("response too large: over %d bytes", maxSubscriptionSize)
	}
	return Body(body, resp.Header.Get("Subscription-Userinfo"), time.Now().Unix()), nil
}
