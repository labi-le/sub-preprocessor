package ghfind //nolint:testpackage // exercises unexported fields (base, sleepFn)

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// testAPI points the client at ts and swaps in a recording sleeper so bucket
// pacing and backoff never touch the wall clock; the returned slice receives
// every sleep the client would have performed. The pacing intervals are zeroed
// because an injected sleeper does not advance the clock, which would make
// bucket.wait spin until the pacing window elapsed for real.
func testAPI(t *testing.T, ts *httptest.Server) (*API, *[]time.Duration) {
	t.Helper()
	slept := new([]time.Duration)
	api := NewAPI("test-token", ts.Client(), zerolog.Nop())
	api.base = ts.URL
	api.core.minInterval = 0
	api.search.minInterval = 0
	api.code.minInterval = 0
	api.sleepFn = func(_ context.Context, d time.Duration) error {
		*slept = append(*slept, d)
		return nil
	}
	return api, slept
}

func TestSearchCodeRetryAfterSleepsThenSucceeds(t *testing.T) {
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/search/code" {
			t.Errorf("path = %q, want /search/code", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("q") != "vless:// extension:txt" {
			t.Errorf("q = %q", q.Get("q"))
		}
		if q.Get("per_page") != "100" {
			t.Errorf("per_page = %q", q.Get("per_page"))
		}
		if q.Get("page") != "2" {
			t.Errorf("page = %q", q.Get("page"))
		}
		if requests.Load() == 1 {
			w.Header().Set("Retry-After", "2")
			w.Header().Set("X-Ratelimit-Remaining", "29")
			w.Header().Set("X-Ratelimit-Reset", strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10))
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`)
			return
		}
		w.Header().Set("X-Ratelimit-Remaining", "28")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"total_count":42,"items":[{"path":"conf/sub.txt","repository":{"full_name":"owner/proj"}}]}`)
	}))
	defer ts.Close()

	api, slept := testAPI(t, ts)
	hits, total, err := api.SearchCode(context.Background(), "vless:// extension:txt", 2)
	if err != nil {
		t.Fatalf("SearchCode: %v", err)
	}
	if total != 42 {
		t.Errorf("total = %d, want 42", total)
	}
	want := []CodeHit{{Repo: "owner/proj", Path: "conf/sub.txt"}}
	if !slices.Equal(hits, want) {
		t.Errorf("hits = %v, want %v", hits, want)
	}
	if requests.Load() != 2 {
		t.Errorf("requests = %d, want 2 (one retry after the 403)", requests.Load())
	}
	if wantSleeps := []time.Duration{2 * time.Second}; !slices.Equal(*slept, wantSleeps) {
		t.Errorf("sleeps = %v, want only the 2s retry-after", *slept)
	}
	if api.Sleeps() != int64(len(*slept)) {
		t.Errorf("Sleeps() = %d, recorded sleeps = %d", api.Sleeps(), len(*slept))
	}
	if api.Calls() != 2 {
		t.Errorf("Calls() = %d, want 2 attempts", api.Calls())
	}
}

func TestSearchReposDecodesMetadata(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/repositories" {
			t.Errorf("path = %q, want /search/repositories", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("q") != "topic:vless pushed:>=2026-08-01" {
			t.Errorf("q = %q", q.Get("q"))
		}
		if q.Get("sort") != "updated" || q.Get("order") != "desc" {
			t.Errorf("sort/order = %q/%q", q.Get("sort"), q.Get("order"))
		}
		if q.Get("per_page") != "100" {
			t.Errorf("per_page = %q", q.Get("per_page"))
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"total_count":77,"items":[{"full_name":"owner/proj","default_branch":"main",`+
			`"pushed_at":"2026-09-01T12:34:56Z","archived":false,"fork":true,`+
			`"stargazers_count":123,"topics":["v2ray","config"]}]}`)
	}))
	defer ts.Close()

	api, _ := testAPI(t, ts)
	repos, total, err := api.SearchRepos(context.Background(), "topic:vless pushed:>=2026-08-01", 1)
	if err != nil {
		t.Fatalf("SearchRepos: %v", err)
	}
	if total != 77 {
		t.Errorf("total = %d, want 77", total)
	}
	if len(repos) != 1 {
		t.Fatalf("len(repos) = %d, want 1", len(repos))
	}
	got := repos[0]
	if got.Name != "owner/proj" || got.DefaultBranch != "main" {
		t.Errorf("repo identity = %q@%q", got.Name, got.DefaultBranch)
	}
	if !got.PushedAt.Equal(time.Date(2026, 9, 1, 12, 34, 56, 0, time.UTC)) {
		t.Errorf("PushedAt = %v, want 2026-09-01T12:34:56Z", got.PushedAt)
	}
	if got.Archived || !got.Fork {
		t.Errorf("Archived/Fork = %v/%v, want false/true", got.Archived, got.Fork)
	}
	if got.Stars != 123 {
		t.Errorf("Stars = %d, want 123", got.Stars)
	}
	if !slices.Equal(got.Topics, []string{"v2ray", "config"}) {
		t.Errorf("Topics = %v", got.Topics)
	}
}

func TestRepoNotFoundIsErrNotFound(t *testing.T) {
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/repos/owner/gone" {
			t.Errorf("path = %q, want /repos/owner/gone", r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"Not Found"}`)
	}))
	defer ts.Close()

	api, _ := testAPI(t, ts)
	_, err := api.Repo(context.Background(), "owner/gone")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if requests.Load() != 1 {
		t.Errorf("requests = %d, want 1 (404 is not retried)", requests.Load())
	}
}

func TestTreeTruncatedIsNotAnError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/proj/git/trees/main" {
			t.Errorf("path = %q, want /repos/owner/proj/git/trees/main", r.URL.Path)
		}
		if r.URL.Query().Get("recursive") != "1" {
			t.Errorf("recursive = %q", r.URL.Query().Get("recursive"))
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"sha":"abc","truncated":true,"tree":[`+
			`{"path":"a/b.txt","type":"blob","size":512},`+
			`{"path":"sub","type":"tree","size":0}]}`)
	}))
	defer ts.Close()

	api, _ := testAPI(t, ts)
	entries, truncated, err := api.Tree(context.Background(), "owner/proj", "main")
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if !truncated {
		t.Error("truncated = false, want true")
	}
	want := []TreeEntry{
		{Path: "a/b.txt", Size: 512, Blob: true},
		{Path: "sub", Blob: false},
	}
	if !slices.Equal(entries, want) {
		t.Errorf("entries = %v, want %v", entries, want)
	}
}

func TestSearchRejectedQueryNotRetried(t *testing.T) {
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnprocessableEntity)
		io.WriteString(w, `{"message":"Validation Failed"}`)
	}))
	defer ts.Close()

	api, _ := testAPI(t, ts)
	_, _, err := api.SearchCode(context.Background(), "bad:query", 1)
	if !errors.Is(err, ErrBadQuery) {
		t.Fatalf("err = %v, want ErrBadQuery", err)
	}
	if requests.Load() != 1 {
		t.Errorf("requests = %d, want 1 (422 is not retried)", requests.Load())
	}
}

func TestRawURLPercentEscapesPathSegments(t *testing.T) {
	got := RawURL("owner/name", "main", "sub dir/conf ig#1.txt")
	want := "https://raw.githubusercontent.com/owner/name/main/sub%20dir/conf%20ig%231.txt"
	if got != want {
		t.Errorf("RawURL = %q, want %q", got, want)
	}
}
