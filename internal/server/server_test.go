package server_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"domains.lst/sub-preprocessor/internal/preprocess"
	"domains.lst/sub-preprocessor/internal/server"
)

type stubService struct{}

func (stubService) Filter(_ context.Context, b *strings.Builder, _ string, _ []string) (preprocess.Stats, error) {
	b.WriteString("vless://test")
	return preprocess.Stats{Total: 1, Kept: 1}, nil
}

type recordingService struct {
	called bool
	ctx    context.Context
	err    error
}

func (s *recordingService) Filter(ctx context.Context, b *strings.Builder, _ string, _ []string) (preprocess.Stats, error) {
	s.called = true
	s.ctx = ctx
	if s.err != nil {
		return preprocess.Stats{}, s.err
	}
	b.WriteString("vless://node#ok")
	return preprocess.Stats{Total: 1, Kept: 1}, nil
}

func TestServerReturnsPlainText(t *testing.T) {
	t.Parallel()

	srv := server.New(":8080", stubService{})
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

func TestServerRejectsNonHTTPSSubscriptionURL(t *testing.T) {
	t.Parallel()

	svc := &recordingService{}
	srv := server.New(":8080", svc)
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
	srv := server.New(":8080", svc)
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
	srv := server.New(":8080", svc)
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

func TestServerHidesInternalErrors(t *testing.T) {
	t.Parallel()

	svc := &recordingService{err: errors.New("dial tcp 10.0.0.5:443: i/o timeout")}
	srv := server.New(":8080", svc)
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
