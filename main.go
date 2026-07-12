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

	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"

	"domains.lst/sub-preprocessor/internal/app"
	"domains.lst/sub-preprocessor/internal/classify"
	"domains.lst/sub-preprocessor/internal/crawl"
	"domains.lst/sub-preprocessor/internal/fetch"
	"domains.lst/sub-preprocessor/internal/log"
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
		Channels:    splitList(getenv("CRAWL_CHANNELS", "o00000000i")),
		PrivatePath: getenv("CRAWL_PRIVATE", "/config/private.yaml"),
		Pages:       atoiDefault(getenv("CRAWL_PAGES", "6"), 6),
		Prune:       boolDefault(getenv("CRAWL_PRUNE", ""), true),
	}
	interval := durationDefault(getenv("CRAWL_INTERVAL", "30m"), 30*time.Minute)

	if len(opts.Channels) == 0 {
		logger.Fatal().Msg("CRAWL_CHANNELS is empty")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	c := crawl.New(opts, logger)
	if getenv("CRAWL_RUN_ONCE", "0") == "1" {
		c.RunOnce(ctx)
		return
	}
	c.Run(ctx, interval)
}

// runClassify classifies a single URL: prints the node count and exits 0 for a
// live subscription, exits 1 otherwise (dead/expired/not a subscription), and
// exits 2 on a usage error.
func runClassify(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: sub-preprocessor classify <url>")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	fmt.Println(res.Nodes)
	return 0
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func splitList(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' })
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n
	}
	return def
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
