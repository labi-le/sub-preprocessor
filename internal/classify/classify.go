// Package classify decides whether a URL serves a usable Mihomo-compatible
// subscription. Its verdict is meant to match what the stable worker sees, so
// it shares both halves of what the worker judges a node by: internal/
// subscription's normalizer/parser, and subscription.PlaceholderNode, which the
// worker's Merge applies to its own pool. It fetches through a caller-supplied
// client whose dialer owns the IP/SSRF policy.
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

// proxySchemes are the URI schemes a Mihomo-compatible subscription is built
// from. The node parser is deliberately scheme-generic, so a document that is
// not a subscription at all can still yield nodes: restricting the count to
// these keeps a "live" verdict tied to proxy links.
//
// mihomo also converts http/https/socks/socks5/socks5h links, and the node
// parser accepts the portful form of each — but those are exactly the links a
// documentation page, a panel's client-setup snippet or a plain HTML nav bar is
// full of, so counting them would make the pages this gate exists to reject
// read as live subscriptions. They stay out on purpose.
var proxySchemes = map[string]bool{
	"vless": true, "vmess": true, "ss": true, "ssr": true, "trojan": true,
	"tuic": true, "hysteria": true, "hysteria2": true, "hy2": true, "anytls": true,
	"mierus": true,
}

// Reason enumerates the verdicts Body can reach. It exists so a caller can say
// WHY a body is not a live subscription — "not a subscription at all" and "the
// origin says it expired" are the same `!Live()` to anything that only asks the
// predicate, and they call for opposite handling.
//
// It is derived from Nodes and Expired rather than stored: a Result cannot then
// carry a reason that contradicts the counts it was built from, and Result keeps
// its two fields (no per-node cost anywhere Body is called in a loop).
type Reason uint8

const (
	ReasonLive     Reason = iota // at least one usable proxy-scheme node and no advertised expiry
	ReasonExpired                // subscription-userinfo advertised an expiry already past
	ReasonNodeless               // 2xx body carrying no usable proxy-scheme node
)

func (r Reason) String() string {
	switch r {
	case ReasonLive:
		return "live"
	case ReasonExpired:
		return "expired"
	case ReasonNodeless:
		return "nodeless-2xx"
	}
	return "unknown"
}

// Result reports what a fetched body looks like.
type Result struct {
	Nodes   int  // usable proxy-scheme nodes after base64 normalization, placeholders excluded
	Expired bool // subscription-userinfo advertised an expiry in the past
}

// Live reports a usable subscription: at least one node that is not a
// placeholder, and not past expiry.
// Its negation is NOT a death verdict. A 2xx body with no usable node is what a
// captive portal, a CDN interstitial, a panel login page or a dead panel's
// placeholder body serves, so only Expired — an expiry the origin itself
// advertised — and a Gone status prove a URL stopped being a subscription;
// callers that delete on a dead verdict must not delete on a merely nodeless
// one. That the placeholder verdict is read out of node contents rather than
// stated by the origin is exactly why it must not authorize deletion.
func (r Result) Live() bool { return r.Reason() == ReasonLive }

// Reason reports the verdict behind Live. Expired outranks a zero node count:
// an origin-advertised expiry is a death verdict and a nodeless body is not, so
// a body that is both must never read as merely nodeless.
func (r Result) Reason() Reason {
	switch {
	case r.Expired:
		return ReasonExpired
	case r.Nodes <= 0:
		return ReasonNodeless
	default:
		return ReasonLive
	}
}

