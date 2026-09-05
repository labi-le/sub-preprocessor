package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"domains.lst/sub-preprocessor/internal/fetch"
	"domains.lst/sub-preprocessor/internal/filter"
	"domains.lst/sub-preprocessor/internal/preprocess"
	"domains.lst/sub-preprocessor/internal/stable"
	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog"
)

type Filterer interface {
	Filter(ctx context.Context, b *bytes.Buffer, req preprocess.FilterRequest) (preprocess.Stats, error)
}

type Server struct {
	listen string
	app    *fiber.App
	logger zerolog.Logger
}

const defaultBuilderCapacity = 4096

// readTimeout bounds reading the full request (slowloris hardening); handler
// execution and the response write are not covered by it.
const readTimeout = 30 * time.Second

// indexRequestTimeout bounds the total work a single GET / request may cause.
// fasthttp's RequestCtx reports no deadline and its Done channel closes only on
// server shutdown, so nothing else stops the pipeline once the client walks
// away. Node resolution is serial with a 5s per-hostname DNS timeout, so an
// abandoned request would otherwise keep resolving hosts for hours. 60s is
// orders of magnitude above a warm request (milliseconds) and still admits a
// cold subscription of a few thousand hostnames.
const indexRequestTimeout = 60 * time.Second

// redactedURLDigestBytes is how much of the subscription URL digest goes into
// the log line: 12 hex chars are enough to tell subscriptions apart while
// staying short enough to read.
const redactedURLDigestBytes = 6

func New(logger zerolog.Logger, listen string, holder *Holder, stableHolder *stable.Holder) *Server {
	errorHandler := func(c *fiber.Ctx, err error) error {
		code := fiber.StatusInternalServerError
		if fiberErr, ok := errors.AsType[*fiber.Error](err); ok {
			code = fiberErr.Code
		}

		c.Set(fiber.HeaderContentType, fiber.MIMETextPlainCharsetUTF8)
		return c.Status(code).SendString(err.Error())
	}

	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
		DisableKeepalive:      true,
		ReadTimeout:           readTimeout,
		ErrorHandler:          errorHandler,
	})

	// Request logging middleware.
	app.Use(func(c *fiber.Ctx) error {
		start := time.Now()
		subscriptionURL := strings.TrimSpace(c.Query("subscription_url"))
		rawCountries := strings.TrimSpace(c.Query("countries"))

		err := c.Next()
		if err != nil {
			if handleErr := errorHandler(c, err); handleErr != nil {
				return handleErr
			}
		}

		latency := time.Since(start)
		status := c.Response().StatusCode()
		respSize := len(c.Response().Body())

		logger.Info().
			Str("method", c.Method()).
			Str("path", c.Path()).
			Str("remote", c.IP()).
			Str("subscription_url", redactSubscriptionURL(subscriptionURL)).
			Str("countries", rawCountries).
			Int("status", status).
			Int("size", respSize).
			Dur("latency", latency).
			Msg("")

		if err != nil && status >= fiber.StatusInternalServerError {
			logger.Error().Err(err).Int("status", status).Msg("request error")
		}
		return nil
	})

	app.Use(newRecoveryMiddleware(logger))

	app.Get("/healthz", func(c *fiber.Ctx) error {
		return c.SendString("ok")
	})

	app.Get("/favicon.ico", func(c *fiber.Ctx) error {
		return c.SendStatus(fiber.StatusNoContent)
	})

	app.Get("/", newIndexHandler(holder))

	app.Get("/stable.txt", newStableHandler(stableHolder))

	return &Server{listen: listen, app: app, logger: logger}
}

