package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"domains.lst/sub-preprocessor/internal/fetch"
	"domains.lst/sub-preprocessor/internal/preprocess"
	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog"
)

type Filterer interface {
	Filter(ctx context.Context, b *bytes.Buffer, subscriptionURL string, rawCountries string) (preprocess.Stats, error)
}

type Server struct {
	listen string
	app    *fiber.App
	logger zerolog.Logger
}

const defaultBuilderCapacity = 4096

func New(logger zerolog.Logger, listen string, svc Filterer) *Server {
	errorHandler := func(c *fiber.Ctx, err error) error {
		code := fiber.StatusInternalServerError
		if fiberErr, ok := errors.AsType[*fiber.Error](err); ok && fiberErr != nil {
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

	app.Get("/", func(c *fiber.Ctx) error {
		subscriptionURL := strings.TrimSpace(c.Query("subscription_url"))
		rawCountries := c.Query("countries")

		if subscriptionURL == "" {
			return fiber.NewError(fiber.StatusBadRequest, "subscription_url is required")
		}
		if strings.TrimSpace(rawCountries) == "" {
			return fiber.NewError(fiber.StatusBadRequest, "countries is required")
		}
		if err := fetch.ValidatePublicHTTPSURL(subscriptionURL); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, err.Error())
		}

		var sb bytes.Buffer
		// Pre-allocate some reasonable capacity to avoid reallocations
		sb.Grow(defaultBuilderCapacity)

		stats, err := svc.Filter(c.Context(), &sb, subscriptionURL, rawCountries)
		if err != nil {
			return fiber.NewError(fiber.StatusBadGateway, "failed to preprocess subscription")
		}

		c.Set(fiber.HeaderContentType, fiber.MIMETextPlainCharsetUTF8)
		c.Set("X-Preprocessor-Stats", preprocess.FormatStats(stats))

		sb.WriteByte('\n')
		return c.Send(sb.Bytes())
	})

	return &Server{listen: listen, app: app, logger: logger}
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
