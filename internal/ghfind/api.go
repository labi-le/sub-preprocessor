package ghfind

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
)

const (
	apiBase       = "https://api.github.com"
	rawBase       = "https://raw.githubusercontent.com/"
	apiVersion    = "2022-11-28"
	apiMediaType  = "application/vnd.github+json"
	userAgent     = "sub-preprocessor"
	perPage       = 100
	requestBudget = 30 * time.Second

	// maxResponseBody caps every response read so a hostile or misbehaving
	// server cannot balloon memory. It has to clear a RECURSIVE TREE, not a
	// search page: GitHub serves trees up to 100k entries / ~7 MB, and a cap
	// below that truncates the JSON mid-object, which surfaces as a bogus
	// "unexpected EOF" decode failure rather than as a size refusal — measured
	// 2026-09-07, Surfboardv2ray/TGParse failed exactly that way at 4 MiB.
	maxResponseBody = 16 << 20

	// secondaryRateLimitPhrase is the message GitHub's 403 carries when the
	// secondary (per-minute) limit trips without a Retry-After header.
	secondaryRateLimitPhrase = "secondary rate limit"
)

// Terminal failures shared with the discovery loop in find.go. A repo that
// vanished, a credential GitHub refuses, and a search query GitHub rejects
// are all situations where retrying the same call cannot help.
var (
	ErrNotFound  = errors.New("ghfind: not found")
	ErrForbidden = errors.New("ghfind: forbidden")
	ErrBadQuery  = errors.New("ghfind: bad query")
)

// StatusError reports a non-2xx status the client neither retries nor maps to
// a sentinel; Code and Status mirror the HTTP response.
type StatusError struct {
	Code   int
	Status string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("ghfind: unexpected status %d %s", e.Code, e.Status)
}

// CodeHit is one code-search result: Repo is "owner/name", Path is
// repo-relative.
type CodeHit struct {
	Repo, Path string
}

// RepoMeta is the repository metadata the admission rules in find.go read.
// PushedAt is parsed from the API's RFC3339 pushed_at; a repo that has never
// been pushed carries the zero time.
type RepoMeta struct {
	Name          string
	DefaultBranch string
	PushedAt      time.Time
	Archived      bool
	Fork          bool
	Stars         int
	Topics        []string
}

// TreeEntry is one entry of a recursive git tree listing; Size is meaningful
// only for Blob entries.
type TreeEntry struct {
	Path string
	Size int
	Blob bool
}

// API is a rate-limited GitHub REST v3 client covering the four endpoints the
// discovery phase needs. Each endpoint category is metered by its own bucket;
// retries, pacing and backoff sleeps are counted for metrics.
type API struct {
	base   string
	token  string
	client *http.Client
	log    zerolog.Logger

	core   *bucket // /repos endpoints
	search *bucket // /search/repositories
	code   *bucket // /search/code

	calls  atomic.Int64 // HTTP attempts, retries included
	sleeps atomic.Int64 // pacing, reset and backoff waits performed

	// sleepFn replaces the real sleeper when set so tests run without
	// wall-clock delays; the sleep count still accrues.
	sleepFn func(context.Context, time.Duration) error
}

// NewAPI builds a client for api.github.com; client must already carry the
// transports, timeouts and proxy policy the crawler wants.
func NewAPI(token string, client *http.Client, logger zerolog.Logger) *API {
	return &API{
		base:   apiBase,
		token:  token,
		client: client,
		log:    logger,
		core:   newBucket(coreMinInterval),
		search: newBucket(searchMinInterval),
		code:   newBucket(codeSearchMinInterval),
	}
}

// Sleeps reports how many rate-limit waits the client has performed.
func (a *API) Sleeps() int64 { return a.sleeps.Load() }

// Calls reports how many HTTP attempts the client has made, retries included.
func (a *API) Calls() int64 { return a.calls.Load() }

// SearchCode runs one page (1-based) of a GitHub code search against the
// search/code quota, decoding only the fields the discovery phase reads.
func (a *API) SearchCode(ctx context.Context, query string, page int) ([]CodeHit, int, error) {
	params := url.Values{}
	params.Set("q", query)
	params.Set("per_page", strconv.Itoa(perPage))
	params.Set("page", strconv.Itoa(page))
	var out codeSearchResponse
	if err := a.get(ctx, a.code, a.base+"/search/code?"+params.Encode(), &out); err != nil {
		return nil, 0, fmt.Errorf("searching code: %w", err)
	}
	hits := make([]CodeHit, 0, len(out.Items))
	for _, item := range out.Items {
		hits = append(hits, CodeHit{Repo: item.Repository.FullName, Path: item.Path})
	}
	return hits, out.TotalCount, nil
}

