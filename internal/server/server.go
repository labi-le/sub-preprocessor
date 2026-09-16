package server

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync/atomic"
	"time"

	"domains.lst/sub-preprocessor/internal/stable"
	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog"
)

type Server struct {
	listen string
	app    *fiber.App
	logger zerolog.Logger
}

// readTimeout bounds reading the full request (slowloris hardening); handler
// execution and the response write are not covered by it.
const readTimeout = 30 * time.Second

func New(logger zerolog.Logger, listen string, stableHolder *stable.Holder) *Server {
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

	app.Use(func(c *fiber.Ctx) error {
		start := time.Now()

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

	app.Get("/stable.txt", newStableHandler(stableHolder))

	return &Server{listen: listen, app: app, logger: logger}
}

// newRecoveryMiddleware turns a handler panic into a 500. Neither fiber nor
// fasthttp recovers one, so a panic would otherwise kill the process and with
// it the in-memory stable list (/stable.txt then 503s until the worker
// republishes). It is registered inside the logging middleware so a recovered
// panic still produces an access-log line, and the panic value and stack go to
// the log only, never to the client.
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
