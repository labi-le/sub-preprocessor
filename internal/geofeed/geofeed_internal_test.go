package geofeed

import (
	"context"
	"errors"
	"testing"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/fetch"
)

// TestLoadAllSkipsFailedSource verifies one flaky source does not fail the whole
// load (availability: a transient third-party outage must not crash startup),
// that the skipped source is nonetheless reported through the failed count so a
// caller holding good data can refuse the partial result, and that
// all-sources-failing still errors.
func TestLoadAllSkipsFailedSource(t *testing.T) {
	orig := fetchBytes
	t.Cleanup(func() { fetchBytes = orig })

	fetchBytes = func(_ context.Context, url fetch.SubscriptionURL, _ int64, _ fetch.FileType) ([]byte, error) {
		if string(url) == "https://bad.example/feed" {
			return nil, errors.New("transient boom")
		}
		return []byte("198.51.100.0/24,DE\n"), nil
	}

	sources := []Source{
		{URL: "https://bad.example/feed", Type: fetch.FileType("raw")},
		{URL: "https://good.example/feed", Type: fetch.FileType("raw")},
	}
	entries, failed, err := LoadAll(context.Background(), sources, zerolog.Nop())
	if err != nil {
		t.Fatalf("one bad source must not fail the load: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry from the good source, got %d", len(entries))
	}
	if failed != 1 {
		t.Fatalf("partial load must report 1 failed source, got %d", failed)
	}

	// A clean load reports no failures, so the caller can tell the two apart.
	fetchBytes = func(context.Context, fetch.SubscriptionURL, int64, fetch.FileType) ([]byte, error) {
		return []byte("198.51.100.0/24,DE\n"), nil
	}
	if _, cleanFailed, cleanErr := LoadAll(context.Background(), sources, zerolog.Nop()); cleanErr != nil ||
		cleanFailed != 0 {
		t.Fatalf("clean load must report failed=0 and no error, got failed=%d err=%v", cleanFailed, cleanErr)
	}

	// Every source failing must still return an error (nothing to serve).
	fetchBytes = func(context.Context, fetch.SubscriptionURL, int64, fetch.FileType) ([]byte, error) {
		return nil, errors.New("boom")
	}
	if _, allFailed, allErr := LoadAll(context.Background(), sources, zerolog.Nop()); allErr == nil || allFailed != 2 {
		t.Fatalf("all sources failing must error with failed=2, got failed=%d err=%v", allFailed, allErr)
	}
}