// newRecoveryMiddleware turns a handler panic into a 500. Neither fiber nor
// fasthttp recovers one, so a panic would otherwise kill the process and with
// it the in-memory stable list (/stable.txt then 503s until the worker
// republishes) — and the `/` pipeline runs hand-rolled index arithmetic over
// untrusted bytes, so that is a live risk. It is registered inside the logging
// middleware so a recovered panic still produces an access-log line, and the
// panic value and stack go to the log only, never to the client.
func newRecoveryMiddleware(logger zerolog.Logger) fiber.Handler {
	return func(c *fiber.Ctx) (err error) {
		defer func() {
			if r := recover(); r != nil {
				logger.Error().
					Str("panic", fmt.Sprint(r)).
					Str("path", c.Path()).
					Str("stack", string(debug.Stack())).
					Msg("recovered panic in handler")
				err = fiber.NewError(fiber.StatusInternalServerError, "internal server error")
			}
		}()
		return c.Next()
	}
}

// stableRetryAfter (seconds) is the Retry-After hint on the warm-up 503, before
// the worker publishes its first list. Short on purpose: the first list lands in
// minutes, not the inter-cycle interval.
const stableRetryAfter = "30"

// stableStatsMemo caches one snapshot's X-Stable-Stats rendering: the inputs
// (UpdatedAt, Stats) are immutable between cycles, so re-formatting per
// request is pure waste; recompute only when the holder publishes a new
// snapshot pointer. atomic.Pointer keeps racing first requests race-free —
// redundant computes store equal entries, and a stale entry costs a later
// recompute, never a wrong header, since only the entry matching the snapshot
// loaded in the same request is served.
type stableStatsMemo struct {
	snap *stable.Snapshot
	hdr  string
}

func newStableHandler(holder *stable.Holder) fiber.Handler {
	var memo atomic.Pointer[stableStatsMemo]
	return func(c *fiber.Ctx) error {
		snap := holder.Load()
		if snap == nil || len(snap.Payload) == 0 {
			c.Set("Retry-After", stableRetryAfter)
			return fiber.NewError(fiber.StatusServiceUnavailable, "stable list not ready")
		}

		var hdr string
		if m := memo.Load(); m != nil && m.snap == snap {
			hdr = m.hdr
		} else {
			hdr = fmt.Sprintf(
				"updated=%s sources=%d/%d merged=%d tested=%d kept=%d",
				snap.UpdatedAt.Format(time.RFC3339),
				snap.Stats.SourcesOK, snap.Stats.SourcesTotal,
				snap.Stats.Merged, snap.Stats.Tested, snap.Stats.Kept,
			)
			memo.Store(&stableStatsMemo{snap: snap, hdr: hdr})
		}

		c.Set(fiber.HeaderContentType, fiber.MIMETextPlainCharsetUTF8)
		c.Set("X-Stable-Stats", hdr)
		return c.Send(snap.Payload)
	}
}

// indexQueryKeys are the query parameters GET / reads; each is single-valued.
// fiber answers the FIRST value of a repeated key (fasthttp's Peek scans to
// the first match), so a duplicate would be silently dropped — a second
// exclude_countries= could serve exactly the jurisdictions the caller asked to
// exclude, a second countries= would silently narrow the allow-list. The scan
// stops at the first duplicate.
var indexQueryKeys = [...]string{
	"subscription_url",
	"countries",
	"groups",
	"exclude_countries",
	"exclude_groups",
}

func repeatedIndexQueryParam(c *fiber.Ctx) string {
	dup := -1
	var seen [len(indexQueryKeys)]bool
	c.Request().URI().QueryArgs().VisitAll(func(k, _ []byte) {
		if dup >= 0 {
			return
		}
		for i, key := range indexQueryKeys {
			if string(k) == key {
				if seen[i] {
					dup = i
					return
				}
				seen[i] = true
			}
		}
	})
	if dup < 0 {
		return ""
	}
	return indexQueryKeys[dup]
}

