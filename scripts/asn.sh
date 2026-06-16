#!/usr/bin/env bash
set -euo pipefail

ip="$1"
rev=$(echo "$ip" | awk -F. '{print $4"."$3"."$2"."$1}')

origin=$(dig +short TXT "${rev}.origin.asn.cymru.com" | tr -d '"')
asn=$(echo "$origin" | awk -F'|' '{gsub(/ /,""); print $1}')
as=$(dig +short TXT "AS${asn}.asn.cymru.com" | tr -d '"')
name=$(echo "$as" | awk -F'|' '{print $5}' | sed 's/^[[:space:]]*//')

echo "IP:      $ip"
echo "Origin:  $origin"
echo "AS:      AS${asn} — ${name}"
