#!/usr/bin/env bash
# Crawls a public Telegram channel web preview (t.me/s/<channel>), extracts
# is.wepogp.gay subscription links posted there, validates each one is a live
# subscription endpoint, and appends the new ones to the private.yaml overlay
# that sub-preprocessor merges into its subscriptions.sources. The app's config
# watcher reacts to private.yaml, so the stable worker restarts and picks them
# up automatically. Optionally prunes previously added links that went dead.
set -euo pipefail

CHANNEL="${CHANNEL:-o00000000i}"
PRIVATE="${PRIVATE:-/config/private.yaml}"
INTERVAL="${INTERVAL:-1800}"
PAGES="${PAGES:-3}"                 # how many t.me/s pages (~20 msgs each) to walk back
PRUNE="${PRUNE:-1}"                 # 1 = drop previously-added links that went dead
HTTP_TIMEOUT="${HTTP_TIMEOUT:-20}"
RUN_ONCE="${RUN_ONCE:-0}"
UA="${UA:-Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0 Safari/537.36}"

# Slug used in generated source names; must satisfy the config name regex
# ^[a-z0-9-]+$ enforced by SubscriptionsConfig.Validate.
SLUG="$(printf '%s' "$CHANNEL" | tr '[:upper:]' '[:lower:]' | tr -cs 'a-z0-9' '-' | sed 's/^-//; s/-$//')"

log() { echo "[tg-crawler] $*"; }

# ensure_private makes sure private.yaml exists so yq can read it. yq's `+=`
# auto-creates the subscriptions.sources path, and an existing file (with any
# real private sources) is left untouched here.
ensure_private() {
    if [ ! -f "$PRIVATE" ]; then
        printf 'subscriptions:\n  sources: []\n' > "$PRIVATE"
        log "created $PRIVATE"
    fi
}

# crawl prints every is.wepogp.gay subscription URL found across PAGES pages,
# one per line (deduped by the caller).
crawl() {
    local before="" url page ids
    local i=0
    while [ "$i" -lt "$PAGES" ]; do
        i=$((i + 1))
        url="https://t.me/s/${CHANNEL}"
        [ -n "$before" ] && url="${url}?before=${before}"
        page="$(curl -fsS --max-time "$HTTP_TIMEOUT" -A "$UA" "$url" 2>/dev/null || true)"
        [ -n "$page" ] || break
        # These links are posted as plain text inside <pre> blocks (Telegram does
        # not linkify them), so match the URL directly with a strict URL charset
        # that stops at '<', quotes, whitespace, and any truncation ellipsis.
        printf '%s' "$page" \
            | grep -oE 'https://is\.wepogp\.gay/[A-Za-z0-9._~:/?#@!$&*+,;=%-]+' \
            | sed 's/&amp;/\&/g' || true
        # Oldest message id on this page = cursor for the next older page.
        ids="$(printf '%s' "$page" | grep -oE 'data-post="[^"]+/[0-9]+"' | sed -E 's#.*/([0-9]+)"#\1#' | sort -n | head -1 || true)"
        [ -n "$ids" ] || break
        before="$ids"
    done
}

# validate_url returns 0 if the URL is a live subscription (HTTP 200, non-empty
# body, not expired per the subscription-userinfo header), 1 otherwise.
validate_url() {
    local u="$1" body hdr code exp now
    body="$(mktemp)"
    hdr="$(curl -fsS -D - -o "$body" --max-time "$HTTP_TIMEOUT" -A "$UA" "$u" 2>/dev/null || true)"
    if [ ! -s "$body" ]; then rm -f "$body"; return 1; fi
    rm -f "$body"
    code="$(printf '%s\n' "$hdr" | awk 'toupper($1) ~ /^HTTP/ {c=$2} END {print c}')"
    [ "$code" = "200" ] || return 1
    exp="$(printf '%s\n' "$hdr" | grep -io 'expire=[0-9]\+' | head -1 | cut -d= -f2 || true)"
    now="$(date +%s)"
    if [ -n "$exp" ] && [ "$exp" -gt 0 ] && [ "$exp" -lt "$now" ]; then return 1; fi
    return 0
}

name_for() {
    local u="$1" sha
    sha="$(printf '%s' "$u" | sha256sum | cut -c1-10)"
    printf 'tg-%s-%s' "$SLUG" "$sha"
}

add_new() {
    local url name added=0
    while IFS= read -r url; do
        [ -n "$url" ] || continue
        case "$url" in *payload=*) ;; *) continue ;; esac
        name="$(name_for "$url")"
        if grep -qF "$name" "$PRIVATE" 2>/dev/null; then
            continue
        fi
        if ! validate_url "$url"; then
            log "skip (dead/expired): $name"
            continue
        fi
        NAME="$name" URL="$url" yq e \
            '.subscriptions.sources += [{"name": strenv(NAME), "url": strenv(URL)}]' \
            "$PRIVATE" > "$PRIVATE.tmp" && mv "$PRIVATE.tmp" "$PRIVATE"
        log "ADDED $name"
        added=$((added + 1))
    done < <(crawl | sort -u)
    log "cycle: added $added new source(s)"
}

prune_dead() {
    [ "$PRUNE" = "1" ] || return 0
    local name url removed=0
    while IFS=$'\t' read -r name url; do
        [ -n "$name" ] || continue
        if ! validate_url "$url"; then
            NAME="$name" yq e -i \
                'del(.subscriptions.sources[] | select(.name == strenv(NAME)))' "$PRIVATE"
            log "PRUNED $name"
            removed=$((removed + 1))
        fi
    done < <(yq e '.subscriptions.sources[] | select(.name | test("^tg-")) | .name + "\t" + .url' "$PRIVATE" 2>/dev/null || true)
    [ "$removed" -gt 0 ] && log "cycle: pruned $removed dead source(s)" || true
}

cycle() {
    ensure_private
    add_new
    prune_dead
}

log "starting: channel=$CHANNEL private=$PRIVATE interval=${INTERVAL}s pages=$PAGES prune=$PRUNE"
if [ "$RUN_ONCE" = "1" ]; then
    cycle
    exit 0
fi
while true; do
    cycle || log "cycle error (continuing)"
    sleep "$INTERVAL"
done
