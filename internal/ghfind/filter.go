package ghfind

import (
	"bytes"
	"cmp"
	"slices"

	"domains.lst/sub-preprocessor/internal/subscription"
)

// Scored is one blob that passed the candidate filter; Score is the measured
// calibration of docs/guides/github.md, whose numbers this file encodes.
type Scored struct {
	Path  string
	Size  int
	Score int
}

// The refusal and scoring constants are the calibrated filter of
// docs/guides/github.md; every live rate cited here was measured 2026-09-07
// over 2655 sampled candidate files.
const sizeFloor = 2 << 10 // below this, files are 9% live and need scoreWaive

const (
	sizeTiny  = iota // < sizeFloor (9% live)
	sizeSmall        // sizeFloor to 20 KB (31%)
	sizeMid          // 20 KB to 200 KB (68%)
	sizeBig          // >= 200 KB (77%)
)

const (
	size20KB  = 20 << 10
	size200KB = 200 << 10
)

const (
	scoreToken = 2 // one name/path keyword hit
	scoreWaive = 7 // the measured 85-90% live band; waives sizeFloor
)

// candidatesHeadroom doubles the preallocation over the limit, so a tree
// whose passing blobs outnumber the fetch budget grows only when it is
// unusually rich.
const candidatesHeadroom = 2

// Extension score classes. .yaml and .yml are absent on purpose: their
// exclusion is the single most valuable filter the measurement found (0-2%
// live over 238 files), because subscription.Normalize cannot read Clash
// YAML. .json measured 0% too but stays admissible at the lowest score: the
// probe could not see the xray-shaped JSON classify.Body can.
const (
	extTxt   = 3 // .txt: 78% live
	extPlain = 2 // extension-less (38%) and the conf/list/sub/base64 group
	extJSON  = 1 // .json
)

// refusedDirSegments are directory names that mark build, doc, vendoring or
// tooling material; any one of them on a blob's path refuses it. The pairs
// cover both spellings the guide measured.
var refusedDirSegments = [][]byte{
	[]byte(".github"), []byte(".git"), []byte("docs"),
	[]byte("test"), []byte("tests"),
	[]byte("vendor"), []byte("node_modules"), []byte("src"),
	[]byte("assets"), []byte("img"), []byte("images"),
	[]byte(".idea"), []byte(".vscode"),
	[]byte("example"), []byte("examples"),
	[]byte("script"), []byte("scripts"),
	[]byte("bin"), []byte("build"), []byte("dist"),
}

// badNameStems are file names that are plainly not subscription payloads: a
// repository README, a license or a package manifest. "package" is refused
// as a prefix because package.json and package-lock.json must both die.
var badNameStems = [][]byte{
	[]byte("readme"), []byte("license"), []byte("requirements"),
	[]byte("dockerfile"), []byte("go"),
}

var packagePrefix = []byte("package")

// subKeywords are the name/path tokens the measurement found on live files.
// None is a substring of another, so a path scores each keyword at most once
// and the count stays interpretable.
var subKeywords = [][]byte{
	[]byte("v2ray"), []byte("vless"), []byte("vmess"), []byte("trojan"),
	[]byte("hysteria"), []byte("hy2"), []byte("ssr"), []byte("sub"),
	[]byte("config"), []byte("proxy"), []byte("node"), []byte("free"),
}

var (
	txtExt    = []byte("txt")
	jsonExt   = []byte("json")
	confExt   = []byte("conf")
	listExt   = []byte("list")
	subExt    = []byte("sub")
	base64Ext = []byte("base64")
)

