package subscription

import (
	"context"
	"testing"

	"domains.lst/sub-preprocessor/internal/fetch"
)

// A source's hwid is the difference between real nodes and a panel's placeholder
// answer, which arrives as a healthy 200 — so an hwid dropped here disables the
// feature without booking an error anywhere, leaving that source's own
// stable_source_published_nodes at 0 as the only sign. Load is the only hop
// between FilterRequest.HWID and the outbound header, and fetchBytes is where it
// lands. The fetchBytes swap is package-global, so this test may not call
// t.Parallel.
func TestLoadForwardsTheHWIDToFetch(t *testing.T) {
	original := fetchBytes
	t.Cleanup(func() { fetchBytes = original })

	var got struct {
		url      fetch.SubscriptionURL
		limit    int64
		fileType fetch.FileType
		hwid     string
		calls    int
	}
	fetchBytes = func(
		_ context.Context, rawURL fetch.SubscriptionURL, limit int64, fileType fetch.FileType, h string,
	) ([]byte, error) {
		got.url, got.limit, got.fileType, got.hwid = rawURL, limit, fileType, h
		got.calls++
		return []byte("vless://u@192.0.2.1:443#n\n"), nil
	}

	for _, hwid := range []string{"abcdef0123456789", ""} {
		got.calls = 0

		body, err := Load(t.Context(), "https://example.com/sub", hwid)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got.calls != 1 {
			t.Fatalf("fetch calls = %d, want 1", got.calls)
		}
		if got.hwid != hwid {
			t.Errorf("hwid = %q, want %q", got.hwid, hwid)
		}
		if got.url != "https://example.com/sub" {
			t.Errorf("url = %q", got.url)
		}
		if got.limit != MaxSubscriptionSize || got.fileType != fetch.FileTypeRaw {
			t.Errorf("limit/fileType = %d/%q", got.limit, got.fileType)
		}
		if string(body) != "vless://u@192.0.2.1:443#n" {
			t.Errorf("Normalize must still run over a fetched body; got %q", body)
		}
	}
}
