package ghfind

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/classify"
	"domains.lst/sub-preprocessor/internal/fetch"
	"domains.lst/sub-preprocessor/internal/subscription"
)

// Defaults mirror the knob table in docs/guides/github.md; New applies them to
// zero-valued Options so a caller can pass a partially filled struct.
const (
	defaultMaxRepos      = 120
	defaultFilesPerRepo  = 8
	defaultAcceptPerRepo = 4
	defaultMinNovel      = 100
	defaultFresh         = 504 * time.Hour
	defaultConcurrency   = 8
	defaultSearchCode    = 8
	defaultSearchRepo    = 6
	// probeTimeout caps one candidate fetch; a stalled origin must not hang the
	// whole worker pool.
	probeTimeout = 30 * time.Second
	// The per-repository cover's floor is coverFloorMin endpoints or
	// coverFloorPercent of the repository's own endpoint union, whichever is
	// larger (docs/guides/github.md step 7).
	coverFloorMin     = 20
	coverFloorPercent = 2
	// percentDivisor renders coverFloorPercent as a fraction of the union.
	percentDivisor = 100
	// firstPage: the rotating walk makes one page-1 call per query per pass;
	// rotation across passes, not paging, is what beats the result cap.
	firstPage = 1
	// forkFreshDivisor shrinks the push window for forks: a fork must have been
	// pushed within a quarter of Fresh, not the full window.
	forkFreshDivisor = 4
)

// ErrNoToken reports a Discover call without a token. The phase must be
// disabled rather than degraded: unauthenticated code search does not exist.
var ErrNoToken = errors.New("ghfind: token required")

// Options tune one GitHub discovery pass. Zero values take the defaults above.
type Options struct {
	Token         string
	HTTP          *http.Client
	MaxRepos      int
	FilesPerRepo  int
	AcceptPerRepo int
	MinNovel      int
	Fresh         time.Duration
	Concurrency   int
	SearchCode    int
	SearchRepo    int
	Logger        zerolog.Logger
}

// Cursor names where the rotating query walk stopped, so the next pass
// continues from there instead of replaying the head of the grids. Persisted in
// .crawler-state.json; a budget device, not a guarantee of coverage.
type Cursor struct {
	Code int `json:"code"`
	Repo int `json:"repo"`
}

// File is one accepted candidate: a raw URL whose body carries at least
// MinNovel endpoints the census and the earlier picks of this pass do not.
type File struct {
	Repo  string
	Path  string
	URL   string
	Size  int
	Nodes int
	Novel int
}

// Repo reports what one admitted repository contributed to the pass.
type Repo struct {
	Name     string
	PushedAt time.Time
	Accepted []File
	Probed   int
	Live     int
}

// Stats totals one pass. NovelEndpoints is the union of endpoints actually
// accepted — never a sum of per-file counts. Sleeps and Calls come from the
// API's rate limiter: wall time slept and API calls issued.
type Stats struct {
	SearchCalls, ReposSeen, ReposSkipped, Trees, Candidates, Probed, LiveFiles, Accepted, NovelEndpoints int
	Sleeps, Calls, Errors                                                                                int
}

// Input is everything Discover knows before the pass starts.
type Input struct {
	Census     *Census
	Productive []string
	Known      map[string]bool
	Cursor     Cursor
}

// Result is what the pass found and accepted. Cursor continues the walk where
// this pass stopped it.
type Result struct {
	Repos  []Repo
	Cursor Cursor
	Stats  Stats
}

// Finder runs GitHub discovery passes.
type Finder struct {
	opts   Options
	api    *API
	client *http.Client
	log    zerolog.Logger
}

// New builds a Finder, applying the default knob values to zero Options fields.
func New(opts Options) *Finder {
	if opts.MaxRepos == 0 {
		opts.MaxRepos = defaultMaxRepos
	}
	if opts.FilesPerRepo == 0 {
		opts.FilesPerRepo = defaultFilesPerRepo
	}
	if opts.AcceptPerRepo == 0 {
		opts.AcceptPerRepo = defaultAcceptPerRepo
	}
	if opts.MinNovel == 0 {
		opts.MinNovel = defaultMinNovel
	}
	if opts.Fresh == 0 {
		opts.Fresh = defaultFresh
	}
	if opts.Concurrency == 0 {
		opts.Concurrency = defaultConcurrency
	}
	if opts.SearchCode == 0 {
		opts.SearchCode = defaultSearchCode
	}
	if opts.SearchRepo == 0 {
		opts.SearchRepo = defaultSearchRepo
	}
	if opts.HTTP == nil {
		opts.HTTP = http.DefaultClient
	}
	return &Finder{opts: opts, client: opts.HTTP, api: NewAPI(opts.Token, opts.HTTP, opts.Logger), log: opts.Logger}
}

