package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/geoblock"
	"domains.lst/sub-preprocessor/internal/log"
	"domains.lst/sub-preprocessor/internal/metrics"
	"domains.lst/sub-preprocessor/internal/preprocess"
	"domains.lst/sub-preprocessor/internal/reload"
	serverpkg "domains.lst/sub-preprocessor/internal/server"
	"domains.lst/sub-preprocessor/internal/stable"
)

const defaultConfigPath = "./config/config.yaml"

// shutdownTimeout must exceed the longest blocking call an in-flight request
// can be sitting in — a 5s DNS or ASN lookup, which cannot observe the
// cancellation fasthttp delivers until it returns — or an ordinary SIGTERM
// during a lookup reports a deadline error. 15s leaves margin for one queued
// lookup plus the response write, and 15s + controllerStopTimeout stays under
// docker-compose's stop_grace_period.
const shutdownTimeout = 15 * time.Second

// controllerStopTimeout bounds the join on the stable worker. Every stage of a
// cycle takes the cancelled context, so it normally unwinds in milliseconds;
// the bound exists so a future non-cooperative stage cannot make the process
// unkillable.
const controllerStopTimeout = 5 * time.Second

const metricsReadHeaderTimeout = 5 * time.Second

// startMetrics binds the metrics listener synchronously so a bind failure is
// reported like every other startup failure, and "started" is logged only once
// the port is actually held. Callers must skip an empty addr.
func startMetrics(ctx context.Context, addr string, m *metrics.Metrics, logger zerolog.Logger) (*http.Server, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen metrics on %s: %w", addr, err)
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())
	srv := &http.Server{
		// The bound address, not the requested one: a ":0" or hostname addr
		// resolves here, and the log line (and callers) should say where the
		// listener actually is.
		Addr:              ln.Addr().String(),
		Handler:           mux,
		ReadHeaderTimeout: metricsReadHeaderTimeout,
	}
	go func() {
		if serveErr := srv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			logger.Error().Err(serveErr).Msg("metrics listener error")
		}
	}()
	logger.Info().Str("addr", srv.Addr).Msg("metrics listener started")
	return srv, nil
}

// stopController cancels the stable worker and joins it under a bound, so a
// stage that ignores its context degrades to a logged warning instead of a hung
// shutdown.
func stopController(ctl *stable.Controller, logger zerolog.Logger) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctl.Stop()
	}()
	select {
	case <-done:
	case <-time.After(controllerStopTimeout):
		logger.Warn().Dur("budget", controllerStopTimeout).Msg("stable worker did not stop in time, abandoning it")
	}
}

// gracefulShutdown drains the HTTP server within shutdownTimeout. An expired
// budget is a warning, not an error: abandoning a request that outlived the
// budget is the expected outcome of a bounded graceful shutdown, and a routine
// SIGTERM must still exit 0. The error stays in the log line instead.
func gracefulShutdown(ctx context.Context, srv *serverpkg.Server, logger zerolog.Logger) error {
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	logger.Info().Msg("shutting down")

	switch err := srv.Shutdown(shutdownCtx); {
	case err == nil:
		logger.Info().Msg("shutdown complete")
	case errors.Is(err, context.DeadlineExceeded):
		logger.Warn().
			Err(err).
			Dur("budget", shutdownTimeout).
			Msg("shutdown deadline exceeded, abandoning in-flight requests")
	default:
		return fmt.Errorf("server shutdown: %w", err)
	}
	return nil
}

// buildStores constructs the optional geoblock store and dead-node cache from
// config. Both are nil when their feature is disabled; the caller owns the
// store's Close and wires the nil-able Blocklist/DeadCache interfaces.
func buildStores(cfg config.Config, logger zerolog.Logger) (*geoblock.Store, *stable.DeadSet, error) {
	var (
		gbStore *geoblock.Store
		dcache  *stable.DeadSet
	)
	if cfg.GeoBlock.DBPath != "" {
		store, err := geoblock.Open(cfg.GeoBlock.DBPath, cfg.GeoBlock.TTL)
		if err != nil {
			return nil, nil, fmt.Errorf("open geoblock store: %w", err)
		}
		gbStore = store
		logger.Info().Str("db", cfg.GeoBlock.DBPath).Int("blocked", store.Count()).Msg("geoblock store")
	}

	if *cfg.DeadCache.TTL > 0 {
		dcache = stable.NewDeadSet(*cfg.DeadCache.TTL)
		logger.Info().Dur("ttl", *cfg.DeadCache.TTL).Msg("dead-node cache (in-memory)")
	}

	return gbStore, dcache, nil
}