// SearchRepos runs one page (1-based) of a repository search, newest push
// first, against the search quota.
func (a *API) SearchRepos(ctx context.Context, query string, page int) ([]RepoMeta, int, error) {
	params := url.Values{}
	params.Set("q", query)
	params.Set("per_page", strconv.Itoa(perPage))
	params.Set("page", strconv.Itoa(page))
	params.Set("sort", "updated")
	params.Set("order", "desc")
	var out repoSearchResponse
	if err := a.get(ctx, a.search, a.base+"/search/repositories?"+params.Encode(), &out); err != nil {
		return nil, 0, fmt.Errorf("searching repositories: %w", err)
	}
	repos := make([]RepoMeta, 0, len(out.Items))
	for _, item := range out.Items {
		meta, err := item.meta()
		if err != nil {
			return nil, 0, err
		}
		repos = append(repos, meta)
	}
	return repos, out.TotalCount, nil
}

// Repo fetches one repository; code-search results carry no pushed_at, which
// is why the admission rules pay a core call for repositories they consider.
func (a *API) Repo(ctx context.Context, full string) (RepoMeta, error) {
	var out repoResponse
	if err := a.get(ctx, a.core, a.base+"/repos/"+escapePath(full), &out); err != nil {
		return RepoMeta{}, fmt.Errorf("fetching repo %s: %w", full, err)
	}
	return out.meta()
}

// Tree lists one recursive git tree. A truncated tree is returned as far as
// it goes with truncated set, not as an error: the crawler uses what it got.
func (a *API) Tree(ctx context.Context, full, ref string) ([]TreeEntry, bool, error) {
	var out treeResponse
	u := a.base + "/repos/" + escapePath(full) + "/git/trees/" + escapePath(ref) + "?recursive=1"
	if err := a.get(ctx, a.core, u, &out); err != nil {
		return nil, false, fmt.Errorf("listing tree of %s@%s: %w", full, ref, err)
	}
	entries := make([]TreeEntry, 0, len(out.Tree))
	for _, node := range out.Tree {
		entries = append(entries, TreeEntry{Path: node.Path, Size: node.Size, Blob: node.Type == "blob"})
	}
	return entries, out.Truncated, nil
}

// RawURL renders the raw.githubusercontent.com URL of one blob: branch is the
// default branch by name, never HEAD — HEAD would silently follow a branch
// rename, and a renamed default branch is exactly when a source should die
// instead (docs/guides/github.md). Each path segment is percent-escaped so
// spaces and '#' survive; '/' separates segments and is preserved.
func RawURL(full, branch, path string) string {
	var b strings.Builder
	b.Grow(len(full) + len(branch) + len(path) + len(rawBase))
	b.WriteString(rawBase)
	b.WriteString(full)
	b.WriteByte('/')
	b.WriteString(branch)
	b.WriteByte('/')
	for i, seg := range strings.Split(path, "/") {
		if i > 0 {
			b.WriteByte('/')
		}
		b.WriteString(url.PathEscape(seg))
	}
	return b.String()
}

// get performs one endpoint call with pacing and bounded retries: quota or
// secondary-limit 403s, 429s and 5xx are retried after a sleep, everything
// else is terminal. dst receives the decoded 2xx body.
func (a *API) get(ctx context.Context, b *bucket, u string, dst any) error {
	for n := 0; ; n++ {
		delay, retry, err := a.attempt(ctx, b, u, dst, n)
		if err != nil {
			return err
		}
		if !retry {
			return nil
		}
		if delay > 0 {
			if errSleep := a.sleep(ctx, delay); errSleep != nil {
				return fmt.Errorf("rate limit backoff: %w", errSleep)
			}
		}
	}
}

// attempt runs one HTTP round trip: it waits on the bucket, performs the GET
// under requestBudget, folds the response's rate-limit headers, decodes a 2xx
// body into dst, and classifies anything else. n is the zero-based attempt
// count; a retryable status on the maxRetries-th attempt is given up as a
// StatusError rather than looped forever.
func (a *API) attempt(ctx context.Context, b *bucket, u string, dst any, n int) (time.Duration, bool, error) {
	if err := b.wait(ctx, a.sleep); err != nil {
		return 0, false, fmt.Errorf("rate limit wait: %w", err)
	}
	reqCtx, cancel := context.WithTimeout(ctx, requestBudget)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u, nil)
	if err != nil {
		return 0, false, fmt.Errorf("building request: %w", err)
	}
	a.setHeaders(req)
	a.calls.Add(1)
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, false, fmt.Errorf("performing request: %w", err)
	}
	defer resp.Body.Close()
	b.observe(resp.Header)
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		if errDec := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(dst); errDec != nil {
			return 0, false, fmt.Errorf("decoding response: %w", errDec)
		}
		return 0, false, nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	delay, retry, cerr := classifyResponse(resp.StatusCode, resp.Status, resp.Header, body, n)
	if !retry {
		return 0, false, cerr
	}
	if n >= maxRetries {
		return 0, false, &StatusError{Code: resp.StatusCode, Status: resp.Status}
	}
	return delay, true, nil
}

