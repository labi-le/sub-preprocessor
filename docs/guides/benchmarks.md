# Measurement discipline

> **When to read this:** Read before quoting a performance figure or taking one. This file is mostly a list of ways measurements here have been wrong, including figures that had to be retracted.

## Bench / performance notes

- Benchmark results are stored in `./benchmarks/bench-<UTC timestamp>.txt`. That directory
  is gitignored, so a figure quoted out of a snapshot is unreachable from a fresh checkout:
  cite the mechanism, or re-measure with `nix-shell --run "make bench"`
- **Two benchmarks moved because their FIXTURE moved, and both times the fixture was the
  whole of it.** `BenchmarkProcessBodyPipeline`'s annotate list lives in `newBenchProcessor`
  (`internal/preprocess/pipeline_bench_test.go`) and `BenchmarkAnnotate`'s in
  `annotator_bench_test.go`. Both name the annotate tags, so removing a tag edits them:
  `5d06fb6` (the `IP` tag) took the pipeline 4640 -> 1600 B/op, and the `ASN` tag removal
  took `BenchmarkAnnotate` 48 -> 24 B/op. **Neither is an allocation win the code earned.**
  Controlled both times against a hybrid tree holding the fixture constant, the fixture was
  100% of each B/op move. Every `benchmarks/` snapshot older than `5d06fb6` records
  `4640 B/op`, so diffing a fresh `make bench` against one reads a 65% win that does not
  exist; the post-`5d06fb6` pipeline baseline is **~16200 ns/op**, not the hybrid's ~15900.
- **A sub-2% ns delta on these benchmarks is not measurable here and MUST NOT be quoted as
  a result.** The variable is the linked image, not the session: two `git archive` exports
  of byte-identical source, relinked, disagree by up to ~2% on a must-be-zero null control,
  while each individual link holds its own offset to a handful of ns across sessions hours
  apart. So a clean null control proves nothing - it says only that this link sits near its
  control - and re-running only sharpens a constant you cannot attribute. **Re-export and
  RELINK the null control; if two links of one source disagree by more than the delta you
  are chasing, that delta is unmeasured.** This was paid for twice: a +1.8% regression
  attributed to the `IP` removal and a -0.75% win attributed to the `ASN` one. Neither
  survived a relinked control. What DOES survive is the static shape - `go tool objdump`
  shows the annotate dispatch going from a 3-case switch to a 2-case one to a guard clause
  (`if t.key != config.TagGEO { continue }`) as the two tags were removed, seven immediate
  compares down to three. That is an argument about work removed, not about nanoseconds.
- **Allocations are the binding constraint and they are clean across both changes**: no
  `allocs/op` or `B/op` increase on any benchmark. A handful in untouched packages
  (`Parse_1000Entries`, `ParseProxies`, `Parse_Vmess`, `Resolution`, `Resolution_Concurrent`,
  `Merge`, `MergeSSR`, and `CacheStore_RWMutexMap/parallel` / `CacheStore_SyncMap/parallel`)
  differ between trees at `-benchtime 100x` while the SAME tree
  reproduces the same spread across consecutive runs - `Resolution_Concurrent` even flips
  64/65 allocs/op - so that is the instrument, and the honest comparison diffs the TOUCHED
  packages rather than the whole list. Do NOT replace that with a quoted range: the two
  `CacheStore/parallel` cases have a STABLE FLOOR (13 B/op) and a ceiling that is a draw of
  the instrument, because `B/op` is a total divided by 100 iterations spread over 16 P's, so
  a handful of fixed per-P costs lands the quotient anywhere. Three `-benchtime 100x
  -count=20` sweeps, two links: RWMutexMap 13 x10 / 14 x5 / 28,57,71,85,85, then 13->71,
  then 13 x9 / 14 x2 up through 43,53,72,77,86,91; SyncMap 13->86, then 13->61, then
  13 x15 / 14 x3 / 53,76. Same floor every time, a different ceiling every time - which is
  why the pair that used to be quoted here ("13 <-> 52") was outside its own band by the
  second sweep. Name the floor and the mechanism, quote no ceiling.
  The one deliberate increase is in `countryChainOrder`: a config splitting
  one `GEO` chain across several entries now pays a per-request `chainLookup` it did not
  before (0 -> 56 B/op, 2 allocs), which is exactly what the equivalent single-entry chain
  already cost. The shipped config is unmoved, and the alternative was the wrong filter
  verdict - see `Processor.countryChain`.
