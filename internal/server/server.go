package server

import (
	"context"
	"fmt"
	"strings"

	"domains.lst/sub-preprocessor/internal/fetch"
	"domains.lst/sub-preprocessor/internal/preprocess"
	"github.com/gofiber/fiber/v2"
)

type Filterer interface {
	Filter(ctx context.Context, b *strings.Builder, subscriptionURL string, rawCountries string) (preprocess.Stats, error)
}

type Server struct {
	listen string
	app    *fiber.App
}

const defaultBuilderCapacity = 4096

func New(listen string, svc Filterer) *Server {
	app := fiber.New()

	app.Get("/healthz", func(c *fiber.Ctx) error {
		return c.SendString("ok")
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

		var sb strings.Builder
		// Pre-allocate some reasonable capacity to avoid reallocations
		sb.Grow(defaultBuilderCapacity)
		stats, err := svc.Filter(c.Context(), &sb, subscriptionURL, rawCountries)
		if err != nil {
			return fiber.NewError(fiber.StatusBadGateway, "failed to preprocess subscription")
		}

		c.Set(fiber.HeaderContentType, fiber.MIMETextPlainCharsetUTF8)
		c.Set("X-Preprocessor-Stats", preprocess.FormatStats(stats))

		sb.WriteByte('\n')
		return c.SendString(sb.String())
	})

	return &Server{listen: listen, app: app}
}

func (s *Server) Listen() error {
	if err := s.app.Listen(s.listen); err != nil {
		return fmt.Errorf("fiber listen: %w", err)
	}
	return nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- s.app.Shutdown()
	}()
	select {
	case err := <-shutdownDone:
		return err
	case <-ctx.Done():
		return fmt.Errorf("server shutdown timeout: %w", ctx.Err())
	}
}

// TestApp returns the underlying Fiber app for use in tests.
func (s *Server) TestApp() *fiber.App {
	return s.app
}
