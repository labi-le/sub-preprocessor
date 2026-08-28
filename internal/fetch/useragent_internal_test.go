package fetch

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const (
	minDistinctBrands = 5
	uaDraws           = 200
	uaRequests        = 12
	maxBodyBytes      = 1 << 10
)

// uaBrand cuts the leading product token a panel matches on.
func uaBrand(ua string) string {
	brand, _, _ := strings.Cut(ua, " ")
	return brand
}

func TestUserAgentPoolIsNotEmptyAndDistinct(t *testing.T) {
	t.Parallel()

	if len(userAgents) == 0 {
		t.Fatal("UA pool must not be empty")
	}
	brands := make(map[string]bool, len(userAgents))
	for _, ua := range userAgents {
		if ua == "" {
			t.Fatal("UA pool must not contain empty strings")
		}
		brands[uaBrand(ua)] = true
	}
	if len(brands) < minDistinctBrands {
		t.Fatalf("pool covers %d distinct clients, want at least %d", len(brands), minDistinctBrands)
	}
}

func TestUserAgentAlwaysReturnsAPoolMemberAndRotates(t *testing.T) {
	t.Parallel()

	pool := make(map[string]bool, len(userAgents))
	for _, ua := range userAgents {
		pool[ua] = true
	}
	seen := make(map[string]int, len(userAgents))
	for range uaDraws {
		ua := UserAgent()
		if !pool[ua] {
			t.Fatalf("UserAgent = %q, which is not a pool member", ua)
		}
		seen[ua]++
	}
	if len(seen) < 2 {
		t.Fatalf("%d draws produced %d distinct identities; rotation is not happening", uaDraws, len(seen))
	}
}

// The client swap below is the only way a test can observe BytesWithType's
// outbound identity: the function hard-requires an https URL with a public
// host, so a plain httptest target is unreachable without redirecting the
// dial to the local listener while keeping a public-looking hostname.
func TestBytesWithTypeSendsAPoolIdentityOnEveryRequest(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var got []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, r.Header.Get("User-Agent"))
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)

	// The swap below repoints the package sharedClient var, so no other test in
	// package fetch may run in parallel with this one (cidrset's load_internal_test
	// documents the same constraint over its own fetch swap). hwid_internal_test
	// swaps it too and stays sequential for that reason.

	originalClient := sharedClient
	sharedClient = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, srv.Listener.Addr().String())
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // test server presents an httptest-internal cert
	}}
	t.Cleanup(func() { sharedClient = originalClient })

	pool := make(map[string]bool, len(userAgents))
	for _, ua := range userAgents {
		pool[ua] = true
	}

	seen := make(map[string]bool, len(userAgents))
	for range uaRequests {
		body, err := BytesWithType(t.Context(), "https://example.com/sub", maxBodyBytes, FileTypeRaw)
		if err != nil {
			t.Fatalf("BytesWithType: %v", err)
		}
		if string(body) != "ok" {
			t.Fatalf("body = %q, want %q", body, "ok")
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != uaRequests {
		t.Fatalf("server saw %d requests, want %d", len(got), uaRequests)
	}
	for _, ua := range got {
		if !pool[ua] {
			t.Fatalf("request went out as %q, which is not a pool member", ua)
		}
		seen[ua] = true
	}
	if len(seen) < 2 {
		t.Fatalf("%d requests carried %d distinct identities; rotation is not reaching the wire", uaRequests, len(seen))
	}
}
