package server_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"domains.lst/sub-preprocessor/internal/filter"
	"domains.lst/sub-preprocessor/internal/geofeed"
	"domains.lst/sub-preprocessor/internal/preprocess"
	"domains.lst/sub-preprocessor/internal/server"
	"domains.lst/sub-preprocessor/internal/stable"
	"github.com/rs/zerolog"
)

type stubService struct{}

func (stubService) Filter(_ context.Context, b *bytes.Buffer, _ preprocess.FilterRequest) (preprocess.Stats, error) {
	b.WriteString("vless://test")
	return preprocess.Stats{Total: 1, Kept: 1}, nil
}

type recordingService struct {
	called  bool
	ctx     context.Context
	allowed filter.CountrySet
	err     error
}

func (s *recordingService) Filter(ctx context.Context, b *bytes.Buffer, req preprocess.FilterRequest) (preprocess.Stats, error) {
	s.called = true
	s.ctx = ctx
	s.allowed = req.AllowedCountries
	if s.err != nil {
		return preprocess.Stats{}, s.err
	}
	b.WriteString("vless://node#ok")
	return preprocess.Stats{Total: 1, Kept: 1}, nil
}

type snapStub struct {
	marker  string
	allowed filter.CountrySet
}

func (s *snapStub) Filter(_ context.Context, b *bytes.Buffer, req preprocess.FilterRequest) (preprocess.Stats, error) {
	s.allowed = req.AllowedCountries
	b.WriteString(s.marker)
	return preprocess.Stats{Total: 1, Kept: 1}, nil
}

func nopLogger() zerolog.Logger {
	return zerolog.Nop()
}

func newServer(svc server.Filterer, groups map[string][]string) *server.Server {
	holder := server.NewHolder(&server.Snapshot{Svc: svc, Groups: groups})
	return server.New(nopLogger(), ":8080", holder, stable.NewHolder())
}

func doGet(t *testing.T, srv *server.Server, target string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	resp, err := srv.TestApp().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func TestServerReturnsPlainText(t *testing.T) {
	t.Parallel()

	srv := newServer(stubService{}, nil)
	req := httptest.NewRequest(http.MethodGet, "/?subscription_url=https://mifa.world/vless&countries=FI,EE", nil)
	resp, err := srv.TestApp().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "vless://test") {
		t.Fatalf("unexpected body: %q", body)
	}
}

func TestServerAcceptsGroupsInsteadOfCountries(t *testing.T) {
	t.Parallel()

	groups := map[string][]string{
		"nordics": {"FI", "SE", "NO", "DK"},
	}
	svc := &recordingService{}
	srv := newServer(svc, groups)
	req := httptest.NewRequest(http.MethodGet, "/?subscription_url=https://mifa.world/vless&groups=nordics", nil)
	resp, err := srv.TestApp().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "vless://node#ok") {
		t.Fatalf("unexpected body: %q", body)
	}
	if !svc.called {
		t.Fatal("service should be called")
	}
	if !svc.allowed.Has(geofeed.CountryCode{'F', 'I'}) {
		t.Fatal("expected FI from nordics group")
	}
	if !svc.allowed.Has(geofeed.CountryCode{'S', 'E'}) {
		t.Fatal("expected SE from nordics group")
	}
	if svc.allowed.Has(geofeed.CountryCode{'D', 'E'}) {
		t.Fatal("unexpected DE (not in group)")
	}
}

func TestServerRejectsMissingBothCountriesAndGroups(t *testing.T) {
	t.Parallel()

	svc := &recordingService{}
	srv := newServer(svc, nil)
	req := httptest.NewRequest(http.MethodGet, "/?subscription_url=https://mifa.world/vless", nil)
	resp, err := srv.TestApp().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}
	if svc.called {
		t.Fatal("service should not be called without countries or groups")
	}
}

func TestServerRejectsNonHTTPSSubscriptionURL(t *testing.T) {
	t.Parallel()

	svc := &recordingService{}
	srv := newServer(svc, nil)
	req := httptest.NewRequest(http.MethodGet, "/?subscription_url=http://mifa.world/vless&countries=FI,EE", nil)
	resp, err := srv.TestApp().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}
	if svc.called {
		t.Fatal("service should not be called for invalid subscription_url")
	}
}

func TestServerRejectsLocalSubscriptionURL(t *testing.T) {
	t.Parallel()

	svc := &recordingService{}
	srv := newServer(svc, nil)
	req := httptest.NewRequest(http.MethodGet, "/?subscription_url=https://127.0.0.1/vless&countries=FI,EE", nil)
	resp, err := srv.TestApp().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}
	if svc.called {
		t.Fatal("service should not be called for invalid subscription_url")
	}
}