// Discover runs one discovery pass (docs/guides/github.md steps 2-7) and
// returns what it accepted. An empty token returns ErrNoToken; a nil census
// refuses the whole pass — nothing is searched and Stats.Errors is 1, because a
// pass without a baseline would mint blind. A cancelled ctx stops the pass and
// returns what was accepted so far wrapped in the ctx error.
func (f *Finder) Discover(ctx context.Context, in Input) (Result, error) {
	if f.opts.Token == "" {
		return Result{}, ErrNoToken
	}
	if in.Census == nil {
		return Result{Stats: Stats{Errors: 1}}, nil
	}
	p := &pass{f: f, ctx: ctx, in: in, now: time.Now()}
	p.discover()
	if err := p.ctx.Err(); err != nil {
		return p.result(), fmt.Errorf("ghfind discover: %w", err)
	}
	return p.result(), nil
}

// pass is the state of one Discover call. Everything Discover touches lives
// here, not on the Finder, so two passes never share scratch or counters.
type pass struct {
	f                  *Finder
	ctx                context.Context
	in                 Input
	now                time.Time
	st                 Stats
	codeLen, repoLen   int
	codeUsed, repoUsed int
	admitted           []*repoWork
	repos              []Repo
	union              map[uint64]struct{}
	countScratch       []uint64
}

// repoWork carries one admitted repository from admission to selection.
type repoWork struct {
	full   string
	branch string
	pushed time.Time
	paths  []Scored
	outs   []probeOutcome
}

// probeJob addresses one candidate fetch; each job's worker writes only that
// candidate's own probeOutcome slot, which is why the pool needs no locking.
type probeJob struct {
	repo int
	cand int
}

// probeOutcome is what one candidate fetch produced. attempted is false only
// when the pass was cancelled before the GET was issued; eps is an owned copy
// kept solely for files that classify live.
type probeOutcome struct {
	nodes     int
	eps       []uint64
	err       bool
	attempted bool
}

// discover runs the pass phases in order. Each phase stops early on a cancelled
// ctx; what was accepted so far is still returned.
func (p *pass) discover() {
	code, repo := p.search()
	if p.ctx.Err() != nil {
		return
	}
	p.admit(code, repo)
	if p.ctx.Err() != nil {
		return
	}
	p.trees()
	if p.ctx.Err() != nil {
		return
	}
	p.union = make(map[uint64]struct{})
	p.probe()
	p.selectFiles()
}

func (p *pass) result() Result {
	st := p.st
	st.Sleeps = int(p.f.api.Sleeps())
	st.Calls = int(p.f.api.Calls())
	return Result{
		Repos: p.repos,
		Cursor: Cursor{
			Code: advance(p.in.Cursor.Code, p.codeUsed, p.codeLen),
			Repo: advance(p.in.Cursor.Repo, p.repoUsed, p.repoLen),
		},
		Stats: st,
	}
}

// advance moves a grid cursor by the number of consumed queries, wrapping at
// the grid length. A zero-length grid leaves the cursor untouched.
func advance(cur, used, size int) int {
	if size <= 0 {
		return cur
	}
	return (cur + used) % size
}

// search walks the code-search grid from in.Cursor.Code for up to SearchCode
// queries and the repo-search grid from in.Cursor.Repo for up to SearchRepo,
// one page-1 call per query; rotation across passes, not paging, is what beats
// the per-query result cap. A rejected query is logged and skipped, not fatal.
func (p *pass) search() (code, repo []foundRepo) {
	codeGrid := CodeQueries()
	repoGrid := RepoQueries(p.now, p.f.opts.Fresh)
	p.codeLen = len(codeGrid)
	p.repoLen = len(repoGrid)
	return p.walkCode(codeGrid), p.walkRepo(repoGrid)
}

