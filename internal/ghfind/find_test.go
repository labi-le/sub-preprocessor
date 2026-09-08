package ghfind //nolint:testpackage // exercises the Finder's unexported pacing state

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// The canned tests drive Discover against a fake GitHub: every search and
// metadata endpoint answers fixed JSON, raw blob paths answer registered
// bodies, and bucket pacing is switched off so no test touches the wall clock.
// Bodies use 192.0.2.x addresses with a plain credential, which classify and
// the census both treat as live non-placeholder nodes (the classify suite
// proves the same shape live).

// treeBlobSize is the advertised size of every canned blob: comfortably in the
// filter's mid bucket, far above its 2 KiB floor, well under the 10 MiB cap.
const treeBlobSize = 60000

// vlessBody renders count nodes on consecutive 192.0.2.x addresses, starting
// at start, and returns the body plus the endpoint hashes it decodes to.
func vlessBody(start, count int) (string, []uint64) {
	var b strings.Builder
	eps := make([]uint64, 0, count)
	for i := start; i < start+count; i++ {
		srv := "192.0.2." + strconv.Itoa(i)
		b.WriteString("vless://u@")
		b.WriteString(srv)
		b.WriteString(":443#n\n")
		eps = append(eps, EndpointHash(srv, "443"))
	}
	return b.String(), eps
}

// cannedGitHub is one fake GitHub per test. Response bodies are registered
// before Discover runs; every request path is logged for assertions.
type cannedGitHub struct {
	t          *testing.T
	srv        *httptest.Server
	mu         sync.Mutex
	got        []string
	code       string            // served for every /search/code request
	rep        string            // served for every /search/repositories request
	codeStatus int               // HTTP status for /search/code; 0 means 200
	repos      map[string]string // /repos/{full} body per owner/name
	trees      map[string]string // /repos/{full}/git/trees/{ref} body
	files      map[string]string // raw blob path (leading /) -> body
}

func newCannedGitHub(t *testing.T) *cannedGitHub {
	t.Helper()
	g := &cannedGitHub{
		t:     t,
		repos: map[string]string{},
		trees: map[string]string{},
		files: map[string]string{},
	}
	g.srv = httptest.NewServer(g)
	t.Cleanup(g.srv.Close)
	return g
}

// cannedRepo describes one repository the fake GitHub knows.
type cannedRepo struct {
	full     string
	branch   string
	age      time.Duration // pushed this long ago; zero means now
	archived bool
	files    map[string]string // blob path -> body
}