// countryTokenPresent reports whether raw holds at least one non-blank
// comma-separated token — the same tokenization buildCountrySet applies below.
// The parameter gate counts only such values, because a value made of blanks
// (",," or " , ") carries no country: treating it as present would let an
// exclusion-only request through with an empty deny list and answer 200 with
// the FULL subscription, where the identically vacuous countries=,, already
// 400s.
func countryTokenPresent(raw string) bool {
	for part := range strings.SplitSeq(raw, ",") {
		if strings.TrimSpace(part) != "" {
			return true
		}
	}
	return false
}

// countryPolicy resolves the four country query parameters into the allow/deny
// pair the pipeline enforces, refusing the requests the handler must not
// answer:
//
//   - neither family carries a non-blank token — the no-parameter request;
//   - every one of the four names a country policy that only a
//     filters[].type: country or asn entry can enforce, so a snapshot without
//     one (countryFilter false) cannot serve any of them;
//   - a token no group map or country-code parse can resolve;
//   - an allow-list the exclusions empty completely.
//
// The exclusions are enforced downstream as a deny-list so an IP no geo source
// covers survives them; the subtraction here only answers the documented
// "nothing is left to serve" case.
func countryPolicy(rawCountries, rawGroups, rawExcludeCountries, rawExcludeGroups string, countryFilter bool, groups map[string][]string) (filter.CountrySet, filter.CountrySet, error) {
	allowRequested := countryTokenPresent(rawCountries) || countryTokenPresent(rawGroups)
	excludeRequested := countryTokenPresent(rawExcludeCountries) || countryTokenPresent(rawExcludeGroups)
	if !allowRequested && !excludeRequested {
		return filter.CountrySet{}, filter.CountrySet{}, fiber.NewError(fiber.StatusBadRequest, "countries, groups, exclude_countries or exclude_groups is required")
	}
	if !countryFilter {
		return filter.CountrySet{}, filter.CountrySet{}, fiber.NewError(fiber.StatusBadRequest,
			"no country-capable filter configured (filters[].type: country or asn): cannot honor countries/groups/exclude_countries/exclude_groups")
	}

	allowed, bad := buildCountrySet(rawCountries, rawGroups, groups)
	denied, badExclude := buildCountrySet(rawExcludeCountries, rawExcludeGroups, groups)
	bad = append(bad, badExclude...)
	if len(bad) > 0 {
		return filter.CountrySet{}, filter.CountrySet{}, fiber.NewError(fiber.StatusBadRequest, "unknown group or country code: "+strings.Join(bad, ","))
	}
	if !allowRequested {
		// Exclusion-only: the full set means "no allow-list", which constrains
		// nothing; see filter.Permits.
		allowed = filter.All()
	}
	remaining := allowed
	remaining.Exclude(denied)
	if filter.IsEmpty(remaining) {
		return filter.CountrySet{}, filter.CountrySet{}, fiber.NewError(fiber.StatusBadRequest, "no allowed countries left after exclusions")
	}
	return allowed, denied, nil
}

// responseTooLargeText is the fetch layer's body-overrun refusal. GET /'s byte
// ceiling binds before preprocess's 50k-node count for realistic URI lengths,
// and the error reaches this handler wrapped with no sentinel errors.Is can
// reach from the server package (fetch is outside this package's file set), so
// the text is the handle; preprocess's node ceiling keeps its sentinel.
const responseTooLargeText = "response too large: over "

func isResponseTooLarge(err error) bool {
	return strings.Contains(err.Error(), responseTooLargeText)
}

