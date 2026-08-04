package geofeed

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/fetch"
)

// stubClock pins timeNow so month templating in LoadDBIP is deterministic.
func stubClock(t *testing.T, now time.Time) {
	t.Helper()
	orig := timeNow
	t.Cleanup(func() { timeNow = orig })
	timeNow = func() time.Time { return now }
}

func stubFetch(t *testing.T, fn func(url fetch.SubscriptionURL, fileType fetch.FileType) ([]byte, error)) *[]string {
	t.Helper()
	orig := fetchBytes
	t.Cleanup(func() { fetchBytes = orig })
	calls := &[]string{}
	fetchBytes = func(_ context.Context, url fetch.SubscriptionURL, _ int64, fileType fetch.FileType) ([]byte, error) {
		*calls = append(*calls, string(url))
		return fn(url, fileType)
	}
	return calls
}

// TestLoadDBIP_MonthFallback: the current month 404s (not yet published), so
// the load must retry exactly once with the previous month and succeed.
func TestLoadDBIP_MonthFallback(t *testing.T) {
	stubClock(t, time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC))
	calls := stubFetch(t, func(url fetch.SubscriptionURL, fileType fetch.FileType) ([]byte, error) {
		if fileType != fetch.FileTypeGzip {
			t.Fatalf("dbip must fetch gzip, got %q", fileType)
		}
		if string(url) == "https://x/db-2026-06.csv.gz" {
			return []byte("1.0.0.0,1.0.0.255,AU\n"), nil
		}
		return nil, &fetch.StatusError{Code: http.StatusNotFound}
	})

	ranges, err := LoadDBIP(context.Background(), "https://x/db-{yyyy-mm}.csv.gz", zerolog.Nop())
	if err != nil {
		t.Fatalf("LoadDBIP: %v", err)
	}
	if len(ranges) != 1 {
		t.Fatalf("got %d ranges, want 1", len(ranges))
	}
	want := []string{"https://x/db-2026-07.csv.gz", "https://x/db-2026-06.csv.gz"}
	if len(*calls) != 2 || (*calls)[0] != want[0] || (*calls)[1] != want[1] {
		t.Fatalf("fetch calls = %v, want %v", *calls, want)
	}
}

// TestLoadDBIP_BothMonths404 verifies the double-404 path returns the error so
// the caller can degrade to an empty lookup.
func TestLoadDBIP_BothMonths404(t *testing.T) {
	stubClock(t, time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC))
	calls := stubFetch(t, func(fetch.SubscriptionURL, fetch.FileType) ([]byte, error) {
		return nil, &fetch.StatusError{Code: http.StatusNotFound}
	})

	if _, err := LoadDBIP(context.Background(), "https://x/db-{yyyy-mm}.csv.gz", zerolog.Nop()); err == nil {
		t.Fatal("both months 404 must return an error")
	}
	if len(*calls) != 2 {
		t.Fatalf("fetch calls = %v, want exactly 2 (one retry)", *calls)
	}
}

// TestLoadDBIP_NoRetryPaths: non-404 failures and URLs without the month
// placeholder must not trigger the previous-month retry.
func TestLoadDBIP_NoRetryPaths(t *testing.T) {
	stubClock(t, time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC))

	calls := stubFetch(t, func(fetch.SubscriptionURL, fetch.FileType) ([]byte, error) {
		return nil, errors.New("network down")
	})
	if _, err := LoadDBIP(context.Background(), "https://x/db-{yyyy-mm}.csv.gz", zerolog.Nop()); err == nil {
		t.Fatal("non-404 failure must return an error")
	}
	if len(*calls) != 1 {
		t.Fatalf("non-404 failure must not retry, calls = %v", *calls)
	}

	calls = stubFetch(t, func(fetch.SubscriptionURL, fetch.FileType) ([]byte, error) {
		return nil, &fetch.StatusError{Code: http.StatusNotFound}
	})
	if _, err := LoadDBIP(context.Background(), "https://x/static.csv.gz", zerolog.Nop()); err == nil {
		t.Fatal("404 without placeholder must return an error")
	}
	if len(*calls) != 1 {
		t.Fatalf("no placeholder means both URLs are identical; must not refetch, calls = %v", *calls)
	}
}

