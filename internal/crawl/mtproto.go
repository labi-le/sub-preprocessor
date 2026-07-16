package crawl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth/qrlogin"
	"github.com/gotd/td/telegram/updates"
	updhook "github.com/gotd/td/telegram/updates/hook"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/mdp/qrterminal/v3"
	"github.com/rs/zerolog"
)

// tgIngestConcurrency bounds the through-post classify work spawned off the
// update loop so a burst of posts cannot exhaust goroutines or block update
// delivery; posts dropped when saturated are re-found by the poll backfill.
const tgIngestConcurrency = 8

// TGOptions configures the optional MTProto userbot push path. It is enabled
// only when APIID and APIHash are both set (from https://my.telegram.org, tied
// to a user account). The userbot receives new posts from the seed channels in
// real time and feeds the SAME classify+merge pipeline the poll uses, so it
// augments the web-scrape poll rather than replacing it — the poll remains the
// backfill for downtime and for posts dropped while ingest is saturated.
type TGOptions struct {
	APIID       int
	APIHash     string
	SessionPath string // persisted session (holds the auth key); survives restarts, mode 0600
	Password    string // optional 2FA password (CRAWL_TG_2FA); empty unless the account has one
}

// RunTelegram connects a gotd/td MTProto user client and blocks until ctx is
// done. On first run (no cached session) it serves a scannable QR at GET /qr on
// the CRAWL_HTTP endpoint (and also prints it to stdout) to scan in Telegram
// (Settings -> Devices -> Link Desktop Device); gotd then caches the
// session to opts.SessionPath so later runs skip the QR. It resolves and joins
// the seed channels (membership is required to receive their pushes) and, on
// every new post, extracts URLs, classifies them, and appends the live ones to
// private.yaml — the same overlay the scheduled poll writes.
func (c *Crawler) RunTelegram(ctx context.Context, opts TGOptions) error {
	if dir := filepath.Dir(opts.SessionPath); dir != "" {
		// gotd writes the session 0600 but does not create the parent dir.
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create session dir: %w", err)
		}
	}
	log := c.logger.With().Str("component", "telegram").Logger()

	dispatcher := tg.NewUpdateDispatcher()
	// OnLoginToken must be registered before Run so the updateLoginToken push
	// from a QR scan wakes QR.Auth.
	loggedIn := qrlogin.OnLoginToken(dispatcher)
	// The updates.Manager adds gap recovery (getDifference/getChannelDifference)
	// and, being the UpdateHandler, still forwards updates to the dispatcher.
	gaps := updates.New(updates.Config{Handler: dispatcher})

	client := telegram.NewClient(opts.APIID, opts.APIHash, telegram.Options{
		SessionStorage: &telegram.FileSessionStorage{Path: opts.SessionPath},
		UpdateHandler:  gaps,
		// Feeds updates carried in RPC results (e.g. joinChannel) back into the
		// manager so per-channel pts stays consistent.
		Middlewares: []telegram.Middleware{updhook.UpdateHook(gaps.Handle)},
	})

	sem := make(chan struct{}, tgIngestConcurrency)
	// Ingest runs off the update loop (classify does network I/O); the outer ctx
	// (not the per-update one) keeps it alive for the session.
	handle := func(e tg.Entities, msg tg.MessageClass) {
		m, ok := msg.(*tg.Message)
		if !ok || (m.Message == "" && len(m.Entities) == 0) {
			return
		}
		select {
		case sem <- struct{}{}:
		default:
			log.Warn().Msg("push: ingest saturated; skipping post (poll will backfill)")
			return
		}
		go func() {
			defer func() { <-sem }()
			c.ingestMessage(ctx, log, e, m)
		}()
	}
	dispatcher.OnNewChannelMessage(func(_ context.Context, e tg.Entities, u *tg.UpdateNewChannelMessage) error {
		handle(e, u.Message)
		return nil
	})
	dispatcher.OnNewMessage(func(_ context.Context, e tg.Entities, u *tg.UpdateNewMessage) error {
		handle(e, u.Message)
		return nil
	})

	channels := c.seedChannels()

	if runErr := client.Run(ctx, func(ctx context.Context) error {
		status, err := client.Auth().Status(ctx)
		if err != nil {
			return fmt.Errorf("auth status: %w", err)
		}
		if !status.Authorized {
			if loginErr := c.qrLogin(ctx, log, client, loggedIn, opts.Password); loginErr != nil {
				return loginErr
			}
		}
		self, err := client.Self(ctx)
		if err != nil {
			return fmt.Errorf("self: %w", err)
		}
		log.Info().Str("user", self.Username).Int("channels", len(channels)).Msg("telegram userbot authorized")
		joinChannels(ctx, log, client.API(), channels)
		log.Info().Msg("listening for new channel posts")
		return gaps.Run(ctx, client.API(), self.ID, updates.AuthOptions{IsBot: self.Bot})
	}); runErr != nil {
		return fmt.Errorf("telegram client: %w", runErr)
	}
	return nil
}

// seedChannels returns the deduped seed-channel slugs from static config
// (CRAWL_CHANNELS) plus the hot-reloadable channels file, normalized to bare
// usernames for ContactsResolveUsername.
func (c *Crawler) seedChannels() []string {
	seen := map[string]struct{}{}
	var out []string
	raw := append(slices.Clone(c.opts.Channels), loadChannels(c.opts.ChannelsPath, c.logger)...)
	for _, entry := range raw {
		slug := normalizeSlug(entry)
		if slug == "" {
			continue
		}
		if _, dup := seen[slug]; dup {
			continue
		}
		seen[slug] = struct{}{}
		out = append(out, slug)
	}
	return out
}

