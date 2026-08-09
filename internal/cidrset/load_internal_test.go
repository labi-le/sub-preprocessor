package cidrset

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/fetch"
)

// stubFetch swaps the package fetch var, so no test using it may run in
// parallel (same reason as geofeed's load_internal_test.go).
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

// TestLoadSkipsFailedSource mirrors geofeed.LoadAll: one dead mirror must not
// fail the load but must be reported, since only the count tells the caller its
// allow-list is partial.
func TestLoadSkipsFailedSource(t *testing.T) {
	calls := stubFetch(t, func(url fetch.SubscriptionURL, fileType fetch.FileType) ([]byte, error) {
		if fileType != fetch.FileTypeGzip {
			t.Errorf("file type = %q, want the one Load was given", fileType)
		}
		if string(url) == "https://bad.example/cidr.txt" {
			return nil, errors.New("transient boom")
		}
		return []byte("10.0.0.0/24\n10.0.9.0/24\n"), nil
	})

	urls := []string{"", "https://bad.example/cidr.txt", "https://good.example/cidr.txt"}
	set, failed, err := Load(context.Background(), urls, fetch.FileTypeGzip, zerolog.Nop())
	if err != nil {
		t.Fatalf("one bad source must not fail the load: %v", err)
	}
	if failed != 1 {
		t.Errorf("failed = %d, want 1", failed)
	}
	if got := set.Len(); got != 2 {
		t.Errorf("Len() = %d, want 2", got)
	}
	if !set.Contains(netip.MustParseAddr("10.0.9.1")) {
		t.Error("the surviving source's ranges must be in the set")
	}
	if len(*calls) != 2 {
		t.Errorf("fetch calls = %v, want only the two non-empty urls", *calls)
	}
}

// TestLoadMergesAcrossSources: two sources whose ranges touch must coalesce, so
// the merge has to happen over the union rather than per source.
func TestLoadMergesAcrossSources(t *testing.T) {
	stubFetch(t, func(url fetch.SubscriptionURL, _ fetch.FileType) ([]byte, error) {
		if string(url) == "https://a.example/cidr.txt" {
			return []byte("10.0.0.0/24\n"), nil
		}
		return []byte("10.0.1.0/24\n"), nil
	})

	urls := []string{"https://a.example/cidr.txt", "https://b.example/cidr.txt"}
	set, failed, err := Load(context.Background(), urls, fetch.FileTypeRaw, zerolog.Nop())
	if err != nil || failed != 0 {
		t.Fatalf("Load: failed=%d err=%v", failed, err)
	}
	if got := set.Len(); got != 1 {
		t.Errorf("Len() = %d, want 1 (adjacent ranges from two sources must merge)", got)
	}
	if !set.Contains(netip.MustParseAddr("10.0.0.255")) || !set.Contains(netip.MustParseAddr("10.0.1.0")) {
		t.Error("containment must hold across the seam between the two sources")
	}
}

// TestLoadAllSourcesFailing covers both ways a load ends up with nothing: no
// body at all, and bodies that parse to zero ranges. Both must error, and the
// message must carry the failed count -- it is what the operator sees.
func TestLoadAllSourcesFailing(t *testing.T) {
	urls := []string{"https://a.example/cidr.txt", "https://b.example/cidr.txt"}

	stubFetch(t, func(fetch.SubscriptionURL, fetch.FileType) ([]byte, error) {
		return nil, errors.New("boom")
	})
	set, failed, err := Load(context.Background(), urls, fetch.FileTypeRaw, zerolog.Nop())
	if err == nil {
		t.Fatal("all sources failing must return an error")
	}
	if failed != 2 || set.Len() != 0 {
		t.Errorf("failed = %d, Len() = %d, want 2 and 0", failed, set.Len())
	}
	if !strings.Contains(err.Error(), "2 source(s) failed") {
		t.Errorf("error %q must name how many sources failed", err)
	}

	stubFetch(t, func(fetch.SubscriptionURL, fetch.FileType) ([]byte, error) {
		return []byte("2001:db8::/32\nnonsense\n"), nil
	})
	if _, emptyFailed, emptyErr := Load(
		context.Background(), urls, fetch.FileTypeRaw, zerolog.Nop(),
	); emptyErr == nil || emptyFailed != 2 {
		t.Fatalf("bodies with zero IPv4 ranges must error with failed=2, got failed=%d err=%v", emptyFailed, emptyErr)
	}
}

// TestNewSetDoesNotPinTheParseScratch falsifies what was previously only a
// comment: the scratch is sized per LINE, so a body whose lines all coalesce
// leaves it mostly slack, and aliasing it instead of copying out would hold the
// whole buffer until the next refresh -- invisible to every other test here.
func TestNewSetDoesNotPinTheParseScratch(t *testing.T) {
	t.Parallel()

	const lines = 64

	var body strings.Builder
	for i := range lines {
		fmt.Fprintf(&body, "10.0.%d.0/24\n", i)
	}

	set, skipped := Parse([]byte(body.String()))
	if skipped != 0 {
		t.Fatalf("skipped = %d, want 0", skipped)
	}
	if set.Len() != 1 {
		t.Fatalf("Len() = %d, want 1: %d adjacent /24s must coalesce", set.Len(), lines)
	}
	if cap(set.ranges) != len(set.ranges) {
		t.Errorf("cap(ranges) = %d, len = %d: newSet must copy out, not alias the %d-line scratch",
			cap(set.ranges), len(set.ranges), lines)
	}
}
