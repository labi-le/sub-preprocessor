# Review loop — the evidence

> **When to read this:** Read before your FIRST edit to production code, to a contract (exported API, config key, metric name, published output shape), or to a document agents act on. `AGENTS.md` carries the rules; this file carries why each one exists and what it cost to learn.

## Workflow — mandatory, not advisory

Every change to production code, to a contract (an exported API, a config key, a metric
name, a published output shape), or to a document agents act on (`AGENTS.md`, `routes.md`,
`README.md`) MUST go through this loop:

> **implement → review (performance + architecture, as SEPARATE passes) → fix → repeat**

**Write the agreement down BEFORE the first implement step** — a numbered list of the agreed
points, plus what is explicitly out of scope, so a round is not spent proposing redesigns
nobody asked for. Without that list "100% of what was agreed" is unfalsifiable in both
directions: a reviewer can assert a point was agreed, an implementer can assert a dropped
point never was, and neither can be disproved.

The loop terminates on two conditions, both required: a full round returns **zero** open
findings, and **every numbered point is either implemented or struck by explicit agreement**,
the strike recorded in the list. Not "the happy path works". Not "tests pass".

**Proportionality.** The gate is risk, not diff size. A low-risk change — docs-only,
config-only — takes ONE pass, and that pass is the ARCHITECTURE one: a document or a key
that points at the wrong seam sends the next agent there, and neither has a hot path. Never
leave which pass survives to the agent's guess. Documents agents act on are IN scope
precisely because they fail this way — this very section is a docs-only change, it was given
a review pass, and it needed one: two of its load-bearing claims were false, both taken from
a report about an attempt that was rejected and never committed, so no reader could check
them. The termination conditions NEVER relax: a one-line change with an open finding is not
done either.

**A finding may be REFUTED, not only fixed — and the loop must allow it.** A round is clean
when every finding is either fixed or disproved with evidence, and a wrong finding
implemented is a regression the process invited. This is not hypothetical here: a reviewer
demanded dropping the ssr port-range check because `adapter.ParseProxy` supposedly enforces
it — measured, `NewShadowSocksR` has no range check at all and accepts `0`, `-1`, `70000`.
Another demanded rejecting a `mierus://` link whose first `port` is empty — measured,
mihomo `continue`s its INNER per-port loop and serves a working proxy from a later port, so
rejecting would have dropped usable nodes. Verify a finding against the code before
implementing it; say so with evidence when it is wrong. When the refutation is ITSELF
disputed, neither side closes the finding unilaterally: it goes to whoever owns the numbered
list — the agent that set the scope — who rules and records the ruling in that list.

**A green suite is not evidence, and MUST NEVER be reported as a review result.** A review
earns its cost only by trying to break the change; mutation is the cheapest way and is
expected — revert the fix, confirm the new test fails, restore. The restore is not done
until an empty `git diff` against the pre-mutation tree says so, and that check MUST run
before the round is reported: the mutation deliberately leaves production code wrong, and
agents here work in worktrees off a parent whose `./config` is bind-mounted read-write into
a running container (`docker-compose.yaml:14`). **Mutate in a `git archive` export under
`/tmp`, never in the shared worktree.** A mutation is a deliberately wrong tree and
concurrent agents are the normal mode here, so a peer reading the shared checkout
mid-round sees a defect that does not exist — this round one reviewer's harness showed
another a red they could not reproduce. Isolation makes the empty-diff check vacuous
rather than optional: it still binds anything mutated in place, and in place is what you
must avoid — and an untracked package (`internal/cidrset` on this branch) has no `git
diff` to be empty at all, so there the check is `cmp` against a pre-mutation copy.
Figures measured in an export tree describe that tree, which is why none of this round's
mutation numbers reached a document. Mutation has repeatedly found tests here
that could not fail at all: the mieru outcome fold shipped with three tests that looked like
coverage while neutering both production fold conditions left the package green, and
replacing `betterTraceOutcome`'s body with `return a.name < b.name` also left it green
(`364a50d`) — every ranked case sorted identically under either key.

**Why review at all when CI is green.** The `geotrace` filter once shipped completely inert,
and stayed that way from `11e5ca3` to `e554307`. `go test ./...`, `-race` and
`golangci-lint` were all clean the whole time. `swapTagValues` stopped scanning at the first
space between tags while `rewrite.LeadingTags`, its own input source, skipped that
whitespace — and because `bandwidth` ran before `geotrace` and prepended `"[SPD:<n>M] "`,
every survivor in production arrived space-separated and came back untouched. Nothing
dropped, nothing warned, and the `corrected` counter read 0, which an operator reads as "the
offline chain was right" — the opposite of the truth. No test caught it: every fixture used
the pre-bandwidth name shape. (There is no `geotrace` filter now, and no `geotrace` at all: `abf452b` deleted
`nodefilter_trace.go` and moved it into the annotate chain, which this branch then renamed to `cloudflare`. The
two mentions BEFORE this parenthetical name the symbol as it was spelled at the time, so the commits stay
findable.)

