package crawl //nolint:testpackage // exercises the unexported fetchOutcomes contract

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// outcomeFixture serves body with status and runs fetchOutcomes against it.
func outcomeFixture(t *testing.T, body string, status int) (outcomeReading, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return fetchOutcomes(t.Context(), srv.Client(), srv.URL)
}

// failingTransport is a RoundTripper that always fails, standing in for a
// refused connection without touching the network.
type failingTransport struct{ err error }

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, f.err
}

// outcomeExposition is the shape the service renders: HELP/TYPE lines, the
// attempt counters (which must NOT drive the fold — stable_cycles_total rises
// on failed cycles too), the publish clock (stable_last_success_timestamp_seconds,
// which the fold reads), other families interleaved, and the per-source
// families with labels in the writer's feed,owner,source order. The decoy
// lines share a family name's prefix and must not be read as the family.
const outcomeExposition = `# HELP stable_cycles_total Stable cycles attempted (published + failed).
# TYPE stable_cycles_total counter
stable_cycles_total 4211
# HELP stable_cycle_failures_total Stable cycles that did not publish a new list.
# TYPE stable_cycle_failures_total counter
stable_cycle_failures_total 19
# HELP stable_sources_ok Sources that returned a usable body last cycle.
# TYPE stable_sources_ok gauge
stable_sources_ok 8
# HELP stable_filter_kept_nodes Survivors kept by each through-node filter.
# TYPE stable_filter_kept_nodes gauge
stable_filter_kept_nodes{filter="bandwidth"} 700
stable_filter_kept_nodes{filter="gemini"} 700
# HELP stable_source_nodes_total Nodes each source yielded before filtering last cycle.
# TYPE stable_source_nodes_total gauge
stable_source_nodes_total{feed="genliberty",owner="crawler",source="genliberty-3631"} 412
stable_source_nodes_total{feed="96c4d7c7a7",owner="crawler",source="96c4d7c7a7"} 1
# HELP stable_source_valid_nodes Nodes each source contributed after preprocess filtering.
# TYPE stable_source_valid_nodes gauge
stable_source_valid_nodes{feed="genliberty",owner="crawler",source="genliberty-3631"} 388
stable_source_valid_nodes{feed="96c4d7c7a7",owner="crawler",source="96c4d7c7a7"} 1 1757000000
stable_source_valid_nodes{feed="flat447",owner="curated",source="flat447"} 8000
stable_source_valid_nodes{feed="esc\"aped,feed",owner="curated",source="with-escaping-1"} 4
# HELP stable_source_tested_nodes Nodes each source contributed that survived the URL test.
# TYPE stable_source_tested_nodes gauge
stable_source_tested_nodes{feed="genliberty",owner="crawler",source="genliberty-3631"} 201
stable_source_tested_nodes{feed="96c4d7c7a7",owner="crawler",source="96c4d7c7a7"} 1
stable_source_tested_nodes{feed="flat447",owner="curated",source="flat447"} 1.234567e+06
stable_source_tested_nodes{feed="esc\"aped,feed",owner="curated",source="with-escaping-1"} 3
# HELP stable_source_published_nodes Nodes each source contributed that survived every through-node filter.
# TYPE stable_source_published_nodes gauge
stable_source_published_nodes{feed="genliberty",owner="crawler",source="genliberty-3631"} 96
# HELP stable_last_success_timestamp_seconds Unix time of the last published cycle.
# TYPE stable_last_success_timestamp_seconds gauge
stable_last_success_timestamp_seconds 1.755474123e+09
stable_source_valid_nodes_total{feed="decoy",owner="curated",source="decoy-family"} 99
stable_last_success_timestamp_seconds_extra 5
`

func TestFetchOutcomesReadsTheExposition(t *testing.T) {
	t.Parallel()

	reading, err := outcomeFixture(t, outcomeExposition, http.StatusOK)
	if err != nil {
		t.Fatalf("fetchOutcomes: %v", err)
	}
	want := outcomeReading{
		Published: 1755474123,
		Sources: map[string]sourceOutcome{
			"genliberty-3631": {Survivors: 201, Valid: 388},
			"96c4d7c7a7":      {Survivors: 1, Valid: 1},
			"flat447":         {Survivors: 1234567, Valid: 8000},
			"with-escaping-1": {Survivors: 3, Valid: 4},
		},
	}
	if !reflect.DeepEqual(reading, want) {
		t.Fatalf("reading = %+v, want %+v", reading, want)
	}
}

