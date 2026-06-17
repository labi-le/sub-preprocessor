#!/usr/bin/env bash
set -euo pipefail

CONFIG="${CONFIG:-/config.yaml}"
ASN_SCRIPT="${ASN_SCRIPT:-/scripts/asn.sh}"
INTERVAL="${INTERVAL:-60}"
GEMINI_API_KEY="${GEMINI_API_KEY:-}"

if [ -z "$GEMINI_API_KEY" ]; then
    echo "[asn-block-check] FATAL: GEMINI_API_KEY is not set"
    exit 1
fi

echo "[asn-block-check] starting, config=$CONFIG, interval=${INTERVAL}s"

while true; do
    IP=$(curl -sS --max-time 10 ifconfig.me 2>/dev/null || true)
    if [ -z "$IP" ]; then
        echo "[asn-block-check] failed to get public IP, retrying in ${INTERVAL}s"
        sleep "$INTERVAL"
        continue
    fi
    echo "[asn-block-check] IP: $IP"

    # Probe Gemini API — returns JSON error when location is unsupported
    GEMINI_RESP=$(curl -sS --max-time 15 \
        "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.0-flash?key=${GEMINI_API_KEY}" \
        2>/dev/null || true)

    echo "[asn-block-check] API response: $(echo "$GEMINI_RESP" | tr '\n' ' ')"

    if echo "$GEMINI_RESP" | grep -q 'User location is not supported for the API use'; then
        echo "[asn-block-check] BLOCKED — location unsupported"

        if [ ! -x "$ASN_SCRIPT" ]; then
            echo "[asn-block-check] asn.sh not found at $ASN_SCRIPT, skipping"
            sleep "$INTERVAL"
            continue
        fi

        ASN_INFO=$("$ASN_SCRIPT" "$IP" 2>/dev/null || true)
        echo "$ASN_INFO"

        AS_NAME=$(echo "$ASN_INFO" | sed -n 's/.*"as_name_short": "\([^"]*\)".*/\1/p')
        if [ -z "$AS_NAME" ]; then
            echo "[asn-block-check] could not parse AS name"
            sleep "$INTERVAL"
            continue
        fi
        echo "[asn-block-check] AS name: $AS_NAME"

        # Check if pattern already in config
        if grep -qiF "$AS_NAME" "$CONFIG" 2>/dev/null; then
            echo "[asn-block-check] AS $AS_NAME already in config deny_patterns"
            sleep "$INTERVAL"
            continue
        fi

        # style="double" keeps regex metacharacters in the pattern as a valid quoted YAML scalar
        PATTERN="(?i)$AS_NAME"
        if yq eval -i "(.asn.deny_patterns += [\"$PATTERN\"]) | .asn.deny_patterns[-1] style=\"double\"" "$CONFIG" 2>/dev/null; then
            echo "[asn-block-check] ADDED deny_pattern: $PATTERN"
        else
            echo "[asn-block-check] failed to update config (yq missing or config not writable)"
        fi
    else
        echo "[asn-block-check] OK — Gemini API accessible"
    fi

    sleep "$INTERVAL"
done
