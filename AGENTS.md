# Agent Instructions

## Running commands

Always run project commands via `nix-shell` — the toolchain (Go version, linter, etc.)
is defined in `shell.nix`. Running tools directly may use different versions or fail.

Prefer Makefile targets for common flows:

```bash
nix-shell --run "make"
nix-shell --run "make test"
nix-shell --run "make fmt"
nix-shell --run "make race"
nix-shell --run "make bench"
```

## Workflow — mandatory, not advisory

Every change to production code, to a contract (an exported API, a config key, a
metric name, a published output shape), or to a document agents act on
(`AGENTS.md`, `routes.md`, `README.md`, the guides below) goes through:

> **implement → review (performance + architecture, as SEPARATE passes) → fix → repeat**

- **Write the numbered agreement down BEFORE the first implement step**, plus what is
  explicitly out of scope. Without it "100% of what was agreed" is unfalsifiable in
  both directions.
- **The loop terminates on two conditions, both required:** a full round returns
  **zero** open findings, and **every numbered point is either implemented or struck
  by explicit agreement**, the strike recorded in the list. Not "the happy path
  works". Not "tests pass".
- **A finding may be REFUTED with evidence, not only fixed.** A wrong finding
  implemented is a regression the process invited. Verify a finding against the code
  before implementing it. When the refutation is itself disputed, it goes to whoever
  owns the numbered list.
- **A green suite is NOT evidence and MUST NEVER be reported as a review result.**
  Mutation is expected: revert the fix, confirm the new test fails, restore.
- **Mutate in a `git archive` export under `/tmp`, never in the shared worktree** —
  concurrent agents are the normal mode here, and a mutation is a deliberately wrong
  tree. Untracked packages have no `git diff` to be empty, so there the check is
  `cmp` against a pre-mutation copy.
- **Proportionality: the gate is risk, not diff size.** A docs-only or config-only
  change takes ONE pass, and that pass is the ARCHITECTURE one. The termination
  conditions never relax: a one-line change with an open finding is not done either.
- **Allocations are the binding constraint.** An `allocs/op` or `B/op` increase is a
  BLOCKING finding the change must justify and the reviewer must accept.
- **Comments and the docs are first-class review targets.** The recurring defect here
  is the true-when-written claim. A review that approves the code and ignores its
  comments — or the docs its change falsified — has not finished.

Why each of these exists, and what shipping without it cost, is in
[`docs/guides/workflow.md`](./docs/guides/workflow.md). Read it before your first
edit; the rules above are the summary, not the argument.
## Code conventions

- **Comments earn their place.** Write one only for what the code cannot say itself: the *why* (rationale, tradeoffs), non-obvious invariants, ordering/locking/concurrency rules, units, edge-case semantics, gotchas, or external behavior (mihomo quirks, SSRF, protocol details).
- **Never restate the code.** Delete doc blocks that only echo the name or signature (`// name returns the name`, `// NewX creates a new X`, `// Close closes it`) or narrate the next line. Self-explanatory code gets no comment.
- **A stale comment is worse than none.** Changing behavior means updating the comment or deleting it — never leave it describing the old world.
- Removing an obvious doc comment is lint-safe: `.golangci.yml` excludes revive's "exported … should have comment", and `godot` needs no trailing period. Comments that remain must still start with the symbol's name (`godoc`/`staticcheck`). staticcheck's SA5011 is off in `_test.go` only — it does not model `t.Fatal` as terminating, so `if x == nil { t.Fatal(...) }` followed by using `x` reads as a nil deref; it stays on for production code.

## Guides — read on demand

`AGENTS.md` holds only what applies to every task. Everything else is a guide you
load when its trigger fires. Each guide states its own trigger at the top.

| Guide | Load it when |
|---|---|
| [`docs/guides/workflow.md`](./docs/guides/workflow.md) | before your FIRST edit to production code, a contract, or a doc agents act on |
| [`docs/guides/design.md`](./docs/guides/design.md) | changing pipeline behaviour, adding a scheme or decoder, touching probe/filter order, or asking "why is it built this way"; also what the endpoints guarantee |
| [`docs/guides/config.md`](./docs/guides/config.md) | adding, renaming or retuning a config key, or editing either shipped `config.yaml` |
| [`docs/guides/monitoring.md`](./docs/guides/monitoring.md) | adding, renaming or rendering a metric, or editing `deploy/grafana/sub-preprocessor.json` |
| [`docs/guides/benchmarks.md`](./docs/guides/benchmarks.md) | quoting a performance figure, or taking one |
| [`docs/guides/sources.md`](./docs/guides/sources.md) | adding, removing or auditing a subscription source, or touching `internal/crawl` |
| [`docs/guides/security.md`](./docs/guides/security.md) | touching `internal/fetch`, a user-supplied URL, or the SSRF gates |
| [`docs/guides/layout.md`](./docs/guides/layout.md) | orientation: which package owns a concern |
| [`routes.md`](./routes.md) | orientation before a change, and after adding, removing or restructuring a package, or changing a package's public API (key types, constructors, interfaces) — the per-package reference: types, functions, tags, dependency graph |

Two rules about the guides themselves, both learned the hard way:

- **A guide is a surface you FIX, never a source you cite.** Code is the only ground
  truth. If a guide disagrees with the code, the guide is wrong — correct it in the
  same change rather than working around it.
- **Update the guide your change falsified, in that change.** `routes.md` kept
  pointing at a function `abf452b` had deleted, and survived a dedicated staleness
  pass that rewrote three other lines around it.