func TestServerUsesRequestContext(t *testing.T) {
	t.Parallel()

	svc := &recordingService{}
	srv := newServer(svc, nil)
	req := httptest.NewRequest(http.MethodGet, "/?subscription_url=https://mifa.world/vless&countries=FI,EE", nil)

	resp, err := srv.TestApp().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if !svc.called {
		t.Fatal("service was not called")
	}
	if svc.ctx == nil {
		t.Fatal("request context was not propagated")
	}
}

func TestServerReturnsNoContentForFavicon(t *testing.T) {
	t.Parallel()

	svc := &recordingService{}
	srv := newServer(svc, nil)
	req := httptest.NewRequest(http.MethodGet, "/favicon.ico", nil)
	resp, err := srv.TestApp().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}
	if len(body) != 0 {
		t.Fatalf("unexpected body: %q", body)
	}
	if svc.called {
		t.Fatal("service should not be called for favicon")
	}
}

func TestServerHidesInternalErrors(t *testing.T) {
	t.Parallel()

	svc := &recordingService{err: errors.New("dial tcp 10.0.0.5:443: i/o timeout")}
	srv := newServer(svc, nil)
	req := httptest.NewRequest(http.MethodGet, "/?subscription_url=https://mifa.world/vless&countries=FI,EE", nil)
	resp, err := srv.TestApp().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}
	if strings.Contains(string(body), "10.0.0.5") || strings.Contains(string(body), "dial tcp") {
		t.Fatalf("internal error leaked to client: %q", body)
	}
}

func TestServerExcludesCountries(t *testing.T) {
	t.Parallel()

	svc := &recordingService{}
	srv := newServer(svc, nil)
	req := httptest.NewRequest(http.MethodGet, "/?subscription_url=https://mifa.world/vless&countries=FI,EE,DE&exclude_countries=DE,EE", nil)
	resp, err := srv.TestApp().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}
	if !svc.called {
		t.Fatal("service should be called")
	}
	if !svc.allowed.Has(geofeed.CountryCode{'F', 'I'}) {
		t.Fatal("expected FI to remain after exclusion")
	}
	if svc.allowed.Has(geofeed.CountryCode{'D', 'E'}) {
		t.Fatal("expected DE to be excluded")
	}
	if svc.allowed.Has(geofeed.CountryCode{'E', 'E'}) {
		t.Fatal("expected EE to be excluded")
	}
}

func TestServerExcludesGroup(t *testing.T) {
	t.Parallel()

	groups := map[string][]string{
		"nordics": {"FI", "SE", "NO"},
		"baltics": {"EE", "LV", "LT"},
	}
	svc := &recordingService{}
	srv := newServer(svc, groups)
	req := httptest.NewRequest(http.MethodGet, "/?subscription_url=https://mifa.world/vless&groups=nordics,baltics&exclude_groups=baltics", nil)
	resp, err := srv.TestApp().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}
	if !svc.allowed.Has(geofeed.CountryCode{'F', 'I'}) {
		t.Fatal("expected FI from nordics")
	}
	if svc.allowed.Has(geofeed.CountryCode{'E', 'E'}) {
		t.Fatal("expected EE to be excluded")
	}
}

func TestServerExcludeOnly(t *testing.T) {
	t.Parallel()

	svc := &recordingService{}
	srv := newServer(svc, nil)
	req := httptest.NewRequest(http.MethodGet, "/?subscription_url=https://mifa.world/vless&exclude_countries=DE", nil)
	resp, err := srv.TestApp().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}
	if !svc.called {
		t.Fatal("service should be called")
	}
	for c1 := byte('A'); c1 <= 'Z'; c1++ {
		for c2 := byte('A'); c2 <= 'Z'; c2++ {
			cc := geofeed.CountryCode{c1, c2}
			want := c1 != 'D' || c2 != 'E'
			if got := svc.allowed.Has(cc); got != want {
				t.Fatalf("%s: got %v, want %v", cc, got, want)
			}
		}
	}
}

func TestServerExcludesAllAllowedReturnsError(t *testing.T) {
	t.Parallel()

	svc := &recordingService{}
	srv := newServer(svc, nil)
	req := httptest.NewRequest(http.MethodGet, "/?subscription_url=https://mifa.world/vless&countries=FI&exclude_countries=FI", nil)
	resp, err := srv.TestApp().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}
	if svc.called {
		t.Fatal("service should not be called when exclusions remove all allowed countries")
	}
}

func TestServerUnknownExcludeGroupIgnored(t *testing.T) {
	t.Parallel()

	groups := map[string][]string{
		"nordics": {"FI", "SE"},
	}
	svc := &recordingService{}
	srv := newServer(svc, groups)
	req := httptest.NewRequest(http.MethodGet, "/?subscription_url=https://mifa.world/vless&groups=nordics&exclude_groups=unknown", nil)
	resp, err := srv.TestApp().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}
	if !svc.allowed.Has(geofeed.CountryCode{'F', 'I'}) {
		t.Fatal("expected FI from nordics")
	}
	if !svc.allowed.Has(geofeed.CountryCode{'S', 'E'}) {
		t.Fatal("expected SE from nordics")
	}
}

