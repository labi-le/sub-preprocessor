package ghfind

import "time"

// Grid order below is a public contract of the cursor in find.go: it stores
// integer offsets into these lists in .crawler-state.json, so the order must
// not change between versions of the binary.

// codeSchemeLiterals are the URI schemes this pipeline can read, quoted so the
// ':' and '/' cannot parse as search qualifiers.
var codeSchemeLiterals = []string{
	`"vless://"`, `"vmess://"`, `"trojan://"`, `"hysteria2://"`, `"ss://"`, `"tuic://"`,
}

// codeShapePartitions are the file shapes the 2026-09-07 measurement found
// productive (docs/guides/github.md): .txt bodies are the live majority and
// larger ones live-ier; sub/config file names and sub paths carry the tokens
// the candidate scorer rewards. Yield order matters because the discovery loop
// walks the grid from the front, so the widest shapes come first.
var codeShapePartitions = []string{
	"extension:txt",
	"extension:txt size:>20000",
	"filename:sub",
	"filename:config",
	"path:sub",
}

// CodeQueries returns the code-search grid: every scheme crossed with every
// shape partition, partition-major so all schemes hit the highest-yield shape
// before the cursor moves on.
func CodeQueries() []string {
	grid := make([]string, 0, len(codeSchemeLiterals)*len(codeShapePartitions))
	for _, shape := range codeShapePartitions {
		for _, scheme := range codeSchemeLiterals {
			grid = append(grid, scheme+" "+shape+" in:file")
		}
	}
	return grid
}

var repoTopicGrid = []string{
	"vless", "v2ray-config", "free-vpn", "v2ray-subscription", "clash-subscription", "proxy-list",
}

var repoKeywordGrid = []string{
	"免费节点 订阅",
	"机场 订阅",
	"free vpn subscription link",
	"v2ray subscription free nodes",
	"proxy subscription collector",
}

// RepoQueries returns the repository-search grid: topic queries first, then
// the English and Chinese keyword terms that dominate this niche, every entry
// restricted to repositories pushed within fresh of now. The pushed date is
// rendered in UTC so one cutoff instant never yields two dates for the same
// grid entry.
func RepoQueries(now time.Time, fresh time.Duration) []string {
	filter := "pushed:>=" + now.Add(-fresh).UTC().Format("2006-01-02")
	grid := make([]string, 0, len(repoTopicGrid)+len(repoKeywordGrid))
	for _, topic := range repoTopicGrid {
		grid = append(grid, "topic:"+topic+" "+filter)
	}
	for _, keywords := range repoKeywordGrid {
		grid = append(grid, keywords+" "+filter)
	}
	return grid
}
