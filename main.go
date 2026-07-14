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
	defaultCrawlInterval = 30 * time.Minute
	classifyTimeout      = 30 * time.Second
	exitUsageError       = 2
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
		Pages:        atoiDefault(getenv("CRAWL_PAGES", ""), defaultCrawlPages),
		Prune:        boolDefault(getenv("CRAWL_PRUNE", ""), true),
		MaxDepth:     intDefault(getenv("CRAWL_DEPTH", ""), defaultCrawlDepth),
		MaxChannels:  intDefault(getenv("CRAWL_MAX_CHANNELS", ""), 0),
		StatePath:    getenv("CRAWL_STATE", "/config/.crawler-state.json"),
		StateTTL:     durationDefault(getenv("CRAWL_STATE_TTL", ""), defaultCrawlStateTTL),
	}
	interval := durationDefault(getenv("CRAWL_INTERVAL", ""), defaultCrawlInterval)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	c := crawl.New(opts, logger)
	if getenv("CRAWL_RUN_ONCE", "0") == "1" {
		c.RunOnce(ctx)
		return
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
		fmt.Fprintf(os.Stderr, "not a live subscription (nodes=%d expired=%v)\n", res.Nodes, res.Expired)
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