- **STRUCK by explicit agreement, after six review rounds:** the per-link median tables,
  per-session interquartile ranges and A/B/C/D build enumerations that used to fill this
  section. They were measured in `/tmp` export trees that no longer exist, so no reader
  could check them from a fresh checkout - which is the rule at the top of this file.
  They existed to support a NULL result, so the apparatus could never be load-bearing and
  could only be wrong in new ways; each rewrite minted the next round's findings ("five
  commits" was seven, "four independent links" was three links and a re-run, a tree that
  "reproduced exactly" was a different tree). The three bullets above are what four
  independent reviewers verified and what stands without figures. Re-measure with
  `nix-shell --run "make bench"` rather than trusting a number here.
- **The 2026-08-18 allocation wave.** Every pair below mixes two provenances, and the
  difference is the point: the BEFORE number is the pre-wave baseline the change that landed it
  measured, and the AFTER number is a re-reading of the SETTLED tree — `go test -run '^$'
  -bench <name> -benchmem -count=5 ./internal/<pkg>`, medians, 2026-08-18, this machine.
  Chaining the individual per-change deltas does NOT reproduce these, because several of the
  paths sit inside one another; re-read the benchmark instead. ALLOCATION counts, not ns/op: a
  fresh checkout reproduces one by re-running the named benchmark, which is exactly what the
  struck ns/op apparatus could not offer.
  - `internal/metrics` — `BenchmarkWriteSources` 24912 -> 3 allocs/op. The renderer appends
    into one self-flushing byte buffer instead of calling `fmt.Fprintf` per sample, which
    boxed every argument into an `[]any` and heap-copied the string headers.
  - `internal/preprocess` — `BenchmarkCollectSurvivors_Permissive` 67340 -> 2664,
    `BenchmarkProcessBodySlice_ManySmallSources` 22765 -> 1413. Survivor lines are interned
    into a chunked arena instead of one `strings.Clone` per node.
  - `internal/subscription` — `BenchmarkParse_SSR` 400 -> 100,
    `BenchmarkRewriteVmessName` 41 -> 3,
    `BenchmarkNormalizeParse_XrayJSON160Outbounds` 6750 -> 3067. `queryList`, an array-backed
    `url.Values`, replaced the map form on the ssr and xray paths, and a new display name is
    spliced over the old `ps` rather than marshalled from a decoded document.
  - `internal/crawl` — `BenchmarkHarvestPages/guarded` 387 -> 21, `/inline` 43 -> 11 and
    691746 -> 60024 B/op. Hand-scanned URL and inline-node extraction plus `unescapeInto`,
    where the regexps and `html.UnescapeString` were. (`guarded` was recorded at 27 the day it
    landed and reads 21 here — which is why the AFTER column is a re-reading and not a record.)
  - `internal/fetch` — `BenchmarkReadBody/median_41704/unannounced` 17 -> 2. `readChunked`
    replaced `io.ReadAll` on the path a body announcing no length takes.
  - `internal/stable` — `BenchmarkMerge` 6228 -> 764 (202601 B/op), `BenchmarkBuildPayload`
    1695 -> 598 (109464 B/op), and `BenchmarkMergeSSR` now at 924. FOUR changes land in those,
    not one: `relabelNode` returning its label into the caller's buffer with `speedPrefix`
    rendering into a renderer-owned one (5791 and 898 at that point), then the vmess splice one
    entry up, which `Merge` reaches through `subscription.RewriteVmessName` inside `relabelNode`
    (`merge.go:213-221`), then a `keyArena` cutting each dedupe key out of a 1 KiB block instead
    of allocating it (`merge.go:110`), then `relabelNode` cutting the vmess/ssr label from that
    same arena (837 -> 764, 997 -> 924). **`Merge` is the worked example of why this is not a
    chain of deltas:** the same benchmark has four baselines, depending on when you asked.
  **`Merge` is on that list, so read the "untouched packages" enumeration above as relative
  to the two TAG-REMOVAL commits and not to this wave**: diff the packages the change in
  front of you touched, never a membership list written for another change.
  **Both pipeline figures in the first bullet — the `1600 B/op` and the `~16200 ns/op`
  baseline — are PRE-WAVE readings.** `BenchmarkProcessBodyPipeline` runs through the
  annotate path, and this wave touched it: `rewrite.NodeName` writes the tag prefix, its
  space and the name straight into the caller's buffer (`rewrite.go:58-63`), leaving a
  contiguous name to be built only by the vmess/ssr payload arms (`displayName`). Re-read on the
  settled tree it allocates NOTHING — 0 B/op, 0 allocs/op over its 100 nodes — so the
  `4640 -> 1600` arithmetic and the "65% win that does not exist" describe a tree at `5d06fb6`
  and say nothing about this one.
  **Two ns/op LOSSES came out of this wave on source it does not touch, and they are
  recorded here rather than against either path.** `BenchmarkParse_SSLegacy` reads 3623 and
  3636 ns/op on two links of HEAD against 3776 and 3876 on two links of the settled tree
  (+5.4% on the median of medians, `1600 B/op` and `50 allocs/op` identical either side),
  and `BenchmarkStripKnownTags/untagged` reads 2.641 and 2.590 against 2.866 and 2.889
  (+10.0%, 0 allocs either side): four exports to separate `/tmp` paths, relinked,
  `-count=5` medians, 2026-08-18, this machine. The relink control the third bullet above
  demands puts two links of ONE tree within 0.4% and 2.6% for SSLegacy and 2.0% and 0.8%
  for StripKnownTags, so both deltas clear their own band; an independent four-link sweep at
  `-count=8` read the same gaps as +7.3% and +10.3% and reproduced both with `-pgo=off`, so
  a stale `default.pgo` is not the cause. **Neither function was edited** —
  `internal/subscription/ss.go` is untouched, and `StripKnownTags`' body is unchanged inside
  a `rewrite.go` whose whole diff is `NodeName` and the new `displayName` — so no work was
  added and the delta is the linked image moving as `query.go` joined package `subscription`
  and `displayName` joined package `rewrite`. Weight: `StripKnownTags` runs once per node
  `rewrite.NodeName` renders, ~0.08 ms per cycle at 300000 nodes, and the SSLegacy delta is
  ~0.2 µs per ss-legacy line; each sits in a package this wave took a large allocation win
  in — `internal/subscription` in its own sub-bullet above, `internal/rewrite` in the
  pipeline note immediately before this one. **Do not attribute either figure to the
  function it names.**
