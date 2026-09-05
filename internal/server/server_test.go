package server_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
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
	denied  filter.CountrySet
	err     error
}

func (s *recordingService) Filter(ctx context.Context, b *bytes.Buffer, req preprocess.FilterRequest) (preprocess.Stats, error) {
	s.called = true
	s.ctx = ctx
	s.allowed = req.AllowedCountries
	s.denied = req.DeniedCountries
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
	// CountryFilter: true — every fake below stands in for a processor whose
	// IP-stage chain enforces the request's country policy.
	holder := server.NewHolder(&server.Snapshot{Svc: svc, Groups: groups, CountryFilter: true})
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
	if !filter.Permits(svc.allowed, svc.denied, geofeed.CountryCode{'F', 'I'}) {
		t.Fatal("expected FI to remain after exclusion")
	}
	if filter.Permits(svc.allowed, svc.denied, geofeed.CountryCode{'D', 'E'}) {
		t.Fatal("expected DE to be excluded")
	}
	if filter.Permits(svc.allowed, svc.denied, geofeed.CountryCode{'E', 'E'}) {
		t.Fatal("expected EE to be excluded")
	}
	// countries= was supplied, so the allow-list still governs: a country the
	// caller never listed is dropped even though it was never excluded either.
	if filter.Permits(svc.allowed, svc.denied, geofeed.CountryCode{'R', 'U'}) {
		t.Fatal("expected an unlisted country to stay outside the allow-list")
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
	if !filter.Permits(svc.allowed, svc.denied, geofeed.CountryCode{'F', 'I'}) {
		t.Fatal("expected FI from nordics")
	}
	if filter.Permits(svc.allowed, svc.denied, geofeed.CountryCode{'E', 'E'}) {
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
			if got := filter.Permits(svc.allowed, svc.denied, cc); got != want {
				t.Fatalf("%s: got %v, want %v", cc, got, want)
			}
		}
	}
	// exclude_countries is a deny-list, not "every country except DE": an IP no
	// geo source can place is in no excluded country and must survive.
	if !filter.Permits(svc.allowed, svc.denied, geofeed.CountryCode{}) {
		t.Fatal("exclusion-only request must keep an unknown-country IP")
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

func TestServerUnknownExcludeGroupRejected(t *testing.T) {
	t.Parallel()

	groups := map[string][]string{
		"nordics": {"FI", "SE"},
	}
	svc := &recordingService{}
	srv := newServer(svc, groups)
	status, body := doGet(t, srv, "/?subscription_url=https://mifa.world/vless&groups=nordics&exclude_groups=unknown")

	if status != http.StatusBadRequest {
		t.Fatalf("unexpected status: %d", status)
	}
	if !strings.Contains(body, "unknown") {
		t.Fatalf("error must name the offending token, got %q", body)
	}
	if svc.called {
		t.Fatal("an exclusion the server cannot honour must not be silently dropped")
	}
}

func TestServerExcludeEveryCountryReturnsError(t *testing.T) {
	t.Parallel()

	var every []string
	for c1 := byte('A'); c1 <= 'Z'; c1++ {
		for c2 := byte('A'); c2 <= 'Z'; c2++ {
			every = append(every, string([]byte{c1, c2}))
		}
	}
	svc := &recordingService{}
	srv := newServer(svc, map[string][]string{"everything": every})
	status, _ := doGet(t, srv, "/?subscription_url=https://mifa.world/vless&exclude_groups=everything")

	if status != http.StatusBadRequest {
		t.Fatalf("excluding every country must fail with 400, got %d", status)
	}
	if svc.called {
		t.Fatal("service should not be called when nothing is left to serve")
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
		Svc:           stubA,
		Groups:        map[string][]string{"ga": {"FI"}},
		CountryFilter: true,
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
		Svc:           stubB,
		Groups:        map[string][]string{"gb": {"DE"}},
		CountryFilter: true,
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

// panicService reproduces what a hand-rolled index expression does on a
// malformed node: it panics inside the request path.
type panicService struct{}

func (panicService) Filter(_ context.Context, _ *bytes.Buffer, _ preprocess.FilterRequest) (preprocess.Stats, error) {
	panic("index out of range [7] with length 3")
}

func TestServerRecoversHandlerPanic(t *testing.T) {
	t.Parallel()

	srv := newServer(panicService{}, nil)
	status, body := doGet(t, srv, "/?subscription_url=https://mifa.world/vless&countries=FI,EE")
	if status != http.StatusInternalServerError {
		t.Fatalf("panicking handler must answer 500, got %d", status)
	}
	if strings.Contains(body, "index out of range") {
		t.Fatalf("panic value leaked to the client: %q", body)
	}

	// The process — and with it the in-memory stable list — must have survived.
	status, body = doGet(t, srv, "/healthz")
	if status != http.StatusOK || body != "ok" {
		t.Fatalf("server unusable after a recovered panic: status=%d body=%q", status, body)
	}
}

// TestServerBoundsPipelineWithDeadline pins the bound on a single request:
// fasthttp's RequestCtx reports no deadline and never cancels on client
// disconnect, so the handler has to install one itself.
func TestServerBoundsPipelineWithDeadline(t *testing.T) {
	t.Parallel()

	svc := &recordingService{}
	srv := newServer(svc, nil)

	status, _ := doGet(t, srv, "/?subscription_url=https://mifa.world/vless&countries=FI,EE")
	if status != http.StatusOK {
		t.Fatalf("unexpected status: %d", status)
	}

	// The handler installs indexRequestTimeout (60s, server.go) on the
	// pipeline context. Assert that budget, not merely "some deadline under
	// five minutes": the 55s floor absorbs this test's own request round-trip
	// while still failing a budget loosened to minutes.
	deadline, ok := svc.ctx.Deadline()
	if !ok {
		t.Fatal("pipeline context carries no deadline: an abandoned request would resolve nodes unbounded")
	}
	if until := time.Until(deadline); until <= 55*time.Second || until > 60*time.Second {
		t.Fatalf("pipeline deadline is %v away, want indexRequestTimeout's 60s budget", until)
	}
}

func TestServerRejectsTooManyNodes(t *testing.T) {
	t.Parallel()

	svc := &recordingService{err: preprocess.ErrTooManyNodes}
	srv := newServer(svc, nil)

	status, body := doGet(t, srv, "/?subscription_url=https://mifa.world/vless&countries=FI,EE")
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversized node list must answer 413, got %d", status)
	}
	if !strings.Contains(body, "nodes") {
		t.Fatalf("client should learn the body was too large: %q", body)
	}
}

func TestServerReportsPipelineTimeout(t *testing.T) {
	t.Parallel()

	svc := &recordingService{err: context.DeadlineExceeded}
	srv := newServer(svc, nil)

	status, body := doGet(t, srv, "/?subscription_url=https://mifa.world/vless&countries=FI,EE")
	if status != http.StatusGatewayTimeout {
		t.Fatalf("a request that hit its deadline must answer 504, got %d", status)
	}
	if strings.Contains(body, "context") {
		t.Fatalf("internal error leaked to client: %q", body)
	}
}

func TestLoggedSubscriptionURLIsRedacted(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	holder := server.NewHolder(&server.Snapshot{Svc: stubService{}, Groups: nil, CountryFilter: true})
	srv := server.New(zerolog.New(&logs), ":8080", holder, stable.NewHolder())

	const secret = "https://provider.example/api/v1/client/subscribe?token=s3cr3t"
	status, _ := doGet(t, srv, "/?subscription_url="+url.QueryEscape(secret)+"&countries=FI,EE")
	if status != http.StatusOK {
		t.Fatalf("unexpected status: %d", status)
	}

	line := logs.String()
	if strings.Contains(line, "s3cr3t") || strings.Contains(line, "subscribe") {
		t.Fatalf("credential-bearing subscription URL logged verbatim: %q", line)
	}
	if !strings.Contains(line, "provider.example") {
		t.Fatalf("redacted log line lost the host, leaving nothing to debug with: %q", line)
	}

	digestOf := func() string {
		for line := range strings.SplitSeq(logs.String(), "\n") {
			if !strings.Contains(line, "subscription_url") {
				continue
			}
			_, rest, ok := strings.Cut(line, "provider.example")
			if !ok {
				continue
			}
			digest, _, _ := strings.Cut(rest, ",")
			return digest
		}
		return ""
	}
	first := digestOf()
	if first == "" || !strings.Contains(first, "#") {
		t.Fatalf("redacted URL must carry a # before the digest: %q", first)
	}

	logs.Reset()
	status2, _ := doGet(t, srv, "/?subscription_url="+url.QueryEscape(secret)+"&countries=FI,EE")
	if status2 != http.StatusOK {
		t.Fatalf("unexpected second status: %d", status2)
	}
	if again := digestOf(); again != first {
		t.Fatalf("digest for one URL must be stable across calls: %q vs %q", first, again)
	}

	logs.Reset()
	doGet(t, srv, "/?subscription_url="+url.QueryEscape(secret+"x")+"&countries=FI,EE")
	if other := digestOf(); other == first {
		t.Fatal("a different subscription URL must render a different digest")
	}
}

func TestServerRejectsBlankOnlyCountryParams(t *testing.T) {
	t.Parallel()

	// Each is the vacuous shape in one family: ",," / "," / whitespace-only
	// tokens carry no country, so the request is exactly the no-parameter one
	// and must 400 — in the exclusion family it used to pass the gate, exclude
	// nothing, and answer 200 with the full unfiltered subscription.
	for _, target := range []string{
		"/?subscription_url=https://mifa.world/vless&exclude_countries=,,",
		"/?subscription_url=https://mifa.world/vless&exclude_groups=,",
		"/?subscription_url=https://mifa.world/vless&exclude_countries=%20,%20",
		"/?subscription_url=https://mifa.world/vless&countries=,,",
		"/?subscription_url=https://mifa.world/vless&groups=%20,%20",
	} {
		svc := &recordingService{}
		srv := newServer(svc, nil)
		status, _ := doGet(t, srv, target)
		if status != http.StatusBadRequest {
			t.Fatalf("%s: blank-only country params must 400, got %d", target, status)
		}
		if svc.called {
			t.Fatalf("%s: service must not be called", target)
		}
	}

	// Blanks between valid tokens stay tolerated: they contribute nothing, the
	// valid tokens still filter.
	svc := &recordingService{}
	srv := newServer(svc, nil)
	status, _ := doGet(t, srv, "/?subscription_url=https://mifa.world/vless&countries=FI,,EE&exclude_countries=EE,")
	if status != http.StatusOK {
		t.Fatalf("valid tokens with stray blanks must still serve, got %d", status)
	}
}

func TestServerRejectsRepeatedQueryKeys(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"subscription_url":  "/?subscription_url=https://a.example.com/sub&subscription_url=https://b.example.com/sub&countries=FI",
		"countries":         "/?subscription_url=https://mifa.world/vless&countries=DE&countries=FR",
		"groups":            "/?subscription_url=https://mifa.world/vless&groups=a&groups=a",
		"exclude_countries": "/?subscription_url=https://mifa.world/vless&exclude_countries=US&exclude_countries=RU",
		"exclude_groups":    "/?subscription_url=https://mifa.world/vless&exclude_groups=a&exclude_groups=b",
	}
	for name, target := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			svc := &recordingService{}
			srv := newServer(svc, map[string][]string{"a": {"FI"}, "b": {"DE"}})
			status, body := doGet(t, srv, target)
			if status != http.StatusBadRequest {
				t.Fatalf("a repeated %s must 400, got %d", name, status)
			}
			// fiber answers the FIRST value of a repeated key, so the second
			// occurrence is otherwise dropped without a trace; the 400 must say
			// which parameter repeated.
			if !strings.Contains(body, "repeated query parameter: "+name) {
				t.Fatalf("400 must name the repeated parameter: %q", body)
			}
			if svc.called {
				t.Fatal("service must not be called with a repeated query key")
			}
		})
	}

	// A comma-joined list under one key is the documented way to pass several
	// values, not a repetition.
	svc := &recordingService{}
	srv := newServer(svc, nil)
	status, _ := doGet(t, srv, "/?subscription_url=https://mifa.world/vless&countries=DE,FR")
	if status != http.StatusOK {
		t.Fatalf("a comma-joined list must still serve, got %d", status)
	}
}

