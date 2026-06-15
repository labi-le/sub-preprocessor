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

	"domains.lst/sub-preprocessor/internal/filter"
	"domains.lst/sub-preprocessor/internal/geofeed"
	"domains.lst/sub-preprocessor/internal/preprocess"
	"domains.lst/sub-preprocessor/internal/server"
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

func nopLogger() zerolog.Logger {
	return zerolog.Nop()
}

func TestServerReturnsPlainText(t *testing.T) {
	t.Parallel()

	srv := server.New(nopLogger(), ":8080", stubService{}, nil)
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
	srv := server.New(nopLogger(), ":8080", svc, groups)
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
	srv := server.New(nopLogger(), ":8080", svc, nil)
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
	srv := server.New(nopLogger(), ":8080", svc, nil)
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
	srv := server.New(nopLogger(), ":8080", svc, nil)
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
	srv := server.New(nopLogger(), ":8080", svc, nil)
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
	srv := server.New(nopLogger(), ":8080", svc, nil)
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
	srv := server.New(nopLogger(), ":8080", svc, nil)
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
	srv := server.New(nopLogger(), ":8080", svc, nil)
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
	srv := server.New(nopLogger(), ":8080", svc, groups)
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
	srv := server.New(nopLogger(), ":8080", svc, nil)
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
	srv := server.New(nopLogger(), ":8080", svc, nil)
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
	srv := server.New(nopLogger(), ":8080", svc, groups)
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
	srv := server.New(nopLogger(), ":8080", svc, nil)
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
	srv := server.New(nopLogger(), ":8080", svc, groups)
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
