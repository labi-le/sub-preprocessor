package stable

import (
	"fmt"

	"domains.lst/sub-preprocessor/internal/rewrite"
	"domains.lst/sub-preprocessor/internal/subscription"
)

// SourceBody is a fetched (and geo-filtered) subscription payload.
type SourceBody struct {
	Name string
	Body []byte
}

// Entry is a merged node. Raw carries the clean <source>-NNN name used for
// probing; Tagged carries the published name (the [GEO][IP] annotation from the
// filter pass, when present, plus the same unique label).
type Entry struct {
	Label  string
	Raw    string
	Tagged string
	Addr   string // server:port, the dead-cache key
}

// Merge parses all source bodies in order, dedupes nodes by Server:Port
// (first source wins) and relabels each kept node to <source>-NNN so probe
// results map back to entries unambiguously. NNN counts kept nodes per source.
func Merge(bodies []SourceBody) []Entry {
	seen := make(map[string]struct{})
	var entries []Entry
	for _, src := range bodies {
		kept := 0
		subscription.Parse(src.Body, func(n subscription.Node) bool {
			key := n.Server + ":" + n.Port
			if _, dup := seen[key]; dup {
				return true
			}
			label := fmt.Sprintf("%s-%03d", src.Name, kept+1)
			raw, ok := relabelNode(n, label)
			if !ok {
				return true
			}
			tagged := raw
			if tags := rewrite.LeadingTags(n.Name); tags != "" {
				if t, tok := relabelNode(n, tags+" "+label); tok {
					tagged = t
				}
			}
			seen[key] = struct{}{}
			kept++
			entries = append(entries, Entry{Label: label, Raw: raw, Tagged: tagged, Addr: key})
			return true
		})
	}
	return entries
}

// relabelNode rewrites a node's display name to label so probe results map
// back to entries. vmess names live in the base64 JSON ps field; every other
// scheme uses a URI #fragment.
func relabelNode(n subscription.Node, label string) (string, bool) {
	if n.Scheme == subscription.SchemeVmess {
		return subscription.RewriteVmessName(n.Raw, label)
	}
	raw := n.Raw
	if n.FragmentIdx >= 0 {
		raw = raw[:n.FragmentIdx]
	}
	return raw + "#" + label, true
}
