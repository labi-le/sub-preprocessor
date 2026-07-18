# GEO annotator provider chain + free offline geo databases

## Problem

`annotate: [{tag: GEO, provider: geofeed}]` produces `[GEO:??]` for ~50% of
nodes. The configured geofeed sources are self-published RFC 8805 feeds and
only cover networks that publish them.

## Goal

- `[GEO:??]` drops from ~50% to near zero.
- Annotation resolves through an ordered provider chain (like `filters`):
  first provider that returns a country wins; all miss → `??` as today.
- New providers are free, keyless, downloadable databases held fully in
  memory — zero per-IP network calls (the only network fallback is the
  existing Cymru resolver, already TTL-cached 24h/5m negative).
- Node names carry only `[GEO:XX]` — no new tag content.

## Providers

Each provider owns an **independent** lookup; cross-provider precedence is
the chain order in `annotate`, never a merged database. The stable-sort /
most-specific rules below apply only *within* one provider's database
(geofeed merges several feeds; registry merges 5 RIR files).

| Name | Source | Semantics | Coverage |
|---|---|---|---|
| `geofeed` | existing RFC 8805 feeds | operator-published geolocation | low, precise |
| `dbip` (new) | DB-IP Country Lite, `https://download.db-ip.com/free/dbip-country-lite-{yyyy-mm}.csv.gz`, gzip CSV `start,end,cc`, monthly, CC BY 4.0 | geolocation | ~full |
| `registry` (new) | 5 RIR `delegated-<rir>-extended-latest` files, pipe-separated, daily | RIR registration country | full allocated space |
| `asn` | existing Team Cymru DNS | AS registration country + name | full, per-IP DNS (cached) |

### Failure semantics

- `{yyyy-mm}` in a URL expands to the current UTC month at fetch time; on
  HTTP 404 (typed `fetch.StatusError`) retry once with the previous month.
- Both months fail / network down at startup: **warn and start with an
  empty dbip lookup** — the chain degrades to the next provider; the
  background refresh retries on the normal trigger path (loadedAt stays
  zero, so the next lookup-triggered refresh attempt reloads). Startup
  NEVER fails because db-ip.com is down.
- Refresh failure with data already loaded: **keep stale data** (geofeed
  `doReload` pattern).
- Registry: per-URL skip-warn-continue; error only when ALL five fail
  (mirrors `LoadAll`). Same empty-start / keep-stale rules as dbip.
- DB-IP attribution link goes to README (CC BY 4.0 requirement).

## Config

```yaml
geo:
  geofeed: { ... }            # unchanged
  dbip:                       # optional; defaults shown
    url: https://download.db-ip.com/free/dbip-country-lite-{yyyy-mm}.csv.gz
    refresh_interval: 24h
  registry:                   # optional; defaults to the 5 RIR URLs
    urls: [ ... ]
    refresh_interval: 24h
  asn: { ... }                # unchanged
annotate:
  - tag: GEO
    providers: [geofeed, dbip, asn]   # ordered chain
  - tag: IP
```

- `AnnotateSpec.Providers []string` (yaml `providers`) replaces the single
  `provider`. Because `Load` uses non-strict yaml, the old key would be
  silently dropped — so the struct KEEPS a `Provider string` field whose
  only purpose is rejection: validation fails with
  `annotate: "provider" was renamed to "providers" (ordered list)` when it
  is set. No silent behavior change is possible.
- Defaults preserve old behavior: GEO → `[geofeed]`, ASN → `[asn]`. `IP`
  takes no providers (error if given). Members ∈
  `{geofeed, dbip, registry, asn}`; duplicates within an entry are an error.
- `dbip`/`registry` are constructed **only when referenced** by an
  `annotate` entry: `NewProcessor` scans `Options.Annotate` before building
  them; unreferenced → no download, no goroutine, nothing.
- Non-goals: the country **filter** keeps `provider: geofeed|asn`. Note the
  asymmetry: with exclusions active, the filter may drop a node whose
  country only dbip knows, and a `[GEO:XX]` tag from dbip is not
  filter-verified. Documented in README; acceptable for now.

## internal/geofeed generalization

- New `Range{Start, End netip.Addr, Country CountryCode}` (inclusive, one
  family per entry) + `NewRangeLookup([]Range) CountryLookup`.
  `NewLookup([]Entry)` keeps its signature; prefixes convert to ranges.
  netip.Addr pairs (not Prefix) because DB-IP ranges are not CIDR-aligned.
- v4 keeps the `(start,end uint32)` structure. RIR v4 records are
  start + count (non-CIDR possible) — ingest as ranges directly.
  RIR v6 records are prefix + length (CIDR by format definition).
- IPv6 lookup moves from linear scan to the v4 structure: sorted by start,
  max-end array, binary search + bounded backward walk. Required: DB-IP
  brings O(100k) v6 ranges. A v6 benchmark at realistic scale (~100k
  ranges) is added against the CURRENT linear scan FIRST, then the rewrite
  lands, then before/after is compared.
- Most-specific = smallest span among covering ranges; identical ranges →
  earliest input order wins (stable sort).
- Parsers (per-line tolerant, like RFC 8805 parse):
  - `ParseDBIP`: `start_ip,end_ip,CC`; skip malformed, mixed-family, `ZZ`.
  - `ParseDelegated`: pipe format; keep `ipv4|ipv6` × `allocated|assigned`
    with exactly-2-ASCII-letter country; skip `ZZ`, empty, `*`, comments,
    version header, `summary` rows, `available|reserved`, asn records.
- Memory: ~500k v4 (~12 B each) + ~200k v6 (~40 B each) ≈ 15–30 MB total
  including index arrays. Acceptable; noted for operators.
- `fetch` gains a typed `StatusError` (code-carrying) so 404 detection does
  not parse error strings. Existing `maxGeofeedSize` (256 MB) already fits
  the ~150 MB decoded DB-IP CSV.

## Wiring

- `Processor` holds dbip/registry lookups next to the geofeed one, same
  TryLock background refresh per `refresh_interval`.
- `geo.Provider` instances read through getters (pattern of `NewGeofeed`).
- Annotator: `annotTag.providers []geo.Provider`; GEO = first non-zero
  country, ASN = first non-empty AS name.
- Reload carry-over (all three required, mirroring geofeed):
  - `preprocess.Options`: `DBIP`/`Registry` configs + `PreloadedDBIP`,
    `PreloadedDBIPLoadedAt`, `PreloadedRegistry`,
    `PreloadedRegistryLoadedAt`.
  - `Processor.DBIPState()` / `RegistryState()` getters.
  - `config.DBIPChanged` / `RegistryChanged` diffs; reloader carries the
    loaded lookups when unchanged (no 30 MB re-download per reload).
  - `reload.OptionsFromConfig` maps the new blocks.

## Verification

- Unit: parsers (v4/v6/ZZ/garbage/non-CIDR), month templating + 404
  fallback + double-404 empty start, v6 indexed lookup (nesting,
  most-specific, ties, misses), annotator chain order and miss handling,
  `provider`-rename rejection, defaults, diff helpers.
- `make bench`: v6 baseline before rewrite, compare after; v4 must not
  regress.
- Smoke: live run downloading real DB-IP + RIR files, annotate an inline
  node list, observe `[GEO:XX]` where geofeed alone missed.
- `make race`, `make lint`.