// qrLogin drives the QR login: each login token is published to c.tgLoginURL so
// the GET /qr endpoint can serve it as a scannable PNG, and also rendered to
// stdout as a fallback. tgLoginURL is cleared when the attempt finishes; then
// the 2FA branch runs if the account has a password.
func (c *Crawler) qrLogin(ctx context.Context, log zerolog.Logger, client *telegram.Client, loggedIn qrlogin.LoggedIn, password string) error {
	log.Info().Msg("MTProto login required: open GET /qr on the CRAWL_HTTP endpoint to scan (or use the QR printed below)")
	_, err := client.QR().Auth(ctx, loggedIn, func(_ context.Context, token qrlogin.Token) error {
		u := token.URL()
		c.tgLoginURL.Store(&u)
		fmt.Fprintln(os.Stdout, "\nTelegram login — scan this QR (or open GET /qr), Telegram → Settings → Devices → Link Desktop Device:")
		qrterminal.Generate(u, qrterminal.L, os.Stdout)
		fmt.Fprintln(os.Stdout)
		return nil
	})
	c.tgLoginURL.Store(nil) // attempt finished (scanned, failed, or ctx done): stop serving the QR
	switch {
	case err == nil:
		return nil
	case tgerr.Is(err, "SESSION_PASSWORD_NEEDED"):
		if password == "" {
			return errors.New("account requires a 2FA password: set CRAWL_TG_2FA")
		}
		if _, perr := client.Auth().Password(ctx, password); perr != nil {
			return fmt.Errorf("2FA password: %w", perr)
		}
		log.Info().Msg("2FA password accepted")
		return nil
	default:
		return fmt.Errorf("qr login: %w", err)
	}
}

// joinChannels resolves each seed slug and joins the channel. Membership is
// what makes Telegram push a public channel's new posts to this session (a
// bare resolve does not). Per-channel errors are logged and skipped so one bad
// slug never stops the rest.
func joinChannels(ctx context.Context, log zerolog.Logger, api *tg.Client, channels []string) {
	for _, slug := range channels {
		res, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: slug})
		if err != nil {
			log.Warn().Err(err).Str("channel", slug).Msg("resolve channel failed")
			continue
		}
		var ch *tg.Channel
		for _, chat := range res.Chats {
			if cc, ok := chat.(*tg.Channel); ok {
				ch = cc
				break
			}
		}
		if ch == nil {
			log.Warn().Str("channel", slug).Msg("resolved peer is not a channel")
			continue
		}
		if _, joinErr := api.ChannelsJoinChannel(ctx, ch.AsInput()); joinErr != nil {
			// Already-a-participant is expected on restarts and is harmless.
			log.Warn().Err(joinErr).Str("channel", slug).Msg("join channel failed (continuing; may already be joined)")
			continue
		}
		log.Info().Str("channel", slug).Str("title", ch.Title).Msg("joined channel")
	}
}

// ingestMessage extracts candidate URLs from one post, classifies them, and
// appends the live subscriptions to private.yaml. It mirrors one scan iteration
// but event-driven for a single message.
func (c *Crawler) ingestMessage(ctx context.Context, log zerolog.Logger, e tg.Entities, m *tg.Message) {
	urls := messageURLs(m)
	cand := make([]string, 0, len(urls))
	for _, u := range urls {
		if candidate(u) {
			cand = append(cand, u)
		}
	}
	if len(cand) == 0 {
		return
	}
	live, _ := c.classifyAll(ctx, cand)
	if len(live) == 0 {
		return
	}
	added := c.appendLive(live)
	if added == 0 {
		return
	}
	src := ""
	if pc, ok := m.PeerID.(*tg.PeerChannel); ok {
		if ch, found := e.Channels[pc.ChannelID]; found {
			src = ch.Username
		}
	}
	log.Info().Str("channel", src).Int("added", added).Int("live", len(live)).Msg("push: live subscriptions added")
}

// messageURLs returns the https URLs in a post: those in the visible text (via
// the shared urlRe) plus hidden ones behind text_url entities (markdown links).
func messageURLs(m *tg.Message) []string {
	out := extractURLs(m.Message)
	for _, ent := range m.Entities {
		if tu, ok := ent.(*tg.MessageEntityTextURL); ok {
			out = append(out, strings.TrimRight(tu.URL, trimSet))
		}
	}
	return out
}

// appendLive adds newly-discovered live subscription URLs to private.yaml as
// managed sources, guarded by pfMu so it never clobbers a concurrent scan
// cycle's write. Names are deterministic (managedName), so the same URL from
// the poll and the push collapse to one entry; pruning stays the scan's job.
// It returns how many new sources were added.
func (c *Crawler) appendLive(live map[string]bool) int {
	c.pfMu.Lock()
	defer c.pfMu.Unlock()
	pf, err := loadPrivate(c.opts.PrivatePath)
	if err != nil {
		c.logger.Error().Err(err).Str("path", c.opts.PrivatePath).Msg("push: read private.yaml failed")
		return 0
	}
	have := make(map[string]struct{}, len(pf.Subscriptions.Sources))
	for _, s := range pf.Subscriptions.Sources {
		have[s.URL] = struct{}{}
	}
	added := 0
	for u := range live {
		if _, dup := have[u]; dup {
			continue
		}
		pf.Subscriptions.Sources = append(pf.Subscriptions.Sources, source{Name: managedName(u), URL: u})
		have[u] = struct{}{}
		added++
	}
	if added == 0 {
		return 0
	}
	if writeErr := writePrivate(c.opts.PrivatePath, pf); writeErr != nil {
		c.logger.Error().Err(writeErr).Msg("push: write private.yaml failed")
		return 0
	}
	return added
}