// addRepo registers metadata, a recursive tree and raw blobs for one repo.
func (g *cannedGitHub) addRepo(r cannedRepo) {
	if r.branch == "" {
		r.branch = "main"
	}
	pushed := time.Now().Add(-r.age).UTC().Format(time.RFC3339)
	meta := `{"full_name":"` + r.full + `","default_branch":"` + r.branch + `","pushed_at":"` +
		pushed + `","archived":` + strconv.FormatBool(r.archived) +
		`,"fork":false,"stargazers_count":0,"topics":[]}`
	g.repos[r.full] = meta

	var tree strings.Builder
	tree.WriteString(`{"truncated":false,"tree":[`)
	paths := make([]string, 0, len(r.files))
	for path := range r.files {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for i, path := range paths {
		if i > 0 {
			tree.WriteByte(',')
		}
		tree.WriteString(`{"path":"` + path + `","type":"blob","size":` + strconv.Itoa(treeBlobSize) + `}`)
	}
	tree.WriteString(`]}`)
	g.trees[r.full+"/git/trees/"+r.branch] = tree.String()

	for path, body := range r.files {
		g.files["/"+r.full+"/"+r.branch+"/"+path] = body
	}
}

// codeHits serves one canned code-search response whose hits are the given
// repos, in order.
func (g *cannedGitHub) codeHits(repos ...string) {
	var b strings.Builder
	b.WriteString(`{"total_count":` + strconv.Itoa(len(repos)) + `,"items":[`)
	for i, full := range repos {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"path":"sub.txt","repository":{"full_name":"` + full + `"}}`)
	}
	b.WriteString(`]}`)
	g.code = b.String()
}

// repoHits serves one canned repository-search response listing the given
// repos (their metadata is looked up from what addRepo registered).
func (g *cannedGitHub) repoHits(full ...string) {
	var b strings.Builder
	b.WriteString(`{"total_count":` + strconv.Itoa(len(full)) + `,"items":[`)
	for i, name := range full {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(g.repos[name])
	}
	b.WriteString(`]}`)
	g.rep = b.String()
}

// finder builds a Finder whose API and raw fetches land on this fake GitHub.
// Bucket pacing intervals and sleeps are switched off, so tests never wait on
// the rate limiter; the responses all carry a generous remaining allowance so
// a bucket never believes it is spent.
func (g *cannedGitHub) finder(opts Options) *Finder {
	if opts.HTTP == nil {
		opts.HTTP = g.client()
	}
	opts.Logger = zerolog.Nop()
	f := New(opts)
	f.api.core.minInterval = 0
	f.api.search.minInterval = 0
	f.api.code.minInterval = 0
	f.api.sleepFn = func(context.Context, time.Duration) error { return nil }
	return f
}

func (g *cannedGitHub) client() *http.Client {
	return &http.Client{Transport: &rewriteTransport{
		next: g.srv.Client().Transport,
		host: strings.TrimPrefix(g.srv.URL, "http://"),
	}}
}

// rewriteTransport redirects the GitHub API and raw hosts to the test server.
type rewriteTransport struct {
	next http.RoundTripper
	host string // the test server's host:port
}

func (rt *rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "api.github.com" && r.URL.Host != "raw.githubusercontent.com" {
		return rt.next.RoundTrip(r)
	}
	u := *r.URL
	u.Scheme = "http"
	u.Host = rt.host
	r2 := r.Clone(r.Context())
	r2.URL = &u
	return rt.next.RoundTrip(r2)
}

func (g *cannedGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.got = append(g.got, r.URL.Path)
	g.mu.Unlock()
	w.Header().Set("X-Ratelimit-Remaining", "5000")
	w.Header().Set("X-Ratelimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
	switch {
	case r.URL.Path == "/search/code":
		if g.codeStatus != 0 {
			w.WriteHeader(g.codeStatus)
			io.WriteString(w, `{"message":"Validation Failed"}`)
			return
		}
		io.WriteString(w, g.code)
	case r.URL.Path == "/search/repositories":
		io.WriteString(w, g.rep)
	case strings.HasPrefix(r.URL.Path, "/repos/"):
		trimmed := strings.TrimPrefix(r.URL.Path, "/repos/")
		segs := strings.Split(trimmed, "/")
		body := ""
		switch {
		case len(segs) == 2:
			body = g.repos[trimmed]
		case len(segs) == 5 && segs[2] == "git" && segs[3] == "trees":
			body = g.trees[trimmed]
		}
		if body == "" {
			g.t.Errorf("canned github: no response for %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, body)
	default:
		body, ok := g.files[r.URL.Path]
		if !ok {
			g.t.Errorf("canned github: no blob for %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, body)
	}
}

// requestPaths returns the logged request paths, for order assertions.
func (g *cannedGitHub) requestPaths() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.got...)
}

func (g *cannedGitHub) hasPath(prefix string) bool {
	for _, p := range g.requestPaths() {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// discover runs one pass with the given knobs and returns its result.
func (g *cannedGitHub) discover(t *testing.T, in Input, opts Options) (Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return g.finder(opts).Discover(ctx, in)
}

func emptyCensus() *Census { return NewCensus(0) }

func TestDiscoverNoTokenRefusesBeforeAnyRequest(t *testing.T) {
	g := newCannedGitHub(t)
	res, err := g.discover(t, Input{Census: emptyCensus()}, Options{})
	if !errors.Is(err, ErrNoToken) {
		t.Fatalf("err = %v, want ErrNoToken", err)
	}
	if res.Stats.Errors != 0 || len(res.Repos) != 0 {
		t.Errorf("res = %+v, want untouched zero result", res)
	}
	if got := g.requestPaths(); len(got) != 0 {
		t.Errorf("requests = %v, want none before the token check", got)
	}
}

func TestDiscoverNilCensusAcceptsNothing(t *testing.T) {
	g := newCannedGitHub(t)
	res, err := g.discover(t, Input{Census: nil}, Options{Token: "t"})
	if err != nil {
		t.Fatalf("Discover with nil census must not error: %v", err)
	}
	if res.Stats.Errors != 1 {
		t.Errorf("Errors = %d, want 1 for the refused pass", res.Stats.Errors)
	}
	if len(res.Repos) != 0 {
		t.Errorf("Repos = %v, want none: a nil census must never mint", res.Repos)
	}
	if got := g.requestPaths(); len(got) != 0 {
		t.Errorf("requests = %v, want none: a nil census must not search either", got)
	}
}

func TestDiscoverSearchesGridFromCursorAndWraps(t *testing.T) {
	g := newCannedGitHub(t)
	body, _ := vlessBody(1, 150)
	codeRepo, repoRepo := "acme/code-repo", "acme/repo-repo"
	g.addRepo(cannedRepo{full: codeRepo, files: map[string]string{"sub-1.txt": body}})
	g.addRepo(cannedRepo{full: repoRepo, files: map[string]string{"sub-1.txt": body}})
	g.codeHits(codeRepo)
	g.repoHits(repoRepo)

	// Start one slot before the end of each grid: consuming 3 code queries and
	// 2 repo queries must wrap past the tail, not replay from the head.
	start := Cursor{
		Code: len(CodeQueries()) - 1,
		Repo: len(RepoQueries(time.Now(), defaultFresh)) - 1,
	}
	res, err := g.discover(t, Input{Census: emptyCensus(), Cursor: start}, Options{
		Token: "t", SearchCode: 3, SearchRepo: 2,
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	want := Cursor{
		Code: (start.Code + 3) % len(CodeQueries()),
		Repo: (start.Repo + 2) % len(RepoQueries(time.Now(), defaultFresh)),
	}
	if res.Cursor != want {
		t.Errorf("Cursor = %+v, want %+v (advance by queries consumed, wrap at grid end)", res.Cursor, want)
	}
	if res.Stats.SearchCalls != 5 {
		t.Errorf("SearchCalls = %d, want 5", res.Stats.SearchCalls)
	}
	// The same code repo was returned by all three queries; it is admitted once.
	if res.Stats.ReposSeen != 2 || res.Stats.Trees != 2 {
		t.Errorf("ReposSeen/Trees = %d/%d, want 2/2 after dedupe", res.Stats.ReposSeen, res.Stats.Trees)
	}
}

func TestDiscoverRefusesArchivedAndStaleRepos(t *testing.T) {
	g := newCannedGitHub(t)
	body, _ := vlessBody(1, 150)
	g.addRepo(cannedRepo{full: "acme/good", files: map[string]string{"sub-1.txt": body}})
	g.addRepo(cannedRepo{full: "acme/archived", age: time.Hour, archived: true})
	g.addRepo(cannedRepo{full: "acme/stale", age: 2 * 365 * 24 * time.Hour})
	g.codeHits()
	g.repoHits("acme/good", "acme/archived", "acme/stale")

	res, err := g.discover(t, Input{Census: emptyCensus()}, Options{Token: "t", SearchCode: 1, SearchRepo: 1})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if res.Stats.ReposSkipped != 2 {
		t.Errorf("ReposSkipped = %d, want the archived and the stale repo", res.Stats.ReposSkipped)
	}
	if res.Stats.ReposSeen != 1 || res.Stats.Trees != 1 || res.Stats.Accepted != 1 {
		t.Errorf("ReposSeen/Trees/Accepted = %d/%d/%d, want 1/1/1", res.Stats.ReposSeen, res.Stats.Trees, res.Stats.Accepted)
	}
	if len(res.Repos) != 1 || res.Repos[0].Name != "acme/good" {
		t.Fatalf("Repos = %+v, want only acme/good", res.Repos)
	}
	for _, p := range g.requestPaths() {
		if strings.Contains(p, "archived") || strings.Contains(p, "stale") {
			t.Errorf("refused repo %s was still fetched", p)
		}
	}
}

func TestDiscoverIdenticalEndpointSetsAcceptOneFile(t *testing.T) {
	g := newCannedGitHub(t)
	body, _ := vlessBody(1, 150)
	g.addRepo(cannedRepo{full: "acme/twin", files: map[string]string{
		"sub-1.txt": body,
		"sub-2.txt": body,
	}})
	g.codeHits("acme/twin")
	g.repoHits()

	res, err := g.discover(t, Input{Census: emptyCensus()}, Options{Token: "t", SearchCode: 1, SearchRepo: 1})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if res.Stats.LiveFiles != 2 {
		t.Errorf("LiveFiles = %d, want 2: both identical files were probed and live", res.Stats.LiveFiles)
	}
	if res.Stats.Accepted != 1 || res.Stats.NovelEndpoints != 150 {
		t.Errorf("Accepted/NovelEndpoints = %d/%d, want 1/150: the twin must collapse to one pick",
			res.Stats.Accepted, res.Stats.NovelEndpoints)
	}
	got := res.Repos[0].Accepted
	if len(got) != 1 || got[0].Novel != 150 {
		t.Errorf("Accepted files = %+v, want exactly one file with Novel 150", got)
	}
}

func TestDiscoverCensusCarriedEndpointsAreNotAccepted(t *testing.T) {
	g := newCannedGitHub(t)
	body, eps := vlessBody(1, 150)
	g.addRepo(cannedRepo{full: "acme/carry", files: map[string]string{"sub-1.txt": body}})
	g.codeHits("acme/carry")
	g.repoHits()

	census := NewCensus(len(eps))
	for _, h := range eps {
		census.Add(h)
	}
	res, err := g.discover(t, Input{Census: census}, Options{Token: "t", SearchCode: 1, SearchRepo: 1})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if res.Stats.Probed != 1 || res.Stats.LiveFiles != 1 {
		t.Errorf("Probed/LiveFiles = %d/%d, want 1/1: the file is still judged", res.Stats.Probed, res.Stats.LiveFiles)
	}
	if res.Stats.Accepted != 0 || res.Stats.NovelEndpoints != 0 {
		t.Errorf("Accepted/NovelEndpoints = %d/%d, want 0/0: a fully known file adds nothing",
			res.Stats.Accepted, res.Stats.NovelEndpoints)
	}
}

func TestDiscoverKnownURLProbedButNotAcceptedSuppressesDuplicate(t *testing.T) {
	g := newCannedGitHub(t)
	body, _ := vlessBody(1, 150)
	g.addRepo(cannedRepo{full: "acme/first", files: map[string]string{"sub-1.txt": body}})
	g.addRepo(cannedRepo{full: "acme/second", files: map[string]string{"sub-1.txt": body}})
	g.codeHits("acme/first", "acme/second")
	g.repoHits()

	managed := RawURL("acme/first", "main", "sub-1.txt")
	res, err := g.discover(t, Input{
		Census: emptyCensus(),
		Known:  map[string]bool{managed: true},
	}, Options{Token: "t", SearchCode: 1, SearchRepo: 1})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if !g.hasPath("/acme/first/main/sub-1.txt") {
		t.Error("the already-managed URL was not probed")
	}
	if res.Stats.Probed != 2 || res.Stats.LiveFiles != 2 {
		t.Errorf("Probed/LiveFiles = %d/%d, want 2/2", res.Stats.Probed, res.Stats.LiveFiles)
	}
	if res.Stats.Accepted != 0 || res.Stats.NovelEndpoints != 0 {
		t.Errorf("Accepted/NovelEndpoints = %d/%d, want 0/0: the managed endpoints must suppress the duplicate",
			res.Stats.Accepted, res.Stats.NovelEndpoints)
	}
	if len(res.Repos) != 2 || res.Repos[0].Name != "acme/first" || res.Repos[1].Name != "acme/second" {
		t.Fatalf("Repos = %+v, want acme/first then acme/second (admission order drives suppression)", res.Repos)
	}
	for _, r := range res.Repos {
		if len(r.Accepted) != 0 {
			t.Errorf("repo %s accepted %+v, want none", r.Name, r.Accepted)
		}
	}
}

func TestDiscoverNovelEndpointsIsUnionNotSum(t *testing.T) {
	g := newCannedGitHub(t)
	// sub-a carries 192.0.2.1-150, sub-b 192.0.2.100-250: 51 endpoints overlap.
	a, _ := vlessBody(1, 150)
	b, _ := vlessBody(100, 151)
	g.addRepo(cannedRepo{full: "acme/overlap", files: map[string]string{
		"sub-a.txt": a,
		"sub-b.txt": b,
	}})
	g.codeHits("acme/overlap")
	g.repoHits()

	res, err := g.discover(t, Input{Census: emptyCensus()}, Options{Token: "t", SearchCode: 1, SearchRepo: 1, MinNovel: 60})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	accepted := res.Repos[0].Accepted
	if len(accepted) != 2 {
		t.Fatalf("Accepted = %+v, want two files", accepted)
	}
	// Cover picks the larger set first (151 > 150), then the other still clears
	// the floor with its 99 remaining endpoints.
	if accepted[0].Path != "sub-b.txt" || accepted[0].Novel != 151 {
		t.Errorf("first pick = %s (novel %d), want sub-b.txt with 151", accepted[0].Path, accepted[0].Novel)
	}
	if accepted[1].Path != "sub-a.txt" || accepted[1].Novel != 99 {
		t.Errorf("second pick = %s (novel %d), want sub-a.txt with 99", accepted[1].Path, accepted[1].Novel)
	}
	// A sum of the files' sizes would be 301; the union of what they actually
	// added is 250, and overlap must not be counted twice.
	if res.Stats.NovelEndpoints != 250 {
		t.Errorf("NovelEndpoints = %d, want the 250-endpoint union, not the 301 sum", res.Stats.NovelEndpoints)
	}
}

func TestDiscoverRejectedQuerySkipsNotFatal(t *testing.T) {
	g := newCannedGitHub(t)
	body, _ := vlessBody(1, 150)
	g.addRepo(cannedRepo{full: "acme/good", files: map[string]string{"sub-1.txt": body}})
	g.codeStatus = http.StatusUnprocessableEntity
	g.repoHits("acme/good")
	res, err := g.discover(t, Input{Census: emptyCensus()}, Options{Token: "t", SearchCode: 1, SearchRepo: 1})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if res.Stats.SearchCalls != 2 {
		t.Errorf("SearchCalls = %d, want 2 (both queries consumed)", res.Stats.SearchCalls)
	}
	if res.Stats.Errors != 0 {
		t.Errorf("Errors = %d, want 0: a rejected query is logged and skipped, not an error", res.Stats.Errors)
	}
	if res.Stats.Accepted != 1 {
		t.Errorf("Accepted = %d, want 1: the pass must continue past the bad query", res.Stats.Accepted)
	}
}
