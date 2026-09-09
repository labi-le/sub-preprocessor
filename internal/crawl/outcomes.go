package crawl

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"
)

// sourceOutcome is one source's share of the last published stable cycle as
// the service's metrics exposition reports it.
type sourceOutcome struct {
	Survivors int // stable_source_tested_nodes
	Valid     int // stable_source_valid_nodes
}

// outcomeReading is a parsed snapshot of the service's per-source outcomes.
// Sources is keyed by the source NAME (the metric's source= label); a source
// absent from it simply has no observation this cycle.
type outcomeReading struct {
	// Published is stable_last_success_timestamp_seconds: the unix time of the
	// last cycle that PUBLISHED a list. It is the fold's clock rather than
	// stable_cycles_total because the service counts every attempt in that
	// total — a probe failure, a cancelled cycle, a cycle that merged nothing
	// all bump it (metrics.ObserveError) — while the per-source gauges render
	// from the last PUBLISHED report and do not move. Folding on the attempt
	// counter would therefore read one genuine survivor-free report six times
	// and condemn on five re-renders of it. This timestamp advances exactly
	// when those rows are replaced (metrics.Observe), which is the only moment
	// a new observation exists. It also survives a service restart honestly:
	// wall time only goes forward, where the in-memory attempt counter resets
	// to zero and would stall every record until it re-lapped.
	Published uint64
	Sources   map[string]sourceOutcome // keyed by the source NAME (the metric's source= label)
}

// Timeout, body and line bounds for the outcomes scrape. The exposition is
// ~700 KB and renders ~4600 series; the cap exists so a wedged or exploding
// endpoint cannot pin a crawl cycle, and one extra byte beyond the cap is read
// so a truncated body is detected instead of folded as if complete.
const (
	outcomesTimeout = 10 * time.Second
	outcomesMaxBody = 8 << 20
	outcomesMaxRead = outcomesMaxBody + 1
	// outcomesLineBuf starts the scanner above every real line (the longest
	// HELP text is a few hundred bytes); outcomesMaxLine turns a pathological
	// line into a loud scan error instead of a truncated tail.
	outcomesLineBuf = 16 << 10
	outcomesMaxLine = 1 << 20
	// outcomesSourceHint clears the 1146 source entries a prod reading held on
	// 2026-09-09 (12 704 sample lines, 11 series per source): a hint under the
	// real count costs two map growths mid-parse, one over it costs buckets.
	outcomesSourceHint = 1024
	// outcomesCountCeil/outcomesClockCeil keep a NaN, negative or overflowing
	// value off the fold: int/uint conversion of such a float is not defined.
	outcomesCountCeil = 1 << 63
	outcomesClockCeil = 1 << 64
)

// Sentinels fetchOutcomes distinguishes. The caller's fail-safe treats every
// error as "withdraw nothing"; errOutcomesNotService additionally means the
// endpoint answered 2xx with a body that is not this service's exposition.
var (
	errOutcomesStatus     = errors.New("outcomes: non-2xx status")
	errOutcomesNotService = errors.New("outcomes: body carries none of the three stable_* families")
)

// The three families fetchOutcomes reads, each with the byte that terminates
// the metric name — a space for the label-less clock, '{' for the labeled
// ones — so a longer name sharing the prefix is not misread.
var (
	publishedFamily = []byte("stable_last_success_timestamp_seconds ")
	testedFamily    = []byte("stable_source_tested_nodes{")
	validFamily     = []byte("stable_source_valid_nodes{")
	sourceLabel     = []byte("source")
)

// fetchOutcomes reads the service's per-source outcomes from url and folds
// the three stable_* families into a reading. It never returns a partial
// reading: a transport error, a non-2xx status, a body over outcomesMaxBody,
// or a scan error returns the zero reading, and a 2xx body that parses but
// carries none of the three families returns errOutcomesNotService.
func fetchOutcomes(ctx context.Context, client *http.Client, url string) (outcomeReading, error) {
	ctx, cancel := context.WithTimeout(ctx, outcomesTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return outcomeReading{}, fmt.Errorf("build outcomes request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return outcomeReading{}, fmt.Errorf("fetch outcomes: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return outcomeReading{}, fmt.Errorf("%w: %s", errOutcomesStatus, resp.Status)
	}

	counter := &countingReader{r: resp.Body}
	sc := bufio.NewScanner(io.LimitReader(counter, outcomesMaxRead))
	sc.Buffer(make([]byte, 0, outcomesLineBuf), outcomesMaxLine)

	var (
		reading outcomeReading
		found   bool
	)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		if foldLine(&reading, line) {
			found = true
		}
	}
	if scanErr := sc.Err(); scanErr != nil {
		return outcomeReading{}, fmt.Errorf("read outcomes body: %w", scanErr)
	}
	if counter.n > outcomesMaxBody {
		return outcomeReading{}, fmt.Errorf("outcomes body exceeds %d bytes", outcomesMaxBody)
	}
	if !found {
		return outcomeReading{}, errOutcomesNotService
	}
	return reading, nil
}

// countingReader counts what it yields, letting fetchOutcomes tell a body cut
// short by outcomesMaxRead from one that ended on its own: io.LimitReader
// reports EOF either way, and a truncated exposition must not be folded.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err //nolint:wrapcheck // io.Reader contract: EOF and read errors surface unwrapped
}

