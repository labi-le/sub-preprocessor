// Package crawl scrapes public Telegram channel web previews (t.me/s/<channel>),
// treats every https link found as a subscription candidate, and keeps the ones
// that classify as a live subscription — appending them to the private.yaml
// overlay the preprocessor merges into subscriptions.sources. It is format
// agnostic: it matches the artifact (an https URL that returns a subscription),
// not any channel-specific wrapper pattern.
package crawl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"

	"domains.lst/sub-preprocessor/internal/classify"
	"domains.lst/sub-preprocessor/internal/fetch"
)

// managedPrefix marks sources this crawler owns. Sources without it (hand-added
// private subscriptions) are never touched.
const managedPrefix = "tg-"

const (
	classifyConcurrency = 8
	classifyTimeout     = 15 * time.Second
	fetchTimeout        = 20 * time.Second
	userAgent           = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/125.0 Safari/537.36"
	maxPageBytes        = 8 << 20 // cap on bytes read from a single channel page
	oneDay              = 24 * time.Hour
)

var (
	urlRe    = regexp.MustCompile(`https://[^\s"'<>]+`)
	cursorRe = regexp.MustCompile(`data-post="[^"]+/(\d+)"`)
	trimSet  = ".,;:!?)]}'\""
)

// Options configures a crawl run.
type Options struct {
	Channels     []string // static seed channels (from CRAWL_CHANNELS); merged with ChannelsPath
	ChannelsPath string   // YAML file of seed channels, re-read each cycle for hot-reload
	PrivatePath  string
	Pages        int           // t.me/s pages (~20 msgs each) to walk back per seed channel
	Prune        bool          // drop managed sources that no longer classify as live
	MaxDepth     int           // repost-recursion depth; 0 = only seed channels (no recursion)
	MaxChannels  int           // safety cap on discovered (non-seed) channels per cycle; 0 = unlimited
	StatePath    string        // persisted productive-channel memory; empty disables persistence
	StateTTL     time.Duration // drop a productive channel from memory after this long without a live sub
}

type source struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
}

type privateFile struct {
	Subscriptions struct {
		Sources []source `yaml:"sources"`
	} `yaml:"subscriptions"`
}

// Crawler runs crawl cycles.
type Crawler struct {
	opts   Options
	client fetchClient
	logger zerolog.Logger
}

// fetchClient fetches a channel page; an interface so tests can avoid the network.
type fetchClient interface {
	page(ctx context.Context, u string) (string, error)
}

// httpFetcher fetches a page with the SSRF-safe client (t.me is public so it
// passes the gate) and a browser User-Agent.
type httpFetcher struct{}

func (httpFetcher) page(ctx context.Context, u string) (string, error) {
	fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := fetch.NewSafeHTTPClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("bad status: %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes))
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}
	return string(b), nil
}

func New(opts Options, logger zerolog.Logger) *Crawler {
	return &Crawler{opts: opts, client: httpFetcher{}, logger: logger}
}

// Run executes RunOnce immediately, then every interval until ctx is done.
func (c *Crawler) Run(ctx context.Context, interval time.Duration) {
	c.RunOnce(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.RunOnce(ctx)
		}
	}
}

// RunDaily runs one cycle at the next occurrence of hour:min in the process's
// local time zone, then once every 24h at that wall-clock time, until ctx is
// done. Unlike Run it does not fire immediately — it waits for the scheduled
// time.
func (c *Crawler) RunDaily(ctx context.Context, hour, minute int) {
	for {
		next := nextDaily(time.Now(), hour, minute)
		c.logger.Info().Time("next_run", next).Str("in", time.Until(next).Truncate(time.Second).String()).
			Msg("crawl scheduled")
		t := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
			c.RunOnce(ctx)
		}
	}
}

// nextDaily returns the next instant at hour:min (local) strictly after now.
func nextDaily(now time.Time, hour, minute int) time.Time {
	n := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !n.After(now) {
		n = n.Add(oneDay)
	}
	return n
}

// RunOnce performs one crawl+classify+merge cycle. The private overlay is only
// rewritten when the managed source set actually changes, so an unchanged cycle
// triggers no reload.
func (c *Crawler) RunOnce(ctx context.Context) {
	pf, err := loadPrivate(c.opts.PrivatePath)
	if err != nil {
		c.logger.Error().Err(err).Str("path", c.opts.PrivatePath).Msg("read private.yaml failed")
		return
	}

	// Discover live subscription URLs by scanning the channel repost graph,
	// seeded by configured channels plus remembered productive ones. scan
	// records freshly productive channels into st; stale ones are pruned.
	st := loadState(c.opts.StatePath)
	live := c.scan(ctx, &st)
	st.prune(time.Now().Add(-c.opts.StateTTL))
	if saveErr := saveState(c.opts.StatePath, st); saveErr != nil {
		c.logger.Warn().Err(saveErr).Msg("save crawler state failed")
	}
	c.logger.Info().Int("discovered", len(live)).Int("productive", len(st.Productive)).
		Msg("live subscriptions discovered")

	managedURL := c.recheckManaged(ctx, pf, live)
	next, managed := c.mergeManaged(pf, live, managedURL)
	if sameSources(pf.Subscriptions.Sources, next) {
		c.logger.Info().Int("managed", len(managed)).Msg("no change")
		return
	}
	pf.Subscriptions.Sources = next
	if writeErr := writePrivate(c.opts.PrivatePath, pf); writeErr != nil {
		c.logger.Error().Err(writeErr).Msg("write private.yaml failed")
		return
	}
	c.logger.Info().Int("managed", len(managed)).Int("total", len(next)).Msg("private.yaml updated")
}