func TestFetchOutcomesKeepsDashedMintedName(t *testing.T) {
	t.Parallel()

	body := `# TYPE stable_source_tested_nodes gauge
stable_source_tested_nodes{feed="gh-morpheusadam-v2ray-config-2",owner="crawler",source="gh-morpheusadam-v2ray-config-2"} 5
stable_source_valid_nodes{feed="gh-morpheusadam-v2ray-config-2",owner="crawler",source="gh-morpheusadam-v2ray-config-2"} 2
`
	reading, err := outcomeFixture(t, body, http.StatusOK)
	if err != nil {
		t.Fatalf("fetchOutcomes: %v", err)
	}
	want := outcomeReading{
		Sources: map[string]sourceOutcome{
			"gh-morpheusadam-v2ray-config-2": {Survivors: 5, Valid: 2},
		},
	}
	if !reflect.DeepEqual(reading, want) {
		t.Fatalf("reading = %+v, want %+v", reading, want)
	}
}

// TestFetchOutcomesFindsSourceByLabelName: label order is not part of the text
// format, so a source line whose labels do not end in source= must still
// resolve its source name. A positional parse (third label value) reads the
// owner or feed and loses the source.
func TestFetchOutcomesFindsSourceByLabelName(t *testing.T) {
	t.Parallel()

	body := `stable_source_tested_nodes{source="first",owner="curated",feed="chan-a"} 11
stable_source_valid_nodes{feed="chan-b",source="second",owner="curated"} 22
stable_source_tested_nodes{owner="crawler",feed="chan-c",source="third"} 33
`
	reading, err := outcomeFixture(t, body, http.StatusOK)
	if err != nil {
		t.Fatalf("fetchOutcomes: %v", err)
	}
	want := outcomeReading{
		Sources: map[string]sourceOutcome{
			"first":  {Survivors: 11, Valid: 0},
			"second": {Survivors: 0, Valid: 22},
			"third":  {Survivors: 33, Valid: 0},
		},
	}
	if !reflect.DeepEqual(reading, want) {
		t.Fatalf("reading = %+v, want %+v", reading, want)
	}
}

// TestFetchOutcomesSkipsMalformedLines: one corrupt line between two good ones
// must cost only itself — the reading keeps the good lines and the malformed
// source stays absent, so the caller folds no verdict for it.
func TestFetchOutcomesSkipsMalformedLines(t *testing.T) {
	t.Parallel()

	body := `stable_source_tested_nodes{feed="a",owner="curated",source="good-1"} 7
stable_source_tested_nodes{feed="a",owner="curated",source="unclosed" 3
stable_source_tested_nodes{feed="a"owner="curated",source="no-eq"} 3
stable_source_tested_nodes{feed="a",owner="curated",source="bad-val"} nope
stable_source_valid_nodes{feed="a",owner="curated"} 3
stable_source_tested_nodes{feed="a",owner="curated",source="nan-src"} NaN
stable_source_tested_nodes{feed="a",owner="curated",source="neg-src"} -4
stable_source_tested_nodes{feed="a",owner="curated",source="bad-float"} 1.2.3
stable_source_tested_nodes 9
stable_source_valid_nodes{feed="a",owner="curated",source="good-2"} 6
stable_source_tested_nodes{feed="a",owner="curated",source="good-2"} 4
`
	reading, err := outcomeFixture(t, body, http.StatusOK)
	if err != nil {
		t.Fatalf("fetchOutcomes: %v", err)
	}
	want := outcomeReading{
		Sources: map[string]sourceOutcome{
			"good-1": {Survivors: 7, Valid: 0},
			"good-2": {Survivors: 4, Valid: 6},
		},
	}
	if !reflect.DeepEqual(reading, want) {
		t.Fatalf("reading = %+v, want %+v", reading, want)
	}
}