// foldLine folds one exposition line into reading and reports whether it
// contributed. Lines of any other family are ignored, and a line that matches
// a family but parses malformed is skipped rather than failed.
func foldLine(reading *outcomeReading, line []byte) bool {
	switch {
	case bytes.HasPrefix(line, publishedFamily):
		v, ok := parsePublished(line[len(publishedFamily):])
		if !ok {
			return false
		}
		reading.Published = v
		return true
	case bytes.HasPrefix(line, testedFamily):
		return foldSample(reading, line[len(testedFamily):], true)
	case bytes.HasPrefix(line, validFamily):
		return foldSample(reading, line[len(validFamily):], false)
	}
	return false
}

// foldSample merges one stable_source_tested/valid_nodes line into reading;
// tested selects the family. It reports whether the line contributed, so one
// malformed line is skipped rather than sinking the reading.
func foldSample(reading *outcomeReading, rest []byte, tested bool) bool {
	name, value, ok := splitSourceSample(rest)
	if !ok {
		return false
	}
	n, ok := parseCount(value)
	if !ok {
		return false
	}
	if reading.Sources == nil {
		reading.Sources = make(map[string]sourceOutcome, outcomesSourceHint)
	}
	o := reading.Sources[name]
	if tested {
		o.Survivors = n
	} else {
		o.Valid = n
	}
	reading.Sources[name] = o
	return true
}

// splitSourceSample walks a source-family sample line — everything after the
// family name, starting at the first label — and returns the source= label
// value plus the value token after the closing brace. Labels are matched by
// name, never by position: the writer emits feed,owner,source, but label order
// is not part of the text format. Quoted values may contain ',', '}' or the
// three backslash escapes, so the walk honors quotes instead of splitting
// blindly.
func splitSourceSample(line []byte) (name string, value []byte, ok bool) {
	i := 0
	for {
		eq := bytes.IndexByte(line[i:], '=')
		if eq < 0 {
			return "", nil, false
		}
		eq += i
		open := eq + 1 // the value's opening quote
		if open >= len(line) || line[open] != '"' {
			return "", nil, false
		}
		start := open + 1
		end, escaped := labelValueEnd(line, start)
		if end < 0 {
			return "", nil, false
		}
		if bytes.Equal(line[i:eq], sourceLabel) {
			raw := line[start:end]
			if escaped {
				name = unescapeLabel(raw)
			} else {
				name = string(raw)
			}
		}
		i = end + 1
		if i >= len(line) {
			return "", nil, false
		}
		if line[i] == '}' {
			return name, line[i+1:], name != ""
		}
		if line[i] != ',' {
			return "", nil, false
		}
		i++
	}
}

// labelValueEnd scans a quoted label value whose first byte is line[start] and
// returns the index of its closing quote. A backslash skips the escaped byte,
// so an escaped quote does not end the value early; escaped reports whether
// any escape was seen (the value then needs unescaping).
func labelValueEnd(line []byte, start int) (end int, escaped bool) {
	for i := start; i < len(line); i++ {
		switch line[i] {
		case '\\':
			escaped = true
			i++ // the escaped byte belongs to the value
		case '"':
			return i, escaped
		}
	}
	return -1, escaped
}

// unescapeLabel decodes the escapes the writer emits — \\, \" and \n — back
// into the config key it escaped. Any other backslash pair is copied through
// untouched: only this service's writer is a producer this parser trusts.
func unescapeLabel(raw []byte) string {
	b := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' || i+1 >= len(raw) {
			b = append(b, raw[i])
			continue
		}
		switch raw[i+1] {
		case '\\':
			b = append(b, '\\')
		case '"':
			b = append(b, '"')
		case 'n':
			b = append(b, '\n')
		default:
			b = append(b, raw[i], raw[i+1])
		}
		i++
	}
	return string(b)
}

// parsePublished reads the value token of a
// stable_last_success_timestamp_seconds line and truncates it to whole
// seconds: the gauge is rendered as a float and the fold only compares it.
func parsePublished(rest []byte) (uint64, bool) {
	f, ok := parseFloatValue(rest)
	if !ok || math.IsNaN(f) || f < 0 || f >= outcomesClockCeil {
		return 0, false
	}
	return uint64(f), true
}

// parseCount reads the value token of a source-family sample line and
// truncates it to a node count.
func parseCount(rest []byte) (int, bool) {
	f, ok := parseFloatValue(rest)
	if !ok || math.IsNaN(f) || f < 0 || f >= outcomesCountCeil {
		return 0, false
	}
	return int(f), true
}

// parseFloatValue reads the first space-delimited token of rest, which is the
// sample value; any further token (the optional scrape timestamp) is ignored.
func parseFloatValue(rest []byte) (float64, bool) {
	rest = bytes.TrimLeft(rest, " \t")
	if len(rest) == 0 {
		return 0, false
	}
	if i := bytes.IndexAny(rest, " \t"); i >= 0 {
		rest = rest[:i]
	}
	f, err := strconv.ParseFloat(string(rest), 64)
	if err != nil {
		return 0, false
	}
	return f, true
}