func (p *pass) walkCode(grid []string) []foundRepo {
	var out []foundRepo
	n, budget := len(grid), p.f.opts.SearchCode
	if n == 0 || budget <= 0 {
		return nil
	}
	start := p.in.Cursor.Code % n
	for used := 0; used < budget && used < n; used++ {
		if p.ctx.Err() != nil {
			break
		}
		q := grid[(start+used)%n]
		p.codeUsed = used + 1
		p.st.SearchCalls++
		hits, _, err := p.f.api.SearchCode(p.ctx, q, firstPage)
		if err != nil {
			if p.ctx.Err() != nil {
				break
			}
			if errors.Is(err, ErrBadQuery) {
				p.f.log.Warn().Str("query", q).Msg("ghfind: code-search query rejected")
				continue
			}
			p.st.Errors++
			p.f.log.Warn().Err(err).Str("query", q).Msg("ghfind: code search failed")
			continue
		}
		for _, h := range hits {
			out = append(out, foundRepo{full: h.Repo})
		}
	}
	return out
}

func (p *pass) walkRepo(grid []string) []foundRepo {
	var out []foundRepo
	n, budget := len(grid), p.f.opts.SearchRepo
	if n == 0 || budget <= 0 {
		return nil
	}
	start := p.in.Cursor.Repo % n
	for used := 0; used < budget && used < n; used++ {
		if p.ctx.Err() != nil {
			break
		}
		q := grid[(start+used)%n]
		p.repoUsed = used + 1
		p.st.SearchCalls++
		repos, _, err := p.f.api.SearchRepos(p.ctx, q, firstPage)
		if err != nil {
			if p.ctx.Err() != nil {
				break
			}
			if errors.Is(err, ErrBadQuery) {
				p.f.log.Warn().Str("query", q).Msg("ghfind: repo-search query rejected")
				continue
			}
			p.st.Errors++
			p.f.log.Warn().Err(err).Str("query", q).Msg("ghfind: repo search failed")
			continue
		}
		for _, r := range repos {
			out = append(out, foundRepo{full: r.Name, meta: r, haveMeta: true})
		}
	}
	return out
}

// foundRepo is one repository the search walk produced. haveMeta is false for
// code-search hits, whose payload carries no repository metadata.
type foundRepo struct {
	full     string
	meta     RepoMeta
	haveMeta bool
}

// admit applies the FRESH gate (docs/guides/github.md step 3) to the searched
// repositories: archived is refused, a repository must have been pushed within
// Fresh, and a fork within a quarter of that. Productive repositories come
// first and never consume the MaxRepos budget; then repo-search finds (their
// metadata arrived with the payload), then code-search finds, whose metadata
// is fetched here.
func (p *pass) admit(code, repo []foundRepo) {
	remaining := p.f.opts.MaxRepos
	seen := make(map[string]bool, len(p.in.Productive)+len(code)+len(repo))
	consider := func(fr foundRepo, productive bool) {
		if seen[fr.full] {
			return
		}
		if !productive {
			if remaining <= 0 {
				return
			}
		}
		seen[fr.full] = true
		meta, err := p.admitMeta(fr)
		if err != nil {
			p.metaFailed(fr.full, err)
			return
		}
		if !p.fresh(meta) {
			p.st.ReposSkipped++
			return
		}
		p.admitted = append(p.admitted, &repoWork{full: fr.full, branch: meta.DefaultBranch, pushed: meta.PushedAt})
		p.st.ReposSeen++
		if !productive {
			remaining--
		}
	}
	for _, full := range p.in.Productive {
		consider(foundRepo{full: full}, true)
	}
	for _, fr := range repo {
		consider(fr, false)
	}
	for _, fr := range code {
		consider(fr, false)
	}
}

// admitMeta returns the metadata admission gates read, fetching it when the
// search payload did not carry it: repo-search results arrive complete, code
// search pays one core call per repository it proposes.
func (p *pass) admitMeta(fr foundRepo) (RepoMeta, error) {
	if fr.haveMeta {
		return fr.meta, nil
	}
	return p.f.api.Repo(p.ctx, fr.full)
}

// metaFailed records a metadata fetch failure. A failure caused by the pass
// being cancelled is not an error of the pass: Discover reports the abort.
func (p *pass) metaFailed(full string, err error) {
	if p.ctx.Err() != nil {
		return
	}
	p.st.Errors++
	p.f.log.Warn().Err(err).Str("repo", full).Msg("ghfind: repository metadata failed")
}

