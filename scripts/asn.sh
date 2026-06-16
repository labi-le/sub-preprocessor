#!/usr/bin/env bash
set -euo pipefail

ip="$1"
rev=$(echo "$ip" | awk -F. '{print $4"."$3"."$2"."$1}')

origin=$(dig +short TXT "${rev}.origin.asn.cymru.com" | tr -d '"')
asn=$(echo "$origin" | awk -F'|' '{gsub(/ /,""); print $1}')
as=$(dig +short TXT "AS${asn}.asn.cymru.com" | tr -d '"')
name_full=$(echo "$as" | awk -F'|' '{print $5}' | sed 's/^[[:space:]]*//')
name_short=$(echo "$name_full" | awk '{print $1}')

cat <<EOF
{
  "ip": "$ip",
  "asn": "$asn",
  "as_name": "$name_full",
  "as_name_short": "$name_short",
  "origin": "$origin"
}
EOF