func TestServerRejectsOversizeBodyWith413(t *testing.T) {
	t.Parallel()

	// The 10 MiB byte ceiling binds before the 50k-node count for realistic
	// URI lengths; the fetch layer refuses the body with this text, wrapped by
	// subscription.Load and the preprocess loader on its way here.
	oversize := &recordingService{err: errors.New("load subscription: fetch subscription: response too large: over 10485760 bytes")}
	srv := newServer(oversize, nil)
	status, body := doGet(t, srv, "/?subscription_url=https://mifa.world/vless&countries=FI,EE")
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("a body over the byte ceiling must answer 413, got %d", status)
	}
	if !strings.Contains(body, "too large") {
		t.Fatalf("client should learn the body was too large: %q", body)
	}

	// An upstream fault keeps its 502: oversize must stay distinguishable from
	// upstream failure by status.
	upstream := &recordingService{err: errors.New("load subscription: fetch subscription: bad status: 500 Internal Server Error")}
	srvUp := newServer(upstream, nil)
	status, _ = doGet(t, srvUp, "/?subscription_url=https://mifa.world/vless&countries=FI,EE")
	if status != http.StatusBadGateway {
		t.Fatalf("an upstream fetch error must stay 502, got %d", status)
	}
}

func TestServerRejectsCountryParamsWithoutCountryFilter(t *testing.T) {
	t.Parallel()

	// CountryFilter: false is the snapshot of a processor built from a config
	// whose filters list has no country/asn entry (empty or cidr-only). GET /
	// cannot then honor any of its four parameters; 200 would be a list the
	// parameters never constrained.
	holder := server.NewHolder(&server.Snapshot{Svc: stubService{}, CountryFilter: false})
	srv := server.New(nopLogger(), ":8080", holder, stable.NewHolder())

	for _, target := range []string{
		"/?subscription_url=https://mifa.world/vless&countries=FI",
		"/?subscription_url=https://mifa.world/vless&groups=gb",
		"/?subscription_url=https://mifa.world/vless&exclude_countries=DE",
		"/?subscription_url=https://mifa.world/vless&exclude_groups=gb",
	} {
		status, body := doGet(t, srv, target)
		if status != http.StatusBadRequest {
			t.Fatalf("%s: no country-capable filter must 400, got %d", target, status)
		}
		if !strings.Contains(body, "country") || !strings.Contains(body, "filters[].type") {
			t.Fatalf("400 must name the missing filter and the enabling config key: %q", body)
		}
	}

	// The asn/cidr-only deployment stays legal: only the country-gated
	// endpoint refuses, the rest of the server answers normally.
	if status, body := doGet(t, srv, "/healthz"); status != http.StatusOK || body != "ok" {
		t.Fatalf("healthz must stay up without a country filter: status=%d body=%q", status, body)
	}
}

