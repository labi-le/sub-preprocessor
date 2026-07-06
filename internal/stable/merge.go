package stable

import (
	"fmt"

	"domains.lst/sub-preprocessor/internal/subscription"
)

// SourceBody is a fetched (and geo-filtered) subscription payload.
type SourceBody struct {
	Name string
	Body []byte
}

// Entry is a merged node whose Raw URI is already relabeled to Label.
type Entry struct {
	Label string
	Raw   string
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
			seen[key] = struct{}{}
			kept++
			label := fmt.Sprintf("%s-%03d", src.Name, kept)
			raw := n.Raw
			if n.FragmentIdx >= 0 {
				raw = raw[:n.FragmentIdx]
			}
			entries = append(entries, Entry{Label: label, Raw: raw + "#" + label})
			return true
		})
	}
	return entries
}