// Candidates keeps the blobs of one repository tree that are worth fetching —
// decodable extension, plausible size, no refused directory — ordered by the
// calibrated Score then Size (both descending) and capped at limit. Matching
// folds each path's ASCII case once into a reusable buffer, so no regexp is
// compiled or run and nothing allocates per tree entry; the returned slice
// is sorted in place.
func Candidates(tree []TreeEntry, limit int) []Scored {
	if limit <= 0 {
		return nil
	}
	out := make([]Scored, 0, min(limit*candidatesHeadroom, len(tree)))
	var fold []byte
	for _, e := range tree {
		if !e.Blob || e.Size <= 0 || e.Size > subscription.MaxSubscriptionSize {
			continue
		}
		fold = asciiFold(fold, e.Path)
		name := fold[bytes.LastIndexByte(fold, '/')+1:]
		if len(name) == 0 || name[0] == '.' || dirsRefused(fold[:len(fold)-len(name)]) {
			continue
		}
		stem, ext := splitName(name)
		if extScore(ext) < 0 || nameRefused(stem) {
			continue
		}
		score := extScore(ext) + sizeBucket(e.Size) + scoreToken*keywordHits(fold)
		if e.Size < sizeFloor && score < scoreWaive {
			continue
		}
		out = append(out, Scored{Path: e.Path, Size: e.Size, Score: score})
	}
	slices.SortFunc(out, func(a, b Scored) int {
		if c := cmp.Compare(a.Score, b.Score); c != 0 {
			return -c
		}
		if c := cmp.Compare(a.Size, b.Size); c != 0 {
			return -c
		}
		return cmp.Compare(a.Path, b.Path)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// asciiFold lowercases the ASCII letters of s into dst, growing dst when the
// previous longest path no longer fits, and returns the folded slice. The
// buffer is shared across the entries of one Candidates call, so folding
// allocates once per call, not once per entry; non-ASCII bytes pass through.
func asciiFold(dst []byte, s string) []byte {
	if cap(dst) < len(s) {
		dst = make([]byte, len(s))
	}
	dst = dst[:len(s)]
	for i := range len(s) {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		dst[i] = c
	}
	return dst
}

// dirsRefused checks the directory part of a path only: dirs runs through the
// last '/' (empty for a repository-root file).
func dirsRefused(dirs []byte) bool {
	start := 0
	for i := 0; i <= len(dirs); i++ {
		if i < len(dirs) && dirs[i] != '/' {
			continue
		}
		seg := dirs[start:i]
		for _, d := range refusedDirSegments {
			if bytes.Equal(seg, d) {
				return true
			}
		}
		start = i + 1
	}
	return false
}

// splitName keeps the stem and extension of the folded basename. A leading
// dot cannot reach it (dotfiles are refused upstream); a trailing dot is an
// empty extension, the extension-less class.
func splitName(name []byte) (stem, ext []byte) {
	dot := bytes.LastIndexByte(name, '.')
	if dot < 0 {
		return name, nil
	}
	return name[:dot], name[dot+1:]
}

// extScore scores an extension without its dot; -1 refuses the extension.
func extScore(ext []byte) int {
	if len(ext) == 0 {
		return extPlain
	}
	switch {
	case bytes.Equal(ext, txtExt):
		return extTxt
	case bytes.Equal(ext, jsonExt):
		return extJSON
	case bytes.Equal(ext, confExt), bytes.Equal(ext, listExt),
		bytes.Equal(ext, subExt), bytes.Equal(ext, base64Ext):
		return extPlain
	default:
		return -1
	}
}

func nameRefused(stem []byte) bool {
	for _, bad := range badNameStems {
		if bytes.Equal(stem, bad) {
			return true
		}
	}
	return bytes.HasPrefix(stem, packagePrefix)
}

// keywordHits counts each keyword at most once, directories included, so a
// path repeating one word scores it a single time.
func keywordHits(fold []byte) int {
	hits := 0
	for _, kw := range subKeywords {
		if bytes.Contains(fold, kw) {
			hits++
		}
	}
	return hits
}

func sizeBucket(size int) int {
	switch {
	case size >= size200KB:
		return sizeBig
	case size >= size20KB:
		return sizeMid
	case size >= sizeFloor:
		return sizeSmall
	default:
		return sizeTiny
	}
}