func TestServerServesCountryParamsWithDefaultSnapshot(t *testing.T) {
	t.Parallel()

	// NewSnapshot defaults CountryFilter to true, which is the shipped
	// deployment shape; a country-gated request must keep serving.
	svc := &recordingService{}
	holder := server.NewHolder(server.NewSnapshot(svc, nil, nil))
	srv := server.New(nopLogger(), ":8080", holder, stable.NewHolder())

	status, _ := doGet(t, srv, "/?subscription_url=https://mifa.world/vless&countries=FI,EE")
	if status != http.StatusOK {
		t.Fatalf("unexpected status: %d", status)
	}
	if !svc.called {
		t.Fatal("service should be called")
	}
}

func TestStableStatsHeaderTracksSnapshot(t *testing.T) {
	t.Parallel()

	stableHolder := stable.NewHolder()
	stableHolder.Store(&stable.Snapshot{
		Payload:   []byte("vless://x#a-001\n"),
		UpdatedAt: time.Date(2026, 7, 7, 3, 4, 5, 0, time.UTC),
		Stats:     stable.Stats{SourcesOK: 1, SourcesTotal: 2, Merged: 3, Tested: 2, Kept: 1},
	})
	holder := server.NewHolder(&server.Snapshot{Svc: stubService{}})
	srv := server.New(nopLogger(), ":8080", holder, stableHolder)

	fetch := func() (hdr, body string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/stable.txt", nil)
		resp, err := srv.TestApp().Test(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.Header.Get("X-Stable-Stats"), string(b)
	}

	want1 := "updated=" + time.Date(2026, 7, 7, 3, 4, 5, 0, time.UTC).Format(time.RFC3339) + " sources=1/2 merged=3 tested=2 kept=1"
	if hdr, body := fetch(); body != "vless://x#a-001\n" || hdr != want1 {
		t.Fatalf("snapshot A:\nbody %q\nhdr  %q\nwant %q", body, hdr, want1)
	}
	// A second request against the same snapshot must render the identical
	// header (the memoized value, not a fresh format).
	if hdr, _ := fetch(); hdr != want1 {
		t.Fatalf("repeated request header changed: got %q want %q", hdr, want1)
	}

	// A Store of a NEW snapshot must be served with ITS header on the next
	// request: the memo is keyed on the snapshot pointer, never on the first
	// one seen.
	stableHolder.Store(&stable.Snapshot{
		Payload:   []byte("vless://x#b-002\n"),
		UpdatedAt: time.Date(2026, 8, 8, 4, 5, 6, 0, time.UTC),
		Stats:     stable.Stats{SourcesOK: 3, SourcesTotal: 4, Merged: 5, Tested: 4, Kept: 2},
	})
	want2 := "updated=" + time.Date(2026, 8, 8, 4, 5, 6, 0, time.UTC).Format(time.RFC3339) + " sources=3/4 merged=5 tested=4 kept=2"
	if hdr, body := fetch(); body != "vless://x#b-002\n" || hdr != want2 {
		t.Fatalf("snapshot B:\nbody %q\nhdr  %q\nwant %q", body, hdr, want2)
	}
}

// stablePairingOK fails t when one /stable.txt response pairs snapshot A's or
// B's payload with the other snapshot's X-Stable-Stats header.
func stablePairingOK(t *testing.T, srv *server.Server, snapA, snapB *stable.Snapshot) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/stable.txt", nil)
	resp, err := srv.TestApp().Test(req)
	if err != nil {
		t.Error(err)
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Error(err)
		return
	}
	hdr := resp.Header.Get("X-Stable-Stats")
	switch string(body) {
	case "A\n":
		if want := "updated=" + snapA.UpdatedAt.Format(time.RFC3339) + " "; !strings.HasPrefix(hdr, want) {
			t.Errorf("header %q paired with payload %q", hdr, body)
		}
	case "B\n":
		if want := "updated=" + snapB.UpdatedAt.Format(time.RFC3339) + " "; !strings.HasPrefix(hdr, want) {
			t.Errorf("header %q paired with payload %q", hdr, body)
		}
	default:
		t.Errorf("unexpected payload %q", body)
	}
}

func TestStableStatsHeaderNeverMixesSnapshots(t *testing.T) {
	stableHolder := stable.NewHolder()
	holder := server.NewHolder(&server.Snapshot{Svc: stubService{}})
	srv := server.New(nopLogger(), ":8080", holder, stableHolder)

	snapA := &stable.Snapshot{Payload: []byte("A\n"), UpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Stats: stable.Stats{SourcesOK: 1, SourcesTotal: 1, Merged: 1, Tested: 1, Kept: 1}}
	snapB := &stable.Snapshot{Payload: []byte("B\n"), UpdatedAt: time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC), Stats: stable.Stats{SourcesOK: 1, SourcesTotal: 1, Merged: 1, Tested: 1, Kept: 1}}
	stableHolder.Store(snapA)

	// The memo is read and written on every request while the worker swaps
	// snapshots; no response may mix one snapshot's payload with the other's
	// header, and the shared memo state must stay race-free.
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 8 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					stablePairingOK(t, srv, snapA, snapB)
				}
			}
		})
	}
	for range 20 {
		stableHolder.Store(snapA)
		stableHolder.Store(snapB)
	}
	close(stop)
	wg.Wait()
}