**Why performance is its OWN pass.** A benchmark that moved is not a regression until the
changed code is shown to be on its path. On that same change the benchmark that moved most
was `BenchmarkParse_SkipsNonURILines`, which feeds 50 lines containing no `://`
(`internal/subscription/subscription_bench_test.go:93`) — `parseNode` is never called, so it
cannot execute one changed instruction and what moved was binary layout. Telling that apart
from real cost takes a hybrid tree (new production code, old test files) and `go tool
objdump` on the suspect functions. Without that pass the branch either ships a regression or
gets rewritten to chase a phantom. The inverse is just as real, and derivable without
running anything: `SelectSurvivors` does one `make([]Survivor, 0, len(entries))`, so its
B/op is `len(entries) * unsafe.Sizeof(Survivor{})` rounded up to the allocator's 8 KiB page
multiple — at the benchmark's `n = 500` and today's 136-byte `Survivor`, 73728 B/op. That
step function has been paid twice. `4ed8009` grew `Survivor` 80 -> 88 bytes for one added
`bool` and moved it 40960 -> 49152 B/op — **+20%**. **Allocations are the binding
constraint:** an `allocs/op` or `B/op` increase is a BLOCKING FINDING the change MUST
justify and the reviewer MUST accept before the round closes. A finding, not a prohibition,
because that 20% was paid deliberately: `0f7af54` then swapped the bool for the code itself
(88 -> 96 bytes, the same 49152 B/op) and the field is now `Entry.Country`, which feeds
`keptCountries` and `stable_kept_country_nodes`. An agent reading the rule as absolute would
have blocked it. `ns/op` is noisy and means nothing without a control: benchmarks here drift
several percent run to run over code whose allocation counters do not move at all.

**Why architecture is its OWN pass.** `8d2c3a5` fixed, in ONE change, all four places where
a mihomo proxy name was matched against an `Entry.Label` — mihomo expands one `mierus://`
link into one proxy per configured port, so every bridge missed for mieru. Every site was
locally correct afterwards and slice-scoped review had nothing left to say. It still left a
seam only a whole-package reader could see: the shared proxy map stayed
`map[string]mihomo.Proxy`, so a multi-port `mierus://` survivor reached the through-node
filters as whichever port mihomo emitted LAST — undoing the best-of-N choice the latency
probe had deliberately just made. `448279b` repaired it by folding on the outcome side
instead of collapsing the proxy set. A reviewer given one slice structurally cannot see
that; someone MUST look at the whole seam, especially where parallel work merges. Five such
bridges exist today — `checker.go`, `prober.go`, `prober_api.go`, `prober_bandwidth.go`, and
`prober_trace.go`'s `TraceCheck` winner map, which was born `entryLabel`-correct after
`8d2c3a5`. (`applyFilters` is now `filterAndMeasureEgress`, renamed by `abf452b`.)

**Why "100% of what was agreed" is its own condition.** "It works" is not "it is done". All
of these passed a working-feature check and were still defects: `Entry.Country` kept its
pre-trace value, so `/stable.txt` published `[GEO:DE]` while `stable_kept_country_nodes`
still counted that node as CA; `corrected`/`unanswered` rode `FilterReport.Dropped` and so
appeared on a Grafana panel titled "drops by reason" for a filter that drops nothing (both
shipped by `b545d0a`, both corrected in `e554307`); `betterTraceOutcome`'s test exercised
only the tiebreak, so its primary key could be deleted with the suite green (`364a50d`).

**Comments are a first-class review target,** not polish — and so are the documents, which
rot the same way with nothing compiling against them. The recurring defect here is the
true-when-written claim. `routes.md`'s `parseNode` entry kept pointing at
`stable.tagCountry` (`merge.go`) for the aliasing rule after `abf452b` deleted that
function, and it survived a dedicated routes.md staleness pass (`322ee2a`) that rewrote
three other lines; the commit carrying this section is what finally fixed it. One comment
said the trace endpoint answers a fixed "211 bytes" — it has no fixed length, the endpoint
echoes the request User-Agent back, 202 and 301 bytes for two of them (`8d11c57`). One
claimed mihomo cannot emit a proxy shape a leading-zero mieru port makes it emit; rare, not
impossible (`bed264c`). Per the code conventions in [`AGENTS.md`](../../AGENTS.md), a stale comment is worse than none, so a
review that approves the code and ignores its comments — or the docs its change falsified —
has not finished.