// fresh reports whether a repository passes the push-window gate.
func (p *pass) fresh(m RepoMeta) bool {
	if m.Archived {
		return false
	}
	age := p.now.Sub(m.PushedAt)
	if age > p.f.opts.Fresh {
		return false
	}
	return !m.Fork || age <= p.f.opts.Fresh/forkFreshDivisor
}

// trees fetches one recursive tree per admitted repository and runs the
// candidate filter over it. A tree failure skips the repository, not the pass.
func (p *pass) trees() {
	for _, rw := range p.admitted {
		if p.ctx.Err() != nil {
			return
		}
		entries, truncated, err := p.f.api.Tree(p.ctx, rw.full, rw.branch)
		if err != nil {
			if p.ctx.Err() == nil {
				p.st.Errors++
				p.f.log.Warn().Err(err).Str("repo", rw.full).Msg("ghfind: tree failed")
			}
			continue
		}
		p.st.Trees++
		if truncated {
			p.f.log.Debug().Str("repo", rw.full).Msg("ghfind: truncated tree used as far as it goes")
		}
		rw.paths = Candidates(entries, p.f.opts.FilesPerRepo)
		p.st.Candidates += len(rw.paths)
	}
}

// probe fetches every candidate body once through a bounded worker pool. Each
// worker keeps one endpoint scratch, handed to probeFile by pointer and grown
// to that worker's running maximum: dropping the grown array would re-allocate
// every live candidate's endpoint slice from zero, which for the measured
// 300 KB base64 candidate is 14 allocations instead of 1.
func (p *pass) probe() {
	var jobs []probeJob
	for ri, rw := range p.admitted {
		rw.outs = make([]probeOutcome, len(rw.paths))
		for ci := range rw.paths {
			jobs = append(jobs, probeJob{repo: ri, cand: ci})
		}
	}
	if len(jobs) == 0 {
		return
	}
	workers := p.f.opts.Concurrency
	if workers < 1 || workers > len(jobs) {
		workers = len(jobs)
	}
	ch := make(chan probeJob)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			var scratch []uint64
			for j := range ch {
				p.probeFile(j, &p.admitted[j.repo].outs[j.cand], &scratch)
			}
		})
	}
	for _, j := range jobs {
		ch <- j
	}
	close(ch)
	wg.Wait()
}

// probeFile fetches one candidate raw URL and fills its outcome. Non-2xx,
// oversize and transport failures are recorded, not fatal; only a pass
// cancellation leaves the outcome untouched.
func (p *pass) probeFile(j probeJob, out *probeOutcome, scratch *[]uint64) {
	if p.ctx.Err() != nil {
		return
	}
	rw := p.admitted[j.repo]
	cand := rw.paths[j.cand]
	rawURL := RawURL(rw.full, rw.branch, cand.Path)
	reqCtx, cancel := context.WithTimeout(p.ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		out.err, out.attempted = true, true
		return
	}
	req.Header.Set("User-Agent", fetch.UserAgent())
	resp, err := p.f.client.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return // the pass was cancelled, not a probe failure
		}
		out.err, out.attempted = true, true
		p.f.log.Debug().Err(err).Str("url", rawURL).Msg("ghfind: candidate fetch failed")
		return
	}
	defer resp.Body.Close()
	out.attempted = true
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		out.err = true
		p.f.log.Debug().Str("url", rawURL).Str("status", resp.Status).Msg("ghfind: candidate fetch non-2xx")
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, subscription.MaxSubscriptionSize+1))
	if err != nil {
		out.err = true
		p.f.log.Debug().Err(err).Str("url", rawURL).Msg("ghfind: candidate body read failed")
		return
	}
	if len(body) > subscription.MaxSubscriptionSize {
		out.err = true
		p.f.log.Debug().Str("url", rawURL).Msg("ghfind: candidate body over the subscription size cap")
		return
	}
	// Normalize once: classify.Body and Endpoints each normalize their input,
	// and on a base64 or xray-JSON envelope that decode is the whole cost of
	// the parse (measured: 21 ms per 10 MB body, paid twice). Normalize is
	// idempotent — a body already carrying "://" comes back as itself — so the
	// verdict is still the one a worker fetch would reach over these bytes.
	norm := subscription.Normalize(body)
	out.nodes = classify.Body(norm, "", p.now.Unix()).Nodes
	if out.nodes == 0 {
		return
	}
	eps := Endpoints(norm, (*scratch)[:0])
	*scratch = eps
	if len(eps) > 0 {
		out.eps = append([]uint64(nil), eps...)
	}
}

