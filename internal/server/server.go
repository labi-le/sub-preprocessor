package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
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

func New(logger zerolog.Logger, listen string, holder *Holder, stableHolder *stable.Holder) *Server {
	errorHandler := func(c *fiber.Ctx, err error) error {
		code := fiber.StatusInternalServerError
		var fiberErr *fiber.Error
		if errors.As(err, &fiberErr) {
			code = fiberErr.Code
		}

		c.Set(fiber.HeaderContentType, fiber.MIMETextPlainCharsetUTF8)
		return c.Status(code).SendString(err.Error())
	}

	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
		DisableKeepalive:      true,
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
			Str("subscription_url", subscriptionURL).
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

// stableRetryAfter (seconds) is the Retry-After hint on the warm-up 503, before
// the worker publishes its first list. Short on purpose: the first list lands in
// minutes, not the inter-cycle interval.
const stableRetryAfter = "30"

func newStableHandler(holder *stable.Holder) fiber.Handler {
	return func(c *fiber.Ctx) error {
		snap := holder.Load()
		if snap == nil || len(snap.Payload) == 0 {
			c.Set("Retry-After", stableRetryAfter)
			return fiber.NewError(fiber.StatusServiceUnavailable, "stable list not ready")
		}

		c.Set(fiber.HeaderContentType, fiber.MIMETextPlainCharsetUTF8)
		c.Set("X-Stable-Stats", fmt.Sprintf(
			"updated=%s sources=%d/%d merged=%d tested=%d kept=%d",
			snap.UpdatedAt.Format(time.RFC3339),
			snap.Stats.SourcesOK, snap.Stats.SourcesTotal,
			snap.Stats.Merged, snap.Stats.Tested, snap.Stats.Kept,
		))
		return c.Send(snap.Payload)
	}
}

func newIndexHandler(holder *Holder) fiber.Handler {
	return func(c *fiber.Ctx) error {
		snap := holder.Load()
		rawSubscriptionURL := strings.TrimSpace(c.Query("subscription_url"))
		subURL := fetch.SubscriptionURL(rawSubscriptionURL)
		rawCountries := c.Query("countries")
		rawGroups := c.Query("groups")
		rawExcludeCountries := c.Query("exclude_countries")
		rawExcludeGroups := c.Query("exclude_groups")

		if rawSubscriptionURL == "" {
			return fiber.NewError(fiber.StatusBadRequest, "subscription_url is required")
		}
		if strings.TrimSpace(rawCountries) == "" && strings.TrimSpace(rawGroups) == "" &&
			strings.TrimSpace(rawExcludeCountries) == "" && strings.TrimSpace(rawExcludeGroups) == "" {
			return fiber.NewError(fiber.StatusBadRequest, "countries, groups, exclude_countries or exclude_groups is required")
		}
		if err := fetch.ValidatePublicHTTPSURL(subURL); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, err.Error())
		}

		allowed := buildCountrySet(rawCountries, rawGroups, snap.Groups)
		excluded := buildCountrySet(rawExcludeCountries, rawExcludeGroups, snap.Groups)
		if strings.TrimSpace(rawCountries) == "" && strings.TrimSpace(rawGroups) == "" {
			allowed = filter.All()
		}
		allowed.Exclude(excluded)
		if isEmpty(allowed) {
			return fiber.NewError(fiber.StatusBadRequest, "no allowed countries left after exclusions")
		}

		var sb bytes.Buffer
		// Pre-allocate some reasonable capacity to avoid reallocations
		sb.Grow(defaultBuilderCapacity)

		req := preprocess.FilterRequest{
			SubscriptionURL:  subURL,
			AllowedCountries: allowed,
		}
		stats, err := snap.Svc.Filter(c.Context(), &sb, req)
		if err != nil {
			return fiber.NewError(fiber.StatusBadGateway, "failed to preprocess subscription")
		}

		c.Set(fiber.HeaderContentType, fiber.MIMETextPlainCharsetUTF8)
		c.Set("X-Preprocessor-Stats", preprocess.FormatStats(stats))

		sb.WriteByte('\n')
		return c.Send(sb.Bytes())
	}
}

func buildCountrySet(rawCountries, rawGroups string, groupsMap map[string][]string) filter.CountrySet {
	var parts []string
	if rawCountries != "" {
		parts = append(parts, rawCountries)
	}
	if rawGroups != "" {
		for part := range strings.SplitSeq(rawGroups, ",") {
			part = strings.TrimSpace(part)
			if countries, ok := groupsMap[part]; ok {
				parts = append(parts, countries...)
			}
		}
	}
	return filter.ParseAllowed(parts...)
}

func isEmpty(set filter.CountrySet) bool {
	for _, v := range set {
		if v != 0 {
			return false
		}
	}
	return true
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

// TestApp returns the underlying Fiber app for use in tests.
func (s *Server) TestApp() *fiber.App {
	return s.app
}