func newIndexHandler(holder *Holder) fiber.Handler {
	return func(c *fiber.Ctx) error {
		snap := holder.Load()
		if key := repeatedIndexQueryParam(c); key != "" {
			return fiber.NewError(fiber.StatusBadRequest, "repeated query parameter: "+key)
		}

		rawSubscriptionURL := strings.TrimSpace(c.Query("subscription_url"))
		subURL := fetch.SubscriptionURL(rawSubscriptionURL)
		if rawSubscriptionURL == "" {
			return fiber.NewError(fiber.StatusBadRequest, "subscription_url is required")
		}
		if err := fetch.ValidatePublicHTTPSURL(subURL); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, err.Error())
		}

		allowed, denied, err := countryPolicy(
			c.Query("countries"), c.Query("groups"),
			c.Query("exclude_countries"), c.Query("exclude_groups"),
			snap.CountryFilter, snap.Groups,
		)
		if err != nil {
			return err
		}

		var sb bytes.Buffer
		sb.Grow(defaultBuilderCapacity)

		req := preprocess.FilterRequest{
			SubscriptionURL:  subURL,
			AllowedCountries: allowed,
			DeniedCountries:  denied,
		}
		// fasthttp's request context carries no deadline and never cancels on
		// client disconnect; keeping it as the parent preserves the cancellation
		// fasthttp does deliver, on shutdown.
		reqCtx, cancel := context.WithTimeout(c.Context(), indexRequestTimeout)
		defer cancel()

		stats, err := snap.Svc.Filter(reqCtx, &sb, req)
		if err != nil {
			switch {
			case errors.Is(err, preprocess.ErrTooManyNodes), isResponseTooLarge(err):
				// The 50k-node count and the 10 MiB byte cap are one ceiling
				// split across two layers; both refusals answer the documented
				// 413 so oversize stays distinguishable from an upstream fault.
				return fiber.NewError(fiber.StatusRequestEntityTooLarge, err.Error())
			case errors.Is(err, context.DeadlineExceeded):
				return fiber.NewError(fiber.StatusGatewayTimeout, "subscription preprocessing timed out")
			}
			return fiber.NewError(fiber.StatusBadGateway, "failed to preprocess subscription")
		}

		c.Set(fiber.HeaderContentType, fiber.MIMETextPlainCharsetUTF8)
		c.Set("X-Preprocessor-Stats", preprocess.FormatStats(stats))

		sb.WriteByte('\n')
		return c.Send(sb.Bytes())
	}
}

// redactSubscriptionURL renders a subscription URL for the access log. Provider
// subscription links are capability URLs — the credential sits in the query
// string or in the path — so only the host is kept verbatim, followed by a
// short digest of the whole URL so repeated requests for one subscription stay
// correlatable in the logs.
func redactSubscriptionURL(raw string) string {
	if raw == "" {
		return ""
	}
	host := "invalid"
	if parsed, err := url.Parse(raw); err == nil && parsed.Host != "" {
		host = parsed.Host
	}
	sum := sha256.Sum256([]byte(raw))
	return host + "#" + hex.EncodeToString(sum[:redactedURLDigestBytes])
}

// buildCountrySet expands the countries/groups query parameters into a set,
// returning every token it could not resolve: an unknown group name, or a token
// that is not an ISO 3166-1 alpha-2 code. A filter the server cannot honour is
// a client error — silently narrowing an exclusion to nothing would serve nodes
// in exactly the jurisdictions the caller asked to avoid.
func buildCountrySet(rawCountries, rawGroups string, groupsMap map[string][]string) (filter.CountrySet, []string) {
	var set filter.CountrySet
	var bad []string
	for part := range strings.SplitSeq(rawCountries, ",") {
		code := strings.TrimSpace(part)
		if code == "" {
			continue
		}
		if !set.Add(code) {
			bad = append(bad, code)
		}
	}
	for part := range strings.SplitSeq(rawGroups, ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		countries, ok := groupsMap[name]
		if !ok {
			bad = append(bad, name)
			continue
		}
		for _, code := range countries {
			set.Add(code)
		}
	}
	return set, bad
}

func (s *Server) Listen() error {
	s.logger.Info().Str("addr", s.listen).Msg("server starting")
	if err := s.app.Listen(s.listen); err != nil {
		return fmt.Errorf("fiber listen: %w", err)
	}
	return nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	if err := s.app.ShutdownWithContext(ctx); err != nil {
		return fmt.Errorf("fiber shutdown: %w", err)
	}
	return nil
}

func (s *Server) TestApp() *fiber.App {
	return s.app
}
