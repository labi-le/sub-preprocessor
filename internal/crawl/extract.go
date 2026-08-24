package crawl

import (
	"html"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"domains.lst/sub-preprocessor/internal/ioutil"
)

const (
	urlScheme = "https://"
	schemeSep = "://"
)

// stopByte ends a harvested URL or inline node: [\s"'<>] as Go's regexp reads
// \s, which excludes \v — a vertical tab stays inside the match.
var stopByte = [utf8.RuneSelf]bool{
	'\t': true, '\n': true, '\f': true, '\r': true, ' ': true,
	'"': true, '\'': true, '<': true, '>': true,
}

// extractURLs returns every https URL in an already-unescaped HTML page,
// stripped of trailing punctuation. Links appear both in href attributes and as
// plain text inside <pre> blocks, so it scans the whole page. Every result is a
// sub-slice of page, which is why each caller that keeps one copies it.
//
// Hand-scanned rather than matched by regexp: FindAllString allocates a []int
// per match attempt, 538419 of the 1876513 objects a BenchmarkHarvestPages
// alloc_objects profile sampled (29%, 2026-08-18), while the pattern is a
// literal prefix plus a negated class. TestExtractorsMatchTheirRegexps holds
// this scan equal to that pattern.
func extractURLs(page string) []string {
	return appendURLs(make([]string, 0, strings.Count(page, urlScheme)), page)
}

// appendURLs is extractURLs over a caller-owned slice: the harvest scans a page
// one message at a time and reuses one slice across them, so attributing URLs
// per message costs no allocation per message.
func appendURLs(dst []string, page string) []string {
	for pos := 0; ; {
		i := strings.Index(page[pos:], urlScheme)
		if i < 0 {
			return dst
		}
		body := pos + i + len(urlScheme)
		end := urlEnd(page, body)
		if end > body { // the class is one-or-more, so a bare scheme is no match
			dst = append(dst, strings.TrimRight(page[pos+i:end], trimSet))
		}
		pos = end
	}
}

// dataPost bounds one message: it is the only per-message identity a scraped
// page carries. pageCursor reads the same attribute off the RAW page, whose
// offsets — and, where a page escapes its own markup, whose attributes — are not
// those of the unescaped text the scanners are given, so attribution is computed
// here on the text extractURLs actually reads.
const dataPost = `data-post="`

// nextMessage splits text at the first message boundary: seg precedes it and so
// belongs to the message already being read, id is the boundary's message id,
// and tail resumes at the attribute's VALUE, so no byte extractURLs could
// harvest is skipped. A zero id ends the walk.
//
// A value that is not "<chat>/<digits>" is no boundary and stays inside seg —
// the shape cursorRe demands of the same attribute, except that postID also
// refuses leading-zero and over-wide digit runs where cursorRe accepts them.
func nextMessage(text string) (seg string, id uint64, tail string) {
	for pos := 0; ; {
		i := strings.Index(text[pos:], dataPost)
		if i < 0 {
			return text, 0, ""
		}
		val := pos + i + len(dataPost)
		q := strings.IndexByte(text[val:], '"')
		if q < 0 {
			return text, 0, ""
		}
		if post := postID(text[val : val+q]); post != 0 {
			return text[:pos+i], post, text[val:]
		}
		pos = val
	}
}

// postID returns the message id of a data-post value, or 0 for a value that
// carries none. Taking the LAST segment mirrors cursorRe's greedy
// `[^"]+/(\d+)`, so a forum's three-segment form yields its message id rather
// than its topic.
//
// Deliberately narrower than the digit run cursorRe accepts: a leading zero and
// a run too wide for uint64 are refused as no boundary, exactly as a
// non-numeric tail is. What stays injective is the digit run onto its uint64,
// not the value onto the name: the last-segment rule above gives "chan/12" and
// "chat/7/12" the same id, which mergeManaged's used set absorbs. The refusals
// stop "chan/007" minting the chan-7 that "chan/7" already owns. Telegram
// emits neither shape; a hostile page can. ParseUint at base 10 takes no sign
// and no underscore, so it is the whole of the digit check as well as the
// conversion — one pass over the run.
func postID(val string) uint64 {
	i := strings.LastIndexByte(val, '/')
	if i <= 0 {
		return 0
	}
	digits := val[i+1:]
	if digits == "" || digits[0] == '0' {
		return 0
	}
	id, err := strconv.ParseUint(digits, decimalBase, 64)
	if err != nil {
		return 0
	}
	return id
}

// extractInlineNodes returns every raw proxy URI pasted directly in a channel
// page, stripped of trailing punctuation. Unlike extractURLs these are node
// URIs, not subscription links, and the caller has already unescaped page.
// Results are sub-slices of page; see extractURLs on the scan.
func extractInlineNodes(page string) []string {
	out := make([]string, 0, strings.Count(page, schemeSep))
	for pos := 0; ; {
		i := strings.Index(page[pos:], schemeSep)
		if i < 0 {
			return out
		}
		sep := pos + i
		body := sep + len(schemeSep)
		end := inlineEnd(page, body)
		if start := wordStart(page, sep); end > body && isInlineScheme(page[start:sep]) {
			out = append(out, strings.TrimRight(page[start:end], trimSet))
			pos = end
			continue
		}
		pos = body
	}
}