// TestLoadRegistry_SkipsFailedSource mirrors LoadAll: one bad RIR must not fail
// the load but must be reported through the failed count; ALL failing (or zero
// total ranges) must error.
func TestLoadRegistry_SkipsFailedSource(t *testing.T) {
	stubFetch(t, func(url fetch.SubscriptionURL, fileType fetch.FileType) ([]byte, error) {
		if fileType != fetch.FileTypeRaw {
			t.Fatalf("registry must fetch raw, got %q", fileType)
		}
		if string(url) == "https://bad.example/delegated" {
			return nil, errors.New("transient boom")
		}
		return []byte("apnic|AU|ipv4|1.0.0.0|256|20110811|assigned\n"), nil
	})

	urls := []string{"https://bad.example/delegated", "https://good.example/delegated"}
	ranges, failed, err := LoadRegistry(context.Background(), urls, zerolog.Nop())
	if err != nil {
		t.Fatalf("one bad RIR must not fail the load: %v", err)
	}
	if len(ranges) != 1 {
		t.Fatalf("got %d ranges, want 1", len(ranges))
	}
	if failed != 1 {
		t.Fatalf("partial load must report 1 failed RIR, got %d", failed)
	}

	stubFetch(t, func(fetch.SubscriptionURL, fetch.FileType) ([]byte, error) {
		return nil, errors.New("boom")
	})
	if _, allFailed, allErr := LoadRegistry(context.Background(), urls, zerolog.Nop()); allErr == nil ||
		allFailed != 2 {
		t.Fatalf("all RIRs failing must error with failed=2, got failed=%d err=%v", allFailed, allErr)
	}

	// Fetches succeed but nothing parses: still an error (nothing to serve).
	stubFetch(t, func(fetch.SubscriptionURL, fetch.FileType) ([]byte, error) {
		return []byte("2|apnic|20260718|1|19830705|20260717|+1000\n"), nil
	})
	if _, _, emptyErr := LoadRegistry(context.Background(), urls, zerolog.Nop()); emptyErr == nil {
		t.Fatal("zero total ranges must return an error")
	}
}

// TestRegistrySerialIsLogged pins the freshness observable. delegatedSerial
// must read field 2 of the version header (the neighbouring fields are a
// record COUNT and a date, so an off-by-one reads plausibly and means nothing),
// and LoadRegistry must put it on the per-source load line -- that log is the
// entire signal that a mirrored copy has stopped tracking its upstream.
func TestRegistrySerialIsLogged(t *testing.T) {
	const header = "2.3|apnic|20260804|188932||20260803|+1000\n"
	const record = "apnic|AU|ipv4|1.0.0.0|256|20110811|assigned\n"

	if got := delegatedSerial([]byte(header + record)); got != "20260804" {
		t.Fatalf("delegatedSerial = %q, want the header's field 2 %q", got, "20260804")
	}
	// Comment banner first, as every real file ships it.
	if got := delegatedSerial([]byte("# CONDITIONS OF USE\n#\n" + header + record)); got != "20260804" {
		t.Fatalf("delegatedSerial past the comment banner = %q, want %q", got, "20260804")
	}
	// A body opening on a RECORD has no serial, and must not pass that record's
	// own field 2 off as one -- "serial=ipv4" would read like a live value.
	if got := delegatedSerial([]byte(record)); got != "" {
		t.Fatalf("delegatedSerial on a headerless body = %q, want empty", got)
	}
	if got := delegatedSerial(nil); got != "" {
		t.Fatalf("delegatedSerial(nil) = %q, want empty", got)
	}

	stubFetch(t, func(fetch.SubscriptionURL, fetch.FileType) ([]byte, error) {
		return []byte(header + record), nil
	})
	var logged bytes.Buffer
	if _, _, err := LoadRegistry(context.Background(),
		[]string{"https://good.example/delegated"}, zerolog.New(&logged)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logged.String(), `"serial":"20260804"`) {
		t.Fatalf("the per-source load line carries no serial: %s", logged.String())
	}
}