// classifyResponse maps a non-2xx status to a retry decision. Retry-After
// wins over the body's secondary-limit mention, which wins over an exhausted
// allowance (whose folded state makes the next bucket wait sleep until reset).
// Named not to shadow the classify package find.go imports for verdicts.
func classifyResponse(code int, status string, h http.Header, body []byte, n int) (time.Duration, bool, error) {
	switch {
	case code == http.StatusUnauthorized:
		return 0, false, ErrForbidden
	case code == http.StatusNotFound:
		return 0, false, ErrNotFound
	case code == http.StatusUnprocessableEntity:
		return 0, false, ErrBadQuery
	case code == http.StatusForbidden:
		if d, ok := retryAfter(h); ok {
			return d, true, nil
		}
		if bytes.Contains(body, []byte(secondaryRateLimitPhrase)) {
			return backoff(n), true, nil
		}
		if remaining, ok := parseHeaderInt(h, "X-Ratelimit-Remaining"); ok && remaining == 0 {
			return 0, true, nil
		}
		return 0, false, ErrForbidden
	case code == http.StatusTooManyRequests:
		if d, ok := retryAfter(h); ok {
			return d, true, nil
		}
		return backoff(n), true, nil
	case code >= http.StatusInternalServerError:
		return serverErrorDelay, true, nil
	default:
		return 0, false, &StatusError{Code: code, Status: status}
	}
}

// sleep waits d, honouring ctx cancellation, and counts every wait toward
// Sleeps; a substituted sleepFn bypasses the timer but not the count.
func (a *API) sleep(ctx context.Context, d time.Duration) error {
	a.sleeps.Add(1)
	a.log.Debug().Dur("sleep", d).Msg("ghfind rate limit wait")
	if a.sleepFn != nil {
		return a.sleepFn(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("sleep aborted: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

func (a *API) setHeaders(req *http.Request) {
	req.Header.Set("Accept", apiMediaType)
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("X-Github-Api-Version", apiVersion)
}

// escapePath percent-escapes each slash-separated segment of a repo or ref
// name: '/' separators survive so nested branch names work, while characters
// like '#' cannot truncate the URL at the fragment marker.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	return strings.Join(segs, "/")
}

// Payload shapes decode only the fields the discovery phase reads; GitHub
// search objects carry far more, and fields nothing consumes would be dead
// weight to keep in sync with the API.

type codeSearchResponse struct {
	TotalCount int `json:"total_count"`
	Items      []struct {
		Path       string `json:"path"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	} `json:"items"`
}

type repoResponse struct {
	FullName      string   `json:"full_name"`
	DefaultBranch string   `json:"default_branch"`
	PushedAt      string   `json:"pushed_at"`
	Archived      bool     `json:"archived"`
	Fork          bool     `json:"fork"`
	Stars         int      `json:"stargazers_count"`
	Topics        []string `json:"topics"`
}

// meta converts the wire shape, parsing pushed_at once per repo so admission
// rules get a real time instead of re-parsing strings.
func (r repoResponse) meta() (RepoMeta, error) {
	meta := RepoMeta{
		Name:          r.FullName,
		DefaultBranch: r.DefaultBranch,
		Archived:      r.Archived,
		Fork:          r.Fork,
		Stars:         r.Stars,
		Topics:        r.Topics,
	}
	if r.PushedAt == "" {
		return meta, nil
	}
	pushed, err := time.Parse(time.RFC3339, r.PushedAt)
	if err != nil {
		return RepoMeta{}, fmt.Errorf("repo %s pushed_at %q: %w", r.FullName, r.PushedAt, err)
	}
	meta.PushedAt = pushed
	return meta, nil
}

type repoSearchResponse struct {
	TotalCount int            `json:"total_count"`
	Items      []repoResponse `json:"items"`
}

type treeResponse struct {
	Truncated bool           `json:"truncated"`
	Tree      []treeNodeJSON `json:"tree"`
}

type treeNodeJSON struct {
	Path string `json:"path"`
	Type string `json:"type"`
	Size int    `json:"size"`
}
