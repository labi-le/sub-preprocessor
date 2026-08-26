# Package map and repository layout

> **When to read this:** Read for orientation, or when you need to know which package owns a concern. `routes.md` is the per-package reference with types and functions; this is the index above it.

## Important package map

- `internal/app` — app bootstrap, config load, service construction, server start
- `internal/config` — YAML config parsing/validation
- `internal/fetch` — safe HTTP fetching, file-type decoding, SSRF protections
- `internal/geofeed` — geofeed download/parse/lookup
- `internal/cidrset` — IPv4 allow-list: fetch/parse a CIDR (or bare-address) list into merged sorted ranges, membership by binary search
- `internal/resolver` — DNS resolution with an in-memory TTL cache
- `internal/asn` — ASN lookup (Team Cymru) for the ASN name/country filter
- `internal/filter` — country allow/deny bitset
- `internal/subscription` — subscription fetch/normalize/parse (scheme-generic, plus dedicated decoders for `vmess://`, legacy `ss://`, `ssr://` and `mierus://` in `vmess.go`/`ss.go`/`ssr.go`/`mieru.go`, and Xray-JSON→share-link conversion in `xray.go`)
- `internal/rewrite` — node name/fragment rewrite (`[GEO:XX]`, vmess `ps` rewrite)
- `internal/preprocess` — the core per-node filter pipeline
- `internal/geoblock` — SQLite TTL list of node hosts that failed a through-node API reachability check (gemini/claude/chatgpt)
- `internal/stable` — `/stable.txt` worker: merge/dedupe/relabel, dead-node cache skip (pre-probe), Mihomo prober + through-node filters (gemini/claude/chatgpt reachability, tidal reachability, bandwidth), the post-filter `cdn-cgi/trace` egress measurement, one-shot name annotation at publication, checker loop, holder, and the JSON snapshot (`snapshot.go`) that carries the published list across a restart
- `internal/reload` — config file watcher + hot-reload
- `internal/server` — Fiber HTTP layer
- `internal/metrics` — renders stable-cycle stats as hand-rolled Prometheus text exposition; served on `server.metrics_listen`
- `internal/geo` — the provider adapters (`geofeed`/`dbip`/`registry`/`asn`) the country filter and the annotator share, each named after the data source it queries
- `internal/classify` — decides whether a URL serves a usable subscription; behind the `classify` subcommand and every crawler candidate
- `internal/crawl` — the `crawl` subcommand: Telegram-preview crawler writing the `private.yaml` overlay; it owns the source-name convention and writes ownership and attribution as the `managed`/`feed` fields on each entry it mints, so no other package parses a name
- `internal/log` — zerolog setup, runtime level changes (`SetLevel`), the `Op` child-logger helper (`ctxlog.go`)
- `internal/ioutil` — `Lines` (non-empty, non-comment line iteration) and `UnsafeString`, shared by `cidrset`, `crawl`, `geofeed`, `preprocess`, `subscription` and `stable`

## Project layout

- `main.go` — entry point
- `config/` — the config directory (`config.yaml` + the `sources.yaml` / `private.yaml` overlays)
- `Makefile` — common targets (`run`, `test`, `fmt`, `race`, `bench`)
- `.golangci.yml` — linter configuration
- `internal/` — internal packages
- `benchmarks/` — timestamped benchmark output snapshots