- **The probe's parse moved behind the TCP pre-check (2026-08-18, this machine, `-count=5`
  medians unless stated).** `Probe` used to build a mihomo adapter object for every converted
  mapping and then let the pre-check condemn most of them unread; it now derives the pre-check's
  whole input from the raw mapping (`probeNodes`) and parses the survivors alone (`parseLive`).
  Every figure below was re-read for this entry on the settled tree — the reorder plus the
  `probeNodes` short-circuit that skips `probeAddr` for a mapping the pre-check will never dial —
  against a snapshot of the pre-reorder worktree, two `/tmp` exports of each.
  - `BenchmarkParseProxies` is the UNCHANGED work — the checker's survivor parse, which is the
    whole payload — and it holds over the same 300 nodes: 5880736 -> 5880414 B/op, allocations
    unmoved. Read the counts as unmoved rather than as -1: each side flickers by one, the
    pre-reorder tree reading 54794/54795 and the settled tree 54793/54794. The -322 B is the
    index-aligned `[]bool` of TCP verdicts that no longer exists — `make([]bool, 300)` lands in
    the 320-byte size class.
  - `BenchmarkProbeParseCondemned` is the new work, the same 300-node payload with
    `benchCondemnedPercent` of the positions condemned: **29656 allocs/op and 3134816 B/op
    against the 54794 / 5880736 the eager front-end spent, -45.9% and -46.7%.** Scaled to the
    8817-node production probe set that is ~1610000 -> ~872000 allocations and ~172.8 -> ~92.1 MB
    per cycle, so ~739000 allocations and ~81 MB saved. Every B/op here is a median of a spread
    tens of bytes wide, while the allocation counts are exact and repeat.
    `BenchmarkSelectSurvivors` (73728 B/op, 1 alloc) and `BenchmarkMerge` (764 allocs/op, B/op
    flickering over the same 7-byte spread on both sides) are unmoved.
  - **That share is a NODE share, and the constant is now seeded from one.** The condemned share
    is a benchmark constant (`benchCondemnedPercent`, 55.8), not a dial: a benchmark cannot own
    the network. Re-derive it from `stable_probe_outcome_nodes{stage="condemned"}` over
    `stable_probed_nodes` — NEVER from `precheckBreakerPercent`, whose 58.9% is refused/judged
    over DISTINCT ENDPOINTS, which `PrecheckReport` states outright is not interchangeable with
    the `stage="condemned"` node count: every position the pre-check never judged is condemned by
    neither, UDP-typed nodes and the ~5.2% of endpoints whose name does not resolve
    (`filterReachable`) included, which caps the node share at 0.589 * 0.948 = 55.8%. **This
    bullet quoted the endpoint share until 2026-08-18**: at 58.9% the same benchmark reads 28263
    allocs/op and 2986025 B/op, so the wrong denominator flattered the change by 4.7% on
    allocations and 4.8% on bytes.
  - **The floor is a small LOSS, and it is the healthy-upstream case.** At 0% condemned the
    reorder pays a second pass over the mappings and a `JoinHostPort` per position that the
    spared adapter then builds again: 55395 allocs/op (55394 on some runs) and 5902783 B/op
    against the eager front-end's 54794 / 5880736 over the same 300 nodes, **+1.10% and +0.375%**
    — +2 allocations and ~74 B per position, ~+17700 allocations and ~650 KB per cycle at 8817
    positions. Break-even is 1.30% condemned: 154.1 allocations saved per condemned position
    against 2 paid per position, so every real cycle is far past it. The fixture is 100%
    TCP-typed, which is why the `probeAddr` short-circuit cannot move this floor — a UDP-typed
    share only lowers it.
  - **`BenchmarkFoldProbeResults` holds its 893616 B/op and 35 allocs/op and costs ~2.2% more
    ns/op**, four links, `-count=11` medians: 168611 and 168069 pre-reorder against 171484 and
    172640 settled, over a relink control of 0.32% and 0.67%, and all 22 pre-reorder samples sit
    below all 22 settled ones. Attributable, unlike the two ns losses recorded above: the fold
    now strides a 56-byte `probeNode` where it strode a 16-byte `mihomo.Proxy` — 494 KB against
    141 KB over the 8817-node fixture — plus a per-position switch. The four cross pairs span
    +1.70% to +2.72%, so the median of medians clears both the relink control and this file's 2%
    floor, which is what makes silence the wrong record here; the weight is ~+3.7 µs once per
    cycle, which is why it is recorded and not chased.
  - **The ceiling is upstream and this does not touch it.** mihomo's reflective struct decoder is
    90.4% of the objects and 81.4% of the bytes `adapter.ParseProxy` allocates: 48.2% of the
    objects are it re-splitting a compile-time-constant `proxy:"..."` tag
    (`common/structure/structure.go:449` and `:487` at mihomo v1.19.27) and a further 21.8% are
    the `reflect.ValueOf(fieldName)` box at `:504` — 485450 and 219450 objects out of
    `ParseProxy`'s 1007010, `-benchtime=50x -memprofilerate=1`, `alloc_objects`. Reuse is not the
    lever either: `structure.Decoder` is stateless (`struct{option *Option}`) and
    `adapter/parser.go:12` mints one per proxy, but that mint allocates NOTHING — it does not
    escape, with `-pgo=off` too — and both costs above are per-`Decode`. So the parse that remains
    costs what it always did per node, and nothing here is reachable without vendoring
    `ParseProxy`'s dispatch switch.
  - **`BenchmarkMergeSSR` and `BenchmarkBuildPayload` are `183896 B/op` / `924 allocs/op` and
    `109464 B/op` / `598 allocs/op` on both sides**, which is the whole load-bearing content.
    Neither runs edited code — `select.go` is byte-identical across the change and `merge.go`'s
    only diff is a comment — so any ns gap on them is a relink comparison by construction and none
    is attributed to this change. **The +4.8% / +5.3% pair this bullet quoted until 2026-08-18,
    and the 5.6% / 6.3% relink control offered against it, reproduce on none of the four links
    either side and are retracted**: per-link ns figures minted to support a null result are the
    apparatus this file struck above.
- Earlier waves, mechanisms only (no figure for them is reachable from a checkout): geofeed
  parsing allocations, fragment-rewrite allocations, inner filter hot-path allocations, and
  skipping non-URI lines during subscription parse.
- Still-hot areas worth revisiting before large refactors:
  - `internal/subscription.Parse` — still the widest per-line path, though `Parse_SSR` is 4x
    lighter since 2026-08-18
  - `internal/filter.ParseAllowed`
  - IPv6 support in resolver/filter path is still incomplete / not yet generalized