// recheckManaged records the URLs of existing managed sources and re-classifies
// the ones not rediscovered this cycle, marking any still live in live so prune
// can drop the dead ones (and !Prune can retain the still-live ones).
func (c *Crawler) recheckManaged(ctx context.Context, pf privateFile, live map[string]bool) map[string]bool {
	managedURL := map[string]bool{}
	var recheck []string
	for _, s := range pf.Subscriptions.Sources {
		if strings.HasPrefix(s.Name, managedPrefix) {
			managedURL[s.URL] = true
			if !live[s.URL] {
				recheck = append(recheck, s.URL)
			}
		}
	}
	for u := range c.classifyAll(ctx, recheck) {
		live[u] = true
	}
	return managedURL
}

// mergeManaged combines the retained hand-added sources with the current managed
// set (deduped and sorted by name) and returns the full next source list plus
// the managed subset for logging.
func (c *Crawler) mergeManaged(pf privateFile, live, managedURL map[string]bool) (kept, managed []source) {
	all := map[string]struct{}{}
	for _, s := range pf.Subscriptions.Sources {
		if strings.HasPrefix(s.Name, managedPrefix) {
			all[s.URL] = struct{}{}
		} else {
			kept = append(kept, s)
		}
	}
	for u := range live {
		all[u] = struct{}{}
	}
	seen := map[string]bool{}
	for u := range all {
		keep := live[u]
		if !c.opts.Prune && managedURL[u] && !keep {
			keep = true
		}
		if keep && !seen[u] {
			seen[u] = true
			managed = append(managed, source{Name: managedName(u), URL: u})
		}
	}
	sort.Slice(managed, func(i, j int) bool { return managed[i].Name < managed[j].Name })
	kept = append(kept, managed...)
	return kept, managed
}

func (c *Crawler) classifyAll(ctx context.Context, urls []string) map[string]bool {
	live := make(map[string]bool, len(urls))
	var mu sync.Mutex
	sem := make(chan struct{}, classifyConcurrency)
	var wg sync.WaitGroup
	client := fetch.NewSafeHTTPClient()
	for _, u := range urls {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			cctx, cancel := context.WithTimeout(ctx, classifyTimeout)
			defer cancel()
			res, err := classify.URL(cctx, client, fetch.SubscriptionURL(u))
			if err == nil && res.Live() {
				mu.Lock()
				live[u] = true
				mu.Unlock()
			}
		}(u)
	}
	wg.Wait()
	return live
}

// extractURLs returns every https URL in an HTML page, HTML-unescaped and
// stripped of trailing punctuation. Links appear both in href attributes and as
// plain text inside <pre> blocks, so it scans the whole page.
func extractURLs(page string) []string {
	page = html.UnescapeString(page)
	matches := urlRe.FindAllString(page, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, strings.TrimRight(m, trimSet))
	}
	return out
}

// pageCursor returns the smallest message id on a t.me/s page, used as the
// ?before= cursor for the next older page.
func pageCursor(page string) string {
	best := ""
	for _, m := range cursorRe.FindAllStringSubmatch(page, -1) {
		id := m[1]
		if best == "" || less(id, best) {
			best = id
		}
	}
	return best
}

// candidate reports whether a URL is worth fetching: not obvious Telegram noise
// and accepted by the SSRF public-https gate.
func candidate(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || isNoiseHost(u.Hostname()) {
		return false
	}
	return fetch.ValidatePublicHTTPSURL(fetch.SubscriptionURL(raw)) == nil
}

// isNoiseHost matches hosts that never serve subscriptions (Telegram itself and
// its media CDN), so they are skipped before the fetch.
func isNoiseHost(host string) bool {
	host = strings.ToLower(host)
	switch host {
	case "t.me", "telegram.org", "www.telegram.org", "telegram.me", "telegram.dog":
		return true
	}
	return host == "telesco.pe" || strings.HasSuffix(host, ".telesco.pe")
}

func managedName(u string) string {
	sum := sha256.Sum256([]byte(u))
	return managedPrefix + hex.EncodeToString(sum[:])[:10]
}

func loadPrivate(path string) (privateFile, error) {
	var pf privateFile
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return pf, nil
		}
		return pf, fmt.Errorf("read private.yaml: %w", err)
	}
	if unmarshalErr := yaml.Unmarshal(b, &pf); unmarshalErr != nil {
		return pf, fmt.Errorf("unmarshal private.yaml: %w", unmarshalErr)
	}
	return pf, nil
}

func writePrivate(path string, pf privateFile) error {
	b, err := yaml.Marshal(pf)
	if err != nil {
		return fmt.Errorf("marshal private.yaml: %w", err)
	}
	tmp := path + ".tmp"
	if writeErr := os.WriteFile(tmp, b, 0o644); writeErr != nil { //nolint:gosec // private.yaml is read by the service under another uid; 0644 required
		return fmt.Errorf("write temp: %w", writeErr)
	}
	if renameErr := os.Rename(tmp, path); renameErr != nil {
		return fmt.Errorf("rename: %w", renameErr)
	}
	return nil
}

func sameSources(a, b []source) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[source]int{}
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// less compares two decimal message-id strings numerically without allocating
// an int when lengths differ.
func less(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}