// appendInlineNodes appends every proxy URI in text to dst, copied into one
// buffer for the whole page. The copy is what lets harvestPages reuse a single
// unescape scratch, and it removes the pin the accepted candidate key already
// clones away: a node URI joins the cycle-wide accumulator, where a sub-slice
// would hold its whole page (up to maxPageBytes) until buildInlineSource runs.
func appendInlineNodes(dst []string, text string) []string {
	nodes := extractInlineNodes(text)
	total := 0
	for _, n := range nodes {
		total += len(n)
	}
	if total == 0 {
		return dst
	}
	// cap is the exact total, so no append moves buf and invalidates a view
	// already handed out, and nothing writes to buf once its view is taken.
	buf := make([]byte, 0, total)
	for _, n := range nodes {
		off := len(buf)
		buf = append(buf, n...)
		dst = append(dst, ioutil.UnsafeString(buf[off:]))
	}
	return dst
}

// unescapeInto returns page with its HTML entities decoded, copying into buf
// only when page holds an '&'. The grown buf comes back for the next page to
// reuse and the returned string aliases it, so that string is valid only until
// the next call — html.UnescapeString instead allocates two copies of every
// page it touches, 92% of BenchmarkHarvestPages/inline's 691746 B/op
// (2026-08-18).
func unescapeInto(buf []byte, page string) (string, []byte) {
	i := strings.IndexByte(page, '&')
	if i < 0 {
		return page, buf
	}
	if cap(buf) < len(page) {
		// Decoding only ever shortens, so one buffer of the page's size is the
		// whole call's allocation; growing it by append cost a copy per doubling.
		buf = make([]byte, 0, len(page))
	}
	buf = append(buf[:0], page[:i]...)
	for rest := page[i:]; len(rest) > 0; {
		if rest[0] != '&' {
			j := strings.IndexByte(rest, '&')
			if j < 0 {
				buf = append(buf, rest...)
				break
			}
			buf = append(buf, rest[:j]...)
			rest = rest[j:]
			continue
		}
		ref := rest[:entityEnd(rest)]
		if r, ok := knownEntity(ref); ok {
			buf = utf8.AppendRune(buf, r)
		} else {
			buf = append(buf, html.UnescapeString(ref)...)
		}
		rest = rest[len(ref):]
	}
	return ioutil.UnsafeString(buf), buf
}

// entityEnd returns the length of the entity reference at the start of s
// (s[0] is '&'): every byte html's unescapeEntity can consume, its name run
// plus a closing ';'. '#' is in the run so a numeric reference is one window.
// html reads nothing past what it consumes (only attribute mode does, and that
// is html.UnescapeString's off case) and copies the window's tail verbatim, so
// a window decodes exactly as it would inside the whole page.
func entityEnd(s string) int {
	i := 1
	for i < len(s) {
		c := s[i]
		if c == '#' || '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' {
			i++
			continue
		}
		if c == ';' {
			i++
		}
		break
	}
	return i
}

// knownEntity decodes the references a t.me page is made of, so an ordinary page
// costs no call into html and no allocation. The two numeric ones are here
// because html.EscapeString emits them; every other numeric reference falls
// back, its windows-1252 and invalid-codepoint rules being html's to own.
func knownEntity(ref string) (rune, bool) {
	switch ref {
	case "&amp;":
		return '&', true
	case "&lt;":
		return '<', true
	case "&gt;":
		return '>', true
	case "&quot;":
		return '"', true
	case "&#34;":
		return '"', true
	case "&#39;":
		return '\'', true
	case "&apos;":
		return '\'', true
	case "&nbsp;":
		return '\u00a0', true
	}
	return 0, false
}

// urlEnd returns the end of the [^\s"'<>\p{Z}]+ run at i. \p{Z} is what keeps an
// unescaped &nbsp; beside a link out of the URL, so a non-ASCII byte costs a
// rune decode.
func urlEnd(page string, i int) int {
	for i < len(page) {
		c := page[i]
		if c < utf8.RuneSelf {
			if stopByte[c] {
				break
			}
			i++
			continue
		}
		r, n := utf8.DecodeRuneInString(page[i:])
		if unicode.Is(unicode.Z, r) {
			break
		}
		i += n
	}
	return i
}

// inlineEnd returns the end of the [^\s"'<>]+ run at i. There is no \p{Z} in an
// inline node's class, so no byte above ASCII ever ends one.
func inlineEnd(page string, i int) int {
	for i < len(page) {
		if c := page[i]; c < utf8.RuneSelf && stopByte[c] {
			break
		}
		i++
	}
	return i
}

// wordStart returns the start of the ASCII word run ending at end, where the \b
// in front of an inline scheme has to sit: a scheme preceded by a word character
// is a substring of a longer token, not a URI.
func wordStart(page string, end int) int {
	i := end
	for i > 0 {
		c := page[i-1]
		if c == '_' || '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' {
			i--
			continue
		}
		break
	}
	return i
}

// isInlineScheme reports whether s is a proxy scheme worth harvesting.
// http/https/socks* are absent on purpose: parseNode rejects only the PORTLESS
// form, so a pasted "https://example.com:8443/docs" IS a valid node, and
// harvesting those would turn every documentation link a channel posts into a
// proxy.
func isInlineScheme(s string) bool {
	switch s {
	case "vless", "vmess", "ss", "ssr", "trojan", "tuic", "hysteria", "hysteria2", "hy2", "anytls", "mierus":
		return true
	}
	return false
}