// selectFiles assembles the Result entries in admission order and runs the
// per-repository cover and global novelty gate.
func (p *pass) selectFiles() {
	p.repos = make([]Repo, 0, len(p.admitted))
	for _, rw := range p.admitted {
		rep := p.selectRepo(rw)
		p.st.Probed += rep.Probed
		p.st.LiveFiles += rep.Live
		p.st.Accepted += len(rep.Accepted)
		p.repos = append(p.repos, rep)
	}
}

// liveCand is one live candidate of a repository during selection.
type liveCand struct {
	idx   int
	url   string
	eps   []uint64
	known bool
}

func (p *pass) selectRepo(rw *repoWork) Repo {
	rep := Repo{Name: rw.full, PushedAt: rw.pushed}
	var live []liveCand
	for i := range rw.outs {
		o := &rw.outs[i]
		if !o.attempted {
			continue
		}
		rep.Probed++
		if o.err {
			p.st.Errors++
			continue
		}
		if o.nodes > 0 {
			live = append(live, liveCand{
				idx: i,
				url: RawURL(rw.full, rw.branch, rw.paths[i].Path),
				eps: o.eps,
			})
		}
	}
	rep.Live = len(live)
	if len(live) == 0 {
		return rep
	}
	// A URL the crawler already manages is still probed — its endpoints join
	// the running union so a duplicate elsewhere cannot read as novel — but it
	// can never be reported accepted again.
	for i := range live {
		if p.in.Known[live[i].url] {
			live[i].known = true
			p.fold(live[i].eps, false)
		}
	}
	p.pick(rw, &rep, live)
	return rep
}

// pick runs the per-repository greedy cover (docs/guides/github.md step 7) and
// then the global MinNovel gate over the cover picks, best first.
func (p *pass) pick(rw *repoWork, rep *Repo, live []liveCand) {
	sets := make([][]uint64, len(live))
	for i := range live {
		sets[i] = live[i].eps
	}
	order := Cover(sets, p.coverFloor(sets), p.f.opts.AcceptPerRepo)
	for _, k := range order {
		c := &live[k]
		if c.known {
			continue
		}
		novel := p.novel(c.eps)
		if novel < p.f.opts.MinNovel {
			continue
		}
		cand := rw.paths[c.idx]
		rep.Accepted = append(rep.Accepted, File{
			Repo:  rw.full,
			Path:  cand.Path,
			URL:   c.url,
			Size:  cand.Size,
			Nodes: rw.outs[c.idx].nodes,
			Novel: novel,
		})
		p.fold(c.eps, true)
	}
}

// coverFloor is the minimum per-repository cover gain: at least coverFloorMin
// endpoints or coverFloorPercent of the repository's endpoint union.
func (p *pass) coverFloor(sets [][]uint64) int {
	floor := coverFloorMin
	p.countScratch = p.countScratch[:0]
	for _, s := range sets {
		p.countScratch = append(p.countScratch, s...)
	}
	slices.Sort(p.countScratch)
	distinct := 0
	for _, h := range p.countScratch {
		if distinct == 0 || h != p.countScratch[distinct-1] {
			p.countScratch[distinct] = h
			distinct++
		}
	}
	if pct := distinct * coverFloorPercent / percentDivisor; pct > floor {
		floor = pct
	}
	return floor
}

// novel counts how many endpoints a file adds beyond the census and everything
// accepted (or already managed) earlier in this pass.
func (p *pass) novel(eps []uint64) int {
	n := 0
	for _, h := range eps {
		if p.in.Census.Has(h) {
			continue
		}
		if _, dup := p.union[h]; !dup {
			n++
		}
	}
	return n
}

// fold adds a live file's endpoints to the running union. Known (already
// managed) files fold without counting; accepted files fold with counting, so
// Stats.NovelEndpoints stays the union of genuinely new inserts.
func (p *pass) fold(eps []uint64, count bool) {
	for _, h := range eps {
		if p.in.Census.Has(h) {
			continue
		}
		if _, dup := p.union[h]; dup {
			continue
		}
		p.union[h] = struct{}{}
		if count {
			p.st.NovelEndpoints++
		}
	}
}