// TestFetchOutcomesUnescapesSourceName: an escaped quote or backslash inside
// the source= value must not end the name early; the value comes back as the
// config key the writer escaped, not as the wire text.
func TestFetchOutcomesUnescapesSourceName(t *testing.T) {
	t.Parallel()

	body := `stable_source_tested_nodes{feed="f",owner="curated",source="gh-esc\"aped-2"} 4
stable_source_valid_nodes{feed="f",owner="curated",source="gh-back\\slash"} 5
`
	reading, err := outcomeFixture(t, body, http.StatusOK)
	if err != nil {
		t.Fatalf("fetchOutcomes: %v", err)
	}
	want := outcomeReading{
		Sources: map[string]sourceOutcome{
			`gh-esc"aped-2`: {Survivors: 4, Valid: 0},
			`gh-back\slash`: {Survivors: 0, Valid: 5},
		},
	}
	if !reflect.DeepEqual(reading, want) {
		t.Fatalf("reading = %+v, want %+v", reading, want)
	}
}

// TestFetchOutcomesTruncatesExponentValue: the writer renders values through
// 'g' formatting, so the publish clock and the counts can arrive as 1.234e+03;
// each must truncate to 1234, the clock to whole seconds and the counts to
// whole nodes.
func TestFetchOutcomesTruncatesExponentValue(t *testing.T) {
	t.Parallel()

	body := `stable_last_success_timestamp_seconds 1.234e+03
stable_source_tested_nodes{feed="f",owner="curated",source="exp"} 1.234e+03
stable_source_valid_nodes{feed="f",owner="curated",source="exp"} 2.5e+00
`
	reading, err := outcomeFixture(t, body, http.StatusOK)
	if err != nil {
		t.Fatalf("fetchOutcomes: %v", err)
	}
	want := outcomeReading{
		Published: 1234,
		Sources: map[string]sourceOutcome{
			"exp": {Survivors: 1234, Valid: 2},
		},
	}
	if !reflect.DeepEqual(reading, want) {
		t.Fatalf("reading = %+v, want %+v", reading, want)
	}
}

func TestFetchOutcomesErrorsOnStatus(t *testing.T) {
	t.Parallel()

	reading, err := outcomeFixture(t, "boom", http.StatusInternalServerError)
	if !errors.Is(err, errOutcomesStatus) {
		t.Fatalf("err = %v, want errOutcomesStatus", err)
	}
	if !reflect.DeepEqual(reading, outcomeReading{}) {
		t.Fatalf("reading = %+v, want the empty reading", reading)
	}
}

// TestFetchOutcomesErrorsOnRefusedConnection: a transport failure must return
// the empty reading and keep the underlying error reachable through errors.Is.
func TestFetchOutcomesErrorsOnRefusedConnection(t *testing.T) {
	t.Parallel()

	refused := errors.New("dial tcp 127.0.0.1:9090: connect: connection refused")
	client := &http.Client{Transport: failingTransport{err: refused}}
	reading, err := fetchOutcomes(t.Context(), client, "http://sub-preprocessor:9090/metrics")
	if !errors.Is(err, refused) {
		t.Fatalf("err = %v, want the transport error wrapped", err)
	}
	if errors.Is(err, errOutcomesStatus) || errors.Is(err, errOutcomesNotService) {
		t.Fatalf("err = %v, want a transport failure, not a status/service verdict", err)
	}
	if !reflect.DeepEqual(reading, outcomeReading{}) {
		t.Fatalf("reading = %+v, want the empty reading", reading)
	}
}

