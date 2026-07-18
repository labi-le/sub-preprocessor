package geofeed

import (
	"context"
	"errors"
	"net/http"
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
// the load; ALL failing (or zero total ranges) must.
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
	ranges, err := LoadRegistry(context.Background(), urls, zerolog.Nop())
	if err != nil {
		t.Fatalf("one bad RIR must not fail the load: %v", err)
	}
	if len(ranges) != 1 {
		t.Fatalf("got %d ranges, want 1", len(ranges))
	}

	stubFetch(t, func(fetch.SubscriptionURL, fetch.FileType) ([]byte, error) {
		return nil, errors.New("boom")
	})
	if _, allErr := LoadRegistry(context.Background(), urls, zerolog.Nop()); allErr == nil {
		t.Fatal("all RIRs failing must return an error")
	}

	// Fetches succeed but nothing parses: still an error (nothing to serve).
	stubFetch(t, func(fetch.SubscriptionURL, fetch.FileType) ([]byte, error) {
		return []byte("2|apnic|20260718|1|19830705|20260717|+1000\n"), nil
	})
	if _, emptyErr := LoadRegistry(context.Background(), urls, zerolog.Nop()); emptyErr == nil {
		t.Fatal("zero total ranges must return an error")
	}
}