// Body classifies an already-fetched subscription body. subUserinfo is the raw
// `subscription-userinfo` response header (may be empty); now is the reference
// unix time for the expiry comparison.
func Body(body []byte, subUserinfo string, now int64) Result {
	var r Result
	if exp, ok := parseExpire(subUserinfo); ok && exp > 0 && exp < now {
		r.Expired = true
	}
	subscription.Parse(subscription.Normalize(body), func(n subscription.Node) bool {
		// Only real proxy schemes count. The parser rejects a portless
		// http/https/socks line, but a portful one (`https://example.com:8443/docs`
		// in a docs page, a panel's own admin URL) is still a valid node to it,
		// so the whitelist is what keeps such a page from reading as a live
		// subscription. Schemes are case-insensitive (RFC 3986), so lowercase
		// before the lookup.
		if n.Server != "" && proxySchemes[strings.ToLower(string(n.Scheme))] &&
			!subscription.PlaceholderNode(n.Raw, n.Server) {
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

// StatusError reports that the origin answered with a non-2xx status: the host
// is alive, but only a Gone code proves the URL is no longer a subscription.
// Any other status (like a transport error) leaves the verdict undetermined.
type StatusError struct {
	Code   int
	Status string
}

func (e *StatusError) Error() string { return "bad status: " + e.Status }

// goneCodes are the statuses that prove a URL stopped being a subscription: the
// origin answered, and its answer is a permanent "not here". Every other non-2xx
// says nothing about the subscription itself — 403 is what a WAF or geo-block
// returns to a non-browser client, 408/425/429 are back-pressure, 5xx is the
// origin failing — so callers that delete on a dead verdict must not delete on
// those.
var goneCodes = map[int]bool{
	http.StatusNotFound:                   true, // 404: no such subscription path
	http.StatusGone:                       true, // 410: explicit permanent removal
	http.StatusUnavailableForLegalReasons: true, // 451: withheld at this origin for good
}

// Gone reports whether the status is a definitive "no longer a subscription"
// verdict. False means transient: worth retrying and never grounds for deleting
// the source.
func (e *StatusError) Gone() bool { return e != nil && goneCodes[e.Code] }

// URL fetches rawURL with the supplied client and classifies the response; the
// client's dialer owns the IP/SSRF policy (guarded for the CLI, unrestricted for
// the crawler). A non-2xx status is returned as *StatusError, whose Gone method
// says whether that status is definitive; any other error means the verdict is
// undetermined. No hwid is sent: this is the entry for fetches that have no
// source config to read one from, the classify CLI included.
func URL(ctx context.Context, client *http.Client, rawURL fetch.SubscriptionURL) (Result, error) {
	return URLWithHWID(ctx, client, rawURL, "")
}

// URLWithHWID is URL with the source's hwid carried as the x-hwid request
// header, exactly as the worker's subscription fetch carries it
// (fetch.BytesWithTypeHWID). A Remnawave panel with the HWID device limit on
// answers a header-less fetch 200 with a single placeholder node, so judging
// such a source without its hwid reads live as nodeless and its owner would be
// retired while the worker keeps publishing real nodes from it. An empty hwid
// sends no header.
func URLWithHWID(ctx context.Context, client *http.Client, rawURL fetch.SubscriptionURL, hwid string) (Result, error) {
	if err := fetch.ValidateHTTPSURL(rawURL); err != nil {
		return Result{}, fmt.Errorf("validate url: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, string(rawURL), nil)
	if err != nil {
		return Result{}, fmt.Errorf("create request: %w", err)
	}
	// Mirror whatever identity a worker fetch presents, per-request rotation
	// included: the verdict must describe what the worker would see, and some
	// panels vary the response format by User-Agent prefix.
	req.Header.Set("User-Agent", fetch.UserAgent())
	if hwid != "" {
		// Header.Set canonicalises any casing, and HTTP/2 lowercases the name
		// on the wire regardless, so the canonical spelling is the whole of it.
		req.Header.Set("X-Hwid", hwid)
	}
	resp, err := client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Result{}, &StatusError{Code: resp.StatusCode, Status: resp.Status}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, subscription.MaxSubscriptionSize+1))
	if err != nil {
		return Result{}, fmt.Errorf("read body: %w", err)
	}
	if int64(len(body)) > subscription.MaxSubscriptionSize {
		return Result{}, fmt.Errorf("response too large: over %d bytes", subscription.MaxSubscriptionSize)
	}
	return Body(body, resp.Header.Get("Subscription-Userinfo"), time.Now().Unix()), nil
}
