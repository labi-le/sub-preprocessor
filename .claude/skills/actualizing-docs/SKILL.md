---
name: actualizing-docs
description: Use when README.md, routes.md, AGENTS.md, or config comments in this repo may have drifted from the code — after adding features, changing the config schema, adding or renaming packages or metrics, or when asked to update, refresh, or actualize documentation.
---

# Actualizing Documentation

## Overview

Code is the only ground truth. `routes.md` and `AGENTS.md` drift like any other doc —
they are surfaces you FIX, never sources you cite. Every claim you write must trace
to a file:line you read in this session.

## Doc surfaces — check every one the change touches

| Surface | Owns | Update when |
|---|---|---|
| `README.md` | user-facing behavior, endpoints, config overview, deploy | any behavior/config/endpoint change |
| `routes.md` | per-package LLM map (types, functions, dep graph) | package added/removed/restructured; public API changed |
| `AGENTS.md` | agent conventions + "Current config shape" | config schema or workflow rule changed |
| `config/config.yaml` comments | per-key semantics and defaults | key semantics changed |
| `deploy/grafana/sub-preprocessor.json` | Grafana dashboard | ANY metric added/renamed in `internal/metrics` — same commit |

## Ground truth map — claim area → verify against

| Claim area | Source |
|---|---|
| Endpoints, query params, headers, status codes | `internal/server/server.go` |
| Config keys, defaults, validation, overlays | `internal/config/config.go` (+ `config/config.yaml`) |
| Hot-reload, watched files, restart-required keys | `internal/reload/{watcher,reloader}.go`, `internal/app/app.go` |
| Stable cycle order and failure semantics | `internal/stable/checker.go`, `controller.go` |
| Filters: IP-stage vs through-node | `internal/preprocess/filters.go`, `internal/stable/nodefilter.go`, `prober_*.go` |
| SSRF policy, HTTP clients | `internal/fetch/fetch.go` |
| Crawler defaults and env vars | `main.go` consts + `runCrawl`, `internal/crawl/` |
| Metric names | `internal/metrics/metrics.go` |
| Deploy, ports, secrets, nix modules | `docker-compose.yaml`, `Makefile`, `flake.nix`, `deploy/` |

## Process

1. List every claim in the target doc that the change could invalidate.
2. Verify each against the ground truth map — read the code, not `routes.md`.
3. Rewrite, stating only what you verified.
4. Dispatch 2–3 read-only scout reviewers with DISJOINT sections. Their contract:
   verify against SOURCE CODE (code wins over `routes.md`/`AGENTS.md`); numbered
   findings; each = quoted claim + verdict + file:line + correction; explicit
   "accurate" confirmation per area.
5. Fix findings. When `routes.md`/`AGENTS.md` was the stale party, fix it in the
   same pass.

## Known drift traps — each has produced a real doc error

- **Scheme lists.** The server parser is scheme-generic (`internal/subscription`);
  the fixed proxy-scheme list exists only in `internal/classify`. Never present it
  as server behavior.
- **Overlays.** THREE files hot-reload: `config.yaml`, `sources.yaml` (curated),
  `private.yaml` (crawler-managed). Docs repeatedly said two.
- **Restart-required keys.** `server.listen` (warned), `server.metrics_listen`
  (silently ignored — the metrics server starts once in `app.Run`, no diff helper),
  `geoblock.db_path`/`ttl`, `deadcache.ttl` (warned via `StoresChanged`).
- **Defaults vs deployment.** `main.go` consts (CRAWL_DEPTH=2, interval 30m) differ
  from `docker-compose.yaml` values (3, 1h). Say which one you are documenting.
- **Cycle ordering.** Read `checker.go` before claiming ordering or failure
  semantics (e.g. `recordDead` runs before the node filters).

## Red flags — STOP and re-verify against code

- You are about to cite `routes.md` or `AGENTS.md` as evidence for a claim.
- A claim has no file:line you personally read this session.
- You touched a metric and the Grafana JSON is unchanged.
- "routes.md is maintained, it's probably right" — that exact assumption put two
  errors into README once; independent code review caught them.