// buildProcessor wires processor options from config and constructs the
// preprocess service.
func buildProcessor(ctx context.Context, cfg config.Config, logger zerolog.Logger, pblock preprocess.Blocklist) (*preprocess.Processor, error) {
	opts := reload.OptionsFromConfig(cfg)
	opts.Blocklist = pblock
	svc, err := preprocess.NewProcessor(ctx, logger, opts)
	if err != nil {
		return nil, fmt.Errorf("create service: %w", err)
	}
	return svc, nil
}

// buildWatcher wires the config reloader and its filesystem watcher.
func buildWatcher(cfg config.Config, logger zerolog.Logger, holder *serverpkg.Holder, svc *preprocess.Processor, ctl *stable.Controller, pblock preprocess.Blocklist) (*reload.Watcher, error) {
	reloader := reload.NewReloader(defaultConfigPath, holder, logger, cfg, svc, ctl, pblock)
	watcher, err := reload.NewWatcher(defaultConfigPath, reloader.Reload, logger)
	if err != nil {
		return nil, fmt.Errorf("create config watcher: %w", err)
	}
	return watcher, nil
}

// restoreStableList seeds the worker's holder with the list the previous run
// persisted, so /stable.txt answers from the first request instead of 503 for
// a whole cycle after every restart (measured 58 minutes on a 68266-node
// pool). A missing or unusable file leaves the holder empty; LoadSnapshot
// warns and startup carries on.
func restoreStableList(cfg config.Config, logger zerolog.Logger) *stable.Holder {
	h := stable.NewHolder()
	if snap := stable.LoadSnapshot(cfg.Subscriptions.SnapshotPath, logger); snap != nil {
		h.Store(snap)
	}

	return h
}

func Run(ctx context.Context) error {
	cfg, err := config.Load(defaultConfigPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger := log.InitLogger(cfg.Log.Level)
	logger.Info().Str("level", cfg.Log.Level).Msg("logger initialized")

	gbStore, deadSet, err := buildStores(cfg, logger)
	if err != nil {
		return err
	}
	var (
		pblock preprocess.Blocklist
		sblock stable.Blocklist
		dcache stable.DeadCache
	)
	if gbStore != nil {
		defer func() { _ = gbStore.Close() }()
		pblock, sblock = gbStore, gbStore
	}
	if deadSet != nil {
		dcache = deadSet
	}

	svc, err := buildProcessor(ctx, cfg, logger, pblock)
	if err != nil {
		return err
	}

	holder := serverpkg.NewHolder(serverpkg.NewSnapshot(svc, svc, cfg.Groups))
	stableHolder := restoreStableList(cfg, logger)
	m := metrics.New()
	ctl := stable.NewController(ctx, stableHolder, func() stable.Filterer {
		return holder.Load().Worker
	}, sblock, dcache, cfg.Subscriptions.SnapshotPath, logger, m)
	if applyErr := ctl.Apply(cfg); applyErr != nil {
		return fmt.Errorf("start stable subscriptions worker: %w", applyErr)
	}
	defer stopController(ctl, logger)

	srv := serverpkg.New(logger, cfg.Server.Listen, holder, stableHolder)

	if cfg.Server.MetricsListen != "" {
		metricsSrv, metricsErr := startMetrics(ctx, cfg.Server.MetricsListen, m, logger)
		if metricsErr != nil {
			return metricsErr
		}
		defer func() { _ = metricsSrv.Close() }()
	}

	watcher, err := buildWatcher(cfg, logger, holder, svc, ctl, pblock)
	if err != nil {
		return err
	}

	// Watcher runs under a derived context; the deferred cancel+join is
	// registered AFTER the stopController/gbStore.Close defers so (LIFO) the
	// watcher is joined FIRST on every return path — an in-flight
	// Reload→ctl.Apply can never race controller/store teardown.
	watcherCtx, cancelWatcher := context.WithCancel(ctx)
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		if watchErr := watcher.Run(watcherCtx); watchErr != nil {
			logger.Error().Err(watchErr).Msg("config watcher error")
		}
	}()
	defer func() {
		cancelWatcher()
		<-watcherDone
	}()

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Listen()
	}()

	select {
	case <-ctx.Done():
		shutdownErr := gracefulShutdown(ctx, srv, logger)
		<-watcherDone
		return shutdownErr
	case listenErr := <-errCh:
		return listenErr
	}
}
