# Crawler Push-Subscription — Design & Decision

You asked for either (A) an HTTP method to trigger the crawler on demand, or (B) a way to *subscribe* to new posts in the tracked Telegram channels instead of polling. **(A) is shipped** (`POST /crawl`, gated by `CRAWL_HTTP`). This doc scopes **(B)** — it needs a decision from you before implementation because it changes the crawler's data-source model and needs Telegram credentials.

## Today (polling)

`internal/crawl` scrapes the *public web preview* `https://t.me/s/<channel>` (+ `?before=` pagination) over the SSRF-safe HTTP client, extracts every `https` link, and keeps the ones that `classify` as a live subscription. No Telegram account, no API key, no auth — just HTML scraping on the `subscriptions.interval`/`CRAWL_AT` schedule. Robust and cheap, but it *pulls* on a timer, so new posts are seen with up to one interval of latency and every cycle re-fetches all seed channels.

## Push options

**Option B1 — Bot API (`channel_post` updates). NOT VIABLE for this use case.**
A Telegram bot receives `channel_post` updates *only for channels where it is a member/admin* ([core.telegram.org/bots/faq](https://core.telegram.org/bots/faq)). The crawler tracks **arbitrary public channels we do not own**, so a bot can't be added to them. Bots also can't join channels or read history via the Bot API. Dead end for crawling third-party channels.

**Option B2 — MTProto user client ("userbot"). The only real push path.**
An MTProto client (a logged-in *user* account, not a bot) connects directly to Telegram's servers — no HTTP, no polling — and receives `updateNewChannelMessage` in real time. It can **passively receive updates from public channels it is not a member of** (Telegram pushes them as long as the session periodically calls `updates.getChannelDifference`) ([core.telegram.org/api/updates](https://core.telegram.org/api/updates), [Telethon: Bot API vs MTProto](https://docs.telethon.dev/en/stable/concepts/botapi-vs-mtproto.html)). This is exactly "subscribe, don't poll."

Costs / requirements of B2:
- **A Go MTProto library** — realistically [`github.com/gotd/td`](https://github.com/gotd/td) (pure Go, `CGO_ENABLED=0`-friendly; `tdlib` is C/cgo → conflicts with our distroless/static build, rejected).
- **Telegram API credentials**: `api_id` + `api_hash` from my.telegram.org, tied to **a phone-number user account**. Stored like the gemini key (agenix secret).
- **A persistent session**: phone-login once, persist the session string; handle reconnects, auth expiry, and **FLOOD_WAIT** rate limits.
- **A stateful long-lived connection** — a departure from the current stateless per-cycle scrape; the crawler becomes an always-connected client.
- **ToS caveat**: automating a user account ("userbot") is tolerated in practice for read-only use but is not officially blessed; the account carries some ban risk.

Design sketch if B2 is chosen: a new `internal/tgclient` wrapping `gotd/td`; on startup, resolve+subscribe to the seed channels; on `updateNewChannelMessage`, run the *existing* extract→classify→merge-to-`private.yaml` pipeline for that one post (event-driven instead of per-cycle); keep the current web-scrape poll as a periodic fallback/backfill; session + `api_id`/`api_hash` via agenix. The classify/merge half is unchanged — only the *ingestion* switches from pull to push.

## Recommendation

- **Keep the polling scrape as the baseline** (zero-credential, robust) and **use the just-shipped `POST /crawl` trigger** for on-demand freshness (e.g. cron, a webhook, or a tiny bot that *is* in a few channels pinging it). This covers "trigger on demand" now, with no new deps or accounts.
- **Adopt B2 (MTProto userbot) only if you want true real-time push** and are willing to (1) create/provide a Telegram user account + `api_id`/`api_hash`, (2) accept the `gotd/td` dependency and a stateful client, and (3) accept the userbot ToS risk.

## Decision needed from you

1. Ship as-is (polling + `POST /crawl` trigger), **or** invest in B2 (MTProto push)?
2. If B2: can you provide a Telegram user account + `api_id`/`api_hash` (for an agenix secret)? Should push **replace** the poll or run **alongside** it (poll as backfill)?

I did **not** implement B2 — it needs your credentials + the dependency/ToS call above.