func TestFetchOutcomesNotTheServiceSentinel(t *testing.T) {
	// A body that shares a family's name prefix without being a parseable
	// sample must not count as the family either.
	unrelated := `# HELP http_requests_total Total requests.
# TYPE http_requests_total counter
http_requests_total{code="200"} 12
stable_source_valid_nodes_total{feed="decoy",owner="curated",source="decoy"} 1
stable_cycles_total_extra 5
stable_source_valid_nodes{feed="a",owner="curated",source="x"}
`
	for name, body := range map[string]string{
		"unrelated metrics": unrelated,
		"empty body":        "",
		"comments only":     "# HELP stable_cycles_total Cycles.\n# TYPE stable_cycles_total counter\n",
		// The service before its first published cycle renders only these two
		// counters: no publish clock, no per-source rows. It must read as
		// "not this service's exposition" now that the fold's clock is the
		// publish timestamp, and the caller's fail-safe treats that verdict
		// exactly like every other unreadable shape — withdraw nothing.
		"counters only, never published": "stable_cycles_total 3\nstable_cycle_failures_total 3\n",
	} {
		t.Run(name, func(t *testing.T) {
			reading, err := outcomeFixture(t, body, http.StatusOK)
			if !errors.Is(err, errOutcomesNotService) {
				t.Fatalf("err = %v, want errOutcomesNotService", err)
			}
			if !reflect.DeepEqual(reading, outcomeReading{}) {
				t.Fatalf("reading = %+v, want the empty reading", reading)
			}
		})
	}

	// The boundary on the other side: the publish clock without any source
	// rows is still this service's exposition — a valid empty reading (the
	// caller withdraws nothing on it), not the not-the-service verdict.
	t.Run("publish clock without source rows", func(t *testing.T) {
		reading, err := outcomeFixture(t, "stable_last_success_timestamp_seconds 1755474123\n", http.StatusOK)
		if err != nil {
			t.Fatalf("fetchOutcomes: %v", err)
		}
		want := outcomeReading{Published: 1755474123}
		if !reflect.DeepEqual(reading, want) {
			t.Fatalf("reading = %+v, want %+v", reading, want)
		}
	})
}

// TestFetchOutcomesRejectsOversizedBody: an exposition past outcomesMaxBody
// must fail the whole reading — a truncated fold is a corrupted observation.
func TestFetchOutcomesRejectsOversizedBody(t *testing.T) {
	t.Parallel()

	// Lines safely under outcomesMaxLine so the scan completes; 17 of them
	// push the body past outcomesMaxBody.
	const (
		oversizedLine  = 1 << 19
		oversizedLines = 17
	)
	line := strings.Repeat("x", oversizedLine) + "\n"
	body := strings.Repeat(line, oversizedLines)
	reading, err := outcomeFixture(t, body, http.StatusOK)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v, want a body-over-cap error", err)
	}
	if !reflect.DeepEqual(reading, outcomeReading{}) {
		t.Fatalf("reading = %+v, want the empty reading", reading)
	}
}

// TestFetchOutcomesRejectsPathologicalLine: one line beyond outcomesMaxLine
// aborts the scan loudly instead of silently truncating the reading's tail.
func TestFetchOutcomesRejectsPathologicalLine(t *testing.T) {
	t.Parallel()

	body := strings.Repeat("x", 2*outcomesMaxLine)
	reading, err := outcomeFixture(t, body, http.StatusOK)
	if err == nil || !strings.Contains(err.Error(), "read outcomes body") {
		t.Fatalf("err = %v, want a scan error", err)
	}
	if !reflect.DeepEqual(reading, outcomeReading{}) {
		t.Fatalf("reading = %+v, want the empty reading", reading)
	}
}

// TestFetchOutcomesAppliesTheTimeout: a wedged endpoint must fail the scrape at
// the declared outcomesTimeout instead of pinning the fold indefinitely. The
// transport here is a normal one and the handler is what hangs — it sleeps
// past the deadline and only answers once its request context is cancelled,
// which is how the scrape's own timeout ends it. The error surfaces as the
// deadline itself and the reading stays empty. The test takes about
// outcomesTimeout to run: the deadline has to actually fire for the pin to be
// honest, and a fetch that returned sooner would prove only that some other
// bound existed.
func TestFetchOutcomesAppliesTheTimeout(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * outcomesTimeout):
			io.WriteString(w, "too late\n")
		case <-r.Context().Done():
			// The scrape gave up; leave so the server can shut down promptly.
		}
	}))
	t.Cleanup(srv.Close)

	reading, err := fetchOutcomes(context.Background(), srv.Client(), srv.URL)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded: outcomesTimeout must bound the scrape", err)
	}
	if !reflect.DeepEqual(reading, outcomeReading{}) {
		t.Fatalf("reading = %+v, want the empty reading", reading)
	}
}
