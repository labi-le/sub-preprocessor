// Package srcname owns the managed-source naming convention. It lives apart
// because the dependency can only point one way: internal/crawl mints the
// names, internal/metrics only renders them as labels, so the rule belongs
// below both rather than inside the minter. It imports no other internal
// package so it can stay there.
package srcname

import "strings"

// ManagedPrefix marks the sources the crawler owns; anything else is a
// hand-added subscription it never touches.
const ManagedPrefix = "tg-"

// shardLen is the length of the sha256-prefix tail in tg-<slug>-<sha6>.
const shardLen = 6

// Split reports the channel a source name attributes to, and whether the
// crawler minted it. The feed is a slice of name, never a new string and never
// empty: tg-genliberty-7e9b21 -> genliberty, tg-inline -> inline, the legacy
// hash-only tg-96c4d7c7a7 -> 96c4d7c7a7, curated flat447 -> flat447. A name
// that would strip to nothing is returned verbatim.
func Split(name string) (feed string, managed bool) {
	if !strings.HasPrefix(name, ManagedPrefix) {
		return name, false
	}
	feed = name[len(ManagedPrefix):]
	if len(feed) > shardLen+1 && feed[len(feed)-shardLen-1] == '-' && isLowerHex(feed[len(feed)-shardLen:]) {
		feed = feed[:len(feed)-shardLen-1]
	}
	if feed == "" {
		return name, true
	}
	return feed, true
}

// isLowerHex matches the mint's own alphabet: hex.EncodeToString emits lower
// case, so an upper-case tail is part of a channel slug, not a shard.
func isLowerHex(s string) bool {
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
