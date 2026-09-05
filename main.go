package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // embed the zoneinfo DB so TZ works in the distroless image

	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"

	"domains.lst/sub-preprocessor/internal/app"
	"domains.lst/sub-preprocessor/internal/classify"
	"domains.lst/sub-preprocessor/internal/crawl"
	"domains.lst/sub-preprocessor/internal/fetch"
	"domains.lst/sub-preprocessor/internal/log"
)

const (
	defaultCrawlPages    = 6
	defaultCrawlDepth    = 2
	defaultCrawlStateTTL = 720 * time.Hour
	// defaultCrawlDeadTTL is 30 days. The TTL is the re-probe period, not a
	// per-cycle cost — a record costs exactly one classify request per TTL — so
	// it trades how long a panel that came back stays locked out against how
	// often a permanently gone one is paid for again. CRAWL_DEAD_TTL=0 is off.
	defaultCrawlDeadTTL   = 720 * time.Hour
	defaultCrawlInterval  = 30 * time.Minute
	defaultCrawlInlineMax = 500
	// defaultCrawlFetchTimeout mirrors config's subscriptions.fetch.timeout
	// default (the worker's per-source fetch budget). The crawl subcommand
	// reads no config.yaml of its own, and Options.FetchTimeout makes each
	// liveness probe live inside whatever budget the worker fetches under, so
	// the mirror is the shipped value — CRAWL_FETCH_TIMEOUT must be set to
	// match when an operator tunes subscriptions.fetch.timeout in config.yaml.
	defaultCrawlFetchTimeout = 3 * time.Second
	classifyTimeout          = 30 * time.Second
	exitUsageError           = 2
)

func main() {
	log.InitDefault()

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "crawl":
			runCrawl()
			return
		case "classify":
			os.Exit(runClassify(os.Args[2:]))
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q\nusage: %s [crawl | classify <url>]\n", os.Args[1], os.Args[0])
			os.Exit(exitUsageError)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	if err := app.Run(ctx); err != nil {
		stop()
		zlog.Fatal().Err(err).Msg("")
	}
	stop()
}

// runCrawl runs the subscription crawler loop, configured entirely from the
// environment so it needs no config.yaml of its own.
func runCrawl() {
	logger := zerolog.New(os.Stderr).With().Timestamp().Str("cmd", "crawl").Logger()

	opts := crawl.Options{
		Channels:     splitList(getenv("CRAWL_CHANNELS", "")),
		ChannelsPath: getenv("CRAWL_CHANNELS_FILE", "/config/channels.yaml"),
		PrivatePath:  getenv("CRAWL_PRIVATE", "/config/private.yaml"),
		// splitList splits on spaces too, so a path holding one cannot be passed.
		CuratedPaths:  splitList(getenv("CRAWL_CURATED", "/config/sources.yaml,/config/config.yaml")),
		Pages:         atoiDefault(getenv("CRAWL_PAGES", ""), defaultCrawlPages),
		Prune:         boolDefault(getenv("CRAWL_PRUNE", ""), true),
		MaxDepth:      intDefault(getenv("CRAWL_DEPTH", ""), defaultCrawlDepth),
		MaxChannels:   intDefault(getenv("CRAWL_MAX_CHANNELS", ""), 0),
		StatePath:     getenv("CRAWL_STATE", "/config/.crawler-state.json"),
		StateTTL:      durationDefault(getenv("CRAWL_STATE_TTL", ""), defaultCrawlStateTTL),
		DeadTTL:       durationAllowZero(getenv("CRAWL_DEAD_TTL", ""), defaultCrawlDeadTTL),
		FetchTimeout:  durationDefault(getenv("CRAWL_FETCH_TIMEOUT", ""), defaultCrawlFetchTimeout),
		InlineEnabled: boolDefault(getenv("CRAWL_INLINE", ""), true),
		InlineMax:     intDefault(getenv("CRAWL_INLINE_MAX", ""), defaultCrawlInlineMax),
	}
	interval := durationDefault(getenv("CRAWL_INTERVAL", ""), defaultCrawlInterval)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	c := crawl.New(opts, logger)
	if getenv("CRAWL_RUN_ONCE", "0") == "1" {
		c.RunOnce(ctx)
		return
	}
	// CRAWL_HTTP=":8081" starts an on-demand HTTP trigger alongside the
	// schedule loop; empty keeps the crawler HTTP-less (unchanged behavior).
	if addr := getenv("CRAWL_HTTP", ""); addr != "" {
		logger.Info().Str("addr", addr).Msg("crawl HTTP trigger starting")
		// Serve returns nil only on a clean ctx-cancelled shutdown, so an
		// error while the context is still live is a listener failure — a
		// bind failure (port already taken) returns almost immediately.
		// CRAWL_HTTP was set explicitly, so running on without the trigger
		// surface would report success to the supervisor while /crawl and
		// /metrics stay dead: exit non-zero instead.
		go func() {
			if err := c.Serve(ctx, addr); err != nil && ctx.Err() == nil {
				logger.Error().Err(err).Str("addr", addr).Msg("crawl HTTP server failed")
				os.Exit(1)
			}
		}()
	}
	// CRAWL_AT="HH:MM" runs once daily at that wall-clock time (local TZ);
	// otherwise fall back to the CRAWL_INTERVAL ticker.
	at := getenv("CRAWL_AT", "")
	if h, m, ok := parseHHMM(at); ok {
		logger.Info().Str("at", at).Str("tz", time.Now().Location().String()).Msg("daily schedule")
		c.RunDaily(ctx, h, m)
		return
	}
	if at != "" {
		logger.Warn().Str("at", at).Msg("invalid CRAWL_AT, using interval schedule")
	}
	c.Run(ctx, interval)
}

// runClassify classifies a single URL: prints the node count and exits 0 for a
// live subscription, exits 1 otherwise (dead/expired/not a subscription), and
// exits 2 on a usage error.
func runClassify(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: sub-preprocessor classify <url>")
		return exitUsageError
	}
	ctx, cancel := context.WithTimeout(context.Background(), classifyTimeout)
	defer cancel()

	res, err := classify.URL(ctx, fetch.NewSafeHTTPClient(), fetch.SubscriptionURL(args[0]))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if !res.Live() {
		fmt.Fprintf(os.Stderr, "not a live subscription (reason=%s nodes=%d)\n", res.Reason(), res.Nodes)
		return 1
	}
	fmt.Fprintln(os.Stdout, res.Nodes)
	return 0
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// splitList splits a comma/whitespace-separated list; FieldsFunc never yields
// empty fields, so its result is returned directly.
func splitList(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' })
}

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n
	}
	return def
}

func intDefault(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil && n >= 0 {
		return n
	}
	return def
}

// parseHHMM parses "HH:MM" (24h). Empty or malformed input returns ok=false so
// the caller falls back to the interval schedule.
func parseHHMM(s string) (hour, minute int, ok bool) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, 0, false
	}
	return t.Hour(), t.Minute(), true
}

func durationDefault(s string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return def
}

// durationAllowZero parses a duration whose ZERO is meaningful, which
// durationDefault cannot express: it reads every non-positive parse as unset
// and substitutes the default, so "0" could never switch a feature off. Empty,
// malformed and negative input still defaults — only an explicit zero is kept.
func durationAllowZero(s string, def time.Duration) time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil || d < 0 {
		return def
	}
	return d
}

func boolDefault(s string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}