func TestServerMalformedCountriesDoesNotAllowAll(t *testing.T) {
	t.Parallel()

	svc := &recordingService{}
	srv := newServer(svc, nil)
	req := httptest.NewRequest(http.MethodGet, "/?subscription_url=https://mifa.world/vless&countries=XXX", nil)
	resp, err := srv.TestApp().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}
	if svc.called {
		t.Fatal("service should not be called with malformed countries")
	}
}

func TestServerUnknownGroupDoesNotAllowAll(t *testing.T) {
	t.Parallel()

	groups := map[string][]string{
		"nordics": {"FI"},
	}
	svc := &recordingService{}
	srv := newServer(svc, groups)
	req := httptest.NewRequest(http.MethodGet, "/?subscription_url=https://mifa.world/vless&groups=unknown", nil)
	resp, err := srv.TestApp().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}
	if svc.called {
		t.Fatal("service should not be called with unknown group")
	}
}

func TestStableNotReady(t *testing.T) {
	t.Parallel()

	srv := newServer(stubService{}, nil)
	req := httptest.NewRequest(http.MethodGet, "/stable.txt", nil)
	resp, err := srv.TestApp().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "30" {
		t.Fatalf("expected Retry-After 30, got %q", ra)
	}
	if !strings.Contains(string(body), "stable list not ready") {
		t.Fatalf("unexpected body: %q", body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("unexpected content type: %q", ct)
	}
}

func TestStableServesPayload(t *testing.T) {
	t.Parallel()

	stableHolder := stable.NewHolder()
	updated := time.Date(2026, 7, 7, 3, 4, 5, 0, time.UTC)
	stableHolder.Store(&stable.Snapshot{
		Payload:   []byte("vless://x#a-001\n"),
		UpdatedAt: updated,
		Stats:     stable.Stats{SourcesOK: 1, SourcesTotal: 2, Merged: 3, Tested: 2, Kept: 1},
	})
	holder := server.NewHolder(&server.Snapshot{Svc: stubService{}})
	srv := server.New(nopLogger(), ":8080", holder, stableHolder)

	req := httptest.NewRequest(http.MethodGet, "/stable.txt", nil)
	resp, err := srv.TestApp().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}
	if string(body) != "vless://x#a-001\n" {
		t.Fatalf("unexpected body: %q", body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("unexpected content type: %q", ct)
	}
	wantStats := "updated=" + updated.Format(time.RFC3339) + " sources=1/2 merged=3 tested=2 kept=1"
	if got := resp.Header.Get("X-Stable-Stats"); got != wantStats {
		t.Fatalf("stats header:\ngot  %q\nwant %q", got, wantStats)
	}
}

func TestStableRejectsPost(t *testing.T) {
	t.Parallel()

	srv := newServer(stubService{}, nil)
	req := httptest.NewRequest(http.MethodPost, "/stable.txt", nil)
	resp, err := srv.TestApp().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}
}

func TestServerReadsSnapshotPerRequest(t *testing.T) {
	t.Parallel()

	stubA := &snapStub{marker: "SNAP-A"}
	holder := server.NewHolder(&server.Snapshot{
		Svc:    stubA,
		Groups: map[string][]string{"ga": {"FI"}},
	})
	srv := server.New(nopLogger(), ":8080", holder, stable.NewHolder())

	statusA, bodyA := doGet(t, srv, "/?subscription_url=https://mifa.world/vless&groups=ga")
	if statusA != http.StatusOK {
		t.Fatalf("snapshot A: unexpected status: %d", statusA)
	}
	if !strings.Contains(bodyA, "SNAP-A") {
		t.Fatalf("snapshot A: unexpected body: %q", bodyA)
	}
	if !stubA.allowed.Has(geofeed.CountryCode{'F', 'I'}) {
		t.Fatal("snapshot A: expected FI from group ga")
	}

	stubB := &snapStub{marker: "SNAP-B"}
	holder.Store(&server.Snapshot{
		Svc:    stubB,
		Groups: map[string][]string{"gb": {"DE"}},
	})

	statusB, bodyB := doGet(t, srv, "/?subscription_url=https://mifa.world/vless&groups=gb")
	if statusB != http.StatusOK {
		t.Fatalf("snapshot B: unexpected status: %d", statusB)
	}
	if !strings.Contains(bodyB, "SNAP-B") {
		t.Fatalf("snapshot B: expected swapped body, got: %q", bodyB)
	}
	if !stubB.allowed.Has(geofeed.CountryCode{'D', 'E'}) {
		t.Fatal("snapshot B: expected DE from group gb")
	}
	if stubA.allowed.Has(geofeed.CountryCode{'D', 'E'}) {
		t.Fatal("snapshot A service must not handle the post-swap request")
	}
}
