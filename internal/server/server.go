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
	Filter(ctx context.Context, subscriptionURL string, countries []string) ([]string, preprocess.Stats, error)
}

type Server struct {
	listen string
	app    *fiber.App
}

func New(listen string, svc Filterer) *Server {
	app := fiber.New()

	app.Get("/healthz", func(c *fiber.Ctx) error {
		return c.SendString("ok")
	})

	app.Get("/", func(c *fiber.Ctx) error {
		subscriptionURL := strings.TrimSpace(c.Query("subscription_url"))
		countries := parseCountries(c.Query("countries"))

		if subscriptionURL == "" {
			return fiber.NewError(fiber.StatusBadRequest, "subscription_url is required")
		}
		if len(countries) == 0 {
			return fiber.NewError(fiber.StatusBadRequest, "countries is required")
		}
		if err := fetch.ValidatePublicHTTPSURL(subscriptionURL); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, err.Error())
		}

		lines, stats, err := svc.Filter(c.Context(), subscriptionURL, countries)
		if err != nil {
			return fiber.NewError(fiber.StatusBadGateway, "failed to preprocess subscription")
		}

		c.Set(fiber.HeaderContentType, fiber.MIMETextPlainCharsetUTF8)
		c.Set("X-Preprocessor-Stats", preprocess.FormatStats(stats))

		return c.SendString(strings.Join(lines, "\n") + "\n")
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

func parseCountries(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}

	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
