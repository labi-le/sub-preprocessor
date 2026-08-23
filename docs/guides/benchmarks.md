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
  - `internal/crawl` — `BenchmarkHarvestPages/guarded` 387 -> 21 for this rewrite, then
    21 -> 16 across the naming cutover that segmented the walk; `/inline` 43 -> 11 and
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
    (`merge.go:213-219`), then a `keyArena` cutting each dedupe key out of a 1 KiB block instead
    of allocating it (`merge.go:114`), then `relabelNode` cutting the vmess/ssr label from that
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
- **The naming cutover, 2026-08-18, crawl arms re-read 2026-08-19 (`-count=5` medians, this
  machine, AMD Ryzen 7 9800X3D).** Four `/tmp` export trees, each relinked — HEAD `8c8ec21`, the
  settled tree, the pre-retype tree carrying `origin.Post` as a string plus one post-id clone per
  keeping message ("the clone tree" below), and the settled tree with only `sourceLabelBytes`
  reverted. Every figure below was taken for this entry, and the crawl arms were taken twice, by
  two runs that shared no export tree: every settled-column allocation figure reproduced exactly,
  `segmented`'s settled ns/op did not repeat to the digit (see that bullet), and the HEAD
  `segmented` row exists only because the fixture was ported into a HEAD export — done twice now,
  to readings that disagree, the later one superseding (see that bullet). Where two readings
  stand, both are named. **Then `cand`, the harvest's per-candidate map, narrowed from
  `map[string]origin` to `map[string]uint64`, and every crawl B/op moved again**: each
  `internal/crawl` figure below is a 2026-08-19 re-reading, of the shipped tree and of a
  fifth export — `git archive 8c8ec21` carrying the new benchmark family as its only change —
  both `go test -run XXX -bench BenchmarkHarvestPages -benchmem -count=5 ./internal/crawl/`. The
  `1992 B` this entry quoted for `guarded` and `segmented` until then, together with the `/blind`
  B/op increase it recorded, are RETRACTED. `/blind` read 27160 B — reproduced exactly on
  2026-08-19, so that READING stands — but **the claim this entry hung on it, that 27160 is "HEAD's
  own figure", is WITHDRAWN: the two 27160s were HEAD's faithful twin against this tree's DRIFTED
  one, not one figure measured twice**. Corrected, `/blind` reads 25560 B / 244 allocs against
  HEAD's 27160 / 249 (both 2026-08-19); it is apparatus rather than an arm of the shipped harvest,
  and the bullet below is where all of it belongs. No arm in the package sits above HEAD, on either
  half of agreement item 14.
  `BenchmarkWriteSources` is untouched by that narrowing and stays a 2026-08-18 reading.
  - **`BenchmarkWriteSources` 86016 -> 73728 B/op, 3 allocs/op either side — and the formula is
    a THIRD of it, not the whole.** Holding the fixture at its new shape and reverting only
    `sourceLabelBytes` to `2*len(s.Name) + labelSyntaxBytes` reads 77824 B/op, so 4096 B is
    `internal/metrics/metrics.go`'s estimate (name + feed, where feed used to be a slice of the
    name) and 8192 B is the benchmark fixture's own shorter names, which lost the `tg-` prefix
    in the same patch: `tg-channel-456-d0c348` (21 B) became `channel42-3456` (14 B). The whole
    12288 B is arena rounding, exactly: the request falls 36007 -> 30513 -> 27949 B and lands in
    40960 -> 32768 -> 28672, the other two allocations (`ends`, the exposition buffer) unmoved
    at 4096 and 40960. **Do not record this drop against the formula.**
    Guarded since 2026-08-19 by `internal/metrics/alloc_test.go`, because until then nothing
    was: `sourceLabelBytes` only sizes the arena and changes no exposition byte, so the golden
    fixture cannot see it drift, and `make bench` tees its output without comparing it. Reducing
    the estimate to `len(s.Name)` alone leaves `go test ./internal/metrics` green while the
    reading goes 141824 B / 8 allocs, 65% above the HEAD this row is published as beating;
    dropping only the `len(sourceFeed(s))` term reads 102400 B / 4 allocs, 19% above it (both
    `-count=5`, 2026-08-19, five identical samples). Either blocks under item 14, and the guard
    fails on both.
  - **`BenchmarkHarvestPages` gained a `segmented` arm because no fixture carried a
    `data-post`.** `benchPages`, `benchNoisePages` and `benchInlinePages` hold none, so
    `nextMessage` returns on its first `strings.Index` miss and `harvestPage`'s message loop
    runs once per page: every other figure here prices the UNSEGMENTED path, which is the path
    the wave did not add. `benchSegmentedPages` is `benchPages` rearranged — same
    `benchPageBytes`, `benchPageCount` and `benchLinkRepeats`, split into 20 messages of one
    link each. **Every column below is that one fixture, and the HEAD column had to be ported
    to get it**: HEAD has no `segmented` arm, so the fixture was carried into a HEAD export
    (`discover.go` there diff-verified byte-identical to `8c8ec21`) and run — 16596 ns / 3448 B /
    21 allocs, `-count=5` medians over samples 16593-16629, 2026-08-19, against the clone tree's
    22175 ns / 2080 B / 22 allocs and the shipped tree's ~22500 ns / 1848 B / 16 allocs.
    **The `16700 ns / 3424 B / 20 allocs` this bullet recorded for HEAD `segmented` until
    2026-08-19 is RETRACTED, and the splice it warned about was its own**: 3424 B / 20 allocs is
    what `BenchmarkHarvestPagesByDistinct`'s 6-distinct point reads on HEAD, one HARNESS
    allocation below any table-driven arm (last bullet here), so that row crossed harnesses. The
    3448 B / 21 allocs a review body reported for HEAD `segmented`, dismissed here as a copy of
    `guarded`'s row, is what the export actually reads. HEAD `guarded` reads 15002 ns / 3448 B /
    21 allocs on it (14990 the day it was first taken), so **`benchPages` and
    `benchSegmentedPages` read IDENTICALLY on HEAD, as they do on the shipped tree**: the 24 B
    and one allocation once called a fixture difference are neither.
  - **Segmentation costs no allocation at all, and the arm equalling `/guarded` to the byte is
    what says so.** Both read 1848 B / 16 allocs on the shipped tree, and both read 3448 / 21 on
    HEAD, where nothing segments: the two fixtures are equal-cost by construction, so the
    20-message walk the shipped tree adds costs no allocation. Agreement item 14 makes an
    `allocs/op` increase blocking, and the clone tree's 22 was one: carrying the message id as a
    sub-slice of the unescape scratch owed one copy per message that KEEPS a link, six on this
    fixture. Retyping `origin.Post` to `uint64` removed the carrier rather than the copy —
    `unsafe.Sizeof(origin{})` 32 -> 24, so nothing is left to alias — leaving this fixture's own
    -5 (21 -> 16, the same -5 `guarded` takes) with no clone added back, and no increase to
    record. The clone tree's 22 is that 16 plus one copy per message that keeps a link, six here;
    HEAD's 21 is that 16 plus the five per-page URL slices the wave folded away. A page whose
    messages mostly keep nothing never paid the clone; a page where every message keeps a link
    paid one per message.
  - **The review's segmented figures do not reproduce and the difference is the fixture.** The
    performance pass measured 2096 B / 17 allocs and attributed one extra allocation to
    segmentation; the same arm reads 2080 B / 22 with the string id and its clone, 1992 B / 16
    without either on the then-settled tree, and 1848 B / 16 once `cand` narrowed further to
    `map[string]uint64` — two changes apart, not one — so the boundary walk allocates nothing and
    the review's `+1` belonged to its own throwaway fixture. Both numbers are honest readings of
    different fixtures, which is why the arm is committed instead of quoted.
  - **The wave's real cost is ns/op, on every arm, and it is the third full pass over each
    page** (`strings.Count` for the slice, `strings.Index` for the boundary, then `appendURLs`),
    plus `postID`'s `ParseUint` over every boundary that pass finds: `guarded` 14990 -> 17104 ->
    17159 on `benchPages` and `segmented` 16700 -> 22175 -> ~22500 on `benchSegmentedPages`,
    across HEAD, the clone tree and the pre-narrowing settled tree, all in one 2026-08-18 run. A
    2026-08-19 reading of the two ENDPOINTS alone — one link of the HEAD export, one of the
    shipped tree — puts the whole cutover at 15002 -> 17105 (+14.0%) and 16596 -> 22439 (+35.2%);
    the 16700 in the chain above is that older run's ported HEAD reading of the same arm. The
    retype's own share is the chain's middle-to-right step and is +0.3% and +1.2%, both read
    within the single run that measured both of those trees — `strconv.ParseUint` over 120
    boundaries an iteration where the clone tree handed back a sub-slice and parsed nothing. The
    percentages are NOT recomputed across runs, and narrowing `cand` moved neither arm out of its
    band: the shipped tree reads `guarded` 17105 and `segmented` 22439 (2026-08-19, five samples
    spanning 17084-17175 and 22406-22539) against 17079 and 22452 on an independent same-day run,
    so the settled `segmented` median stays quoted as ~22500 over a 22406-22590 spread and
    `guarded`'s as ~17100. `noise` read 20254 and `inline` 17251 on the measuring tree, but neither
    was ever taken on the clone tree, so no retype share exists for them; their allocations were
    taken on every tree. Allocations went the other way on every shipped arm — `guarded` and
    `segmented` alike 3448 B / 21 allocs -> 1848 / 16, `noise` 5992 / 60 -> 3528 / 37, `inline`
    unmoved at 60024 / 11 — and that -1600 B is the same flat saving
    `BenchmarkHarvestPagesByDistinct` reads at all six distinct counts below. **`/blind` sat in
    that list until 2026-08-19 and never belonged in it**: it runs a test-owned twin rather than
    the shipped harvest, so it is not a shipped arm and its figure is not a reading of one. It has
    the bullet immediately below to itself.
    The naming cutover has since moved `noise`, and only `noise`, because it is the one arm above
    that executes code this wave changed: it reads 3432 B / 37 allocs with five identical samples
    and a 19938 ns median over a 19903-19977 spread (2026-08-19, `go test -run '^$' -bench
    'BenchmarkHarvestPages$' -benchmem -count=5`), the reject line now logging an 8-hex `urlid`
    instead of minting a name it no longer has. The 3528 above is the earlier wave's endpoint
    rather than a current reading, and `inline`'s 17251 stands: its path is untouched, so the
    difference is the run-to-run variance this file's own rule refuses to recompute.
  - **`/blind` is a TWIN's figure; the HEAD equality it used to report was two different
    comparisons landing on one number, and correcting the twin moved it 27160 -> 25560 B/op and
    249 -> 244 allocs/op.** The arm runs `harvestPagesBlind`
    (`internal/crawl/reject_bench_test.go:276`), a test-owned mirror of `harvestPages` over
    `harvestPage` with the dedupe dropped, so every byte it reports is a property of that mirror
    and of nothing else. **On HEAD that mirror was FAITHFUL**: `8c8ec21`'s own shipped
    `harvestPages` scanned each page through `extractURLs` (`discover.go:278` there), exactly as the
    twin did, and that helper mints one `make([]string, 0, strings.Count(page, urlScheme))` per call
    on both trees — an inline loop at `extract.go:34-35` on HEAD, which had no `appendURLs` at all,
    and the `appendURLs` wrapper at `extract.go:35-36` here. There was no hoist on that tree to
    mirror, so nothing was wrong with the twin as written. **The drift is this wave's**: it hoisted
    one `urls` slice into `harvestPages` (`discover.go:375`, grown only when a page needs more at
    `:404`) and left the twin scanning per page, so from that commit until
    2026-08-19 the twin mirrored a body that had moved under it. **That is why `27160` read "either
    side", and why calling it "HEAD's own figure" was wrong**: it was HEAD's faithful twin against
    this tree's DRIFTED twin, landing on the same number because the twin's drift was exactly the
    size of what the wave had saved. An arm that does not execute the changed code cannot report
    that the change did nothing.
    Corrected, the twin reads **25560 B / 244 allocs** on the shipped tree against **27160 / 249**
    on an `8c8ec21` export — HEAD needs no backport, it carries this family and its own
    `harvestPagesBlind` — both `-count=5` with all five samples identical, measured by
    `agent://ReworkGuards` 2026-08-19 with `go test -count=5 -run '^$' -bench BenchmarkHarvestPages
    -benchmem ./internal/crawl/`, the pre-fix figure taken on an export verified byte-identical to
    the worktree. **Read the two deltas apart, because only one of them is a code win:**
    - **WITHIN this tree, 27160 -> 25560 is the TWIN being corrected**, and no shipped line moved
      for it — `guarded` and `segmented` 1848 / 16, `noise` 3528 / 37, `inline` 60024 / 11 and all
      six curve points re-read unmoved in the same run (that run's readings; the naming cutover
      later took `noise` to 3432 / 37). Book no code credit for that drop: the TEST
      got honest.
    - **ACROSS trees, 25560 against HEAD's 27160 IS the wave's hoist** — the same -1600 B / -5
      allocs every other arm shows, the twin's five redundant 320 B header blocks, the 320 B x 5 the
      FLAT-curve bullet below describes. So the old equality was HIDING a real win rather than
      inventing one, and it stayed hidden for as long as the apparatus kept paying the exact cost
      the wave had removed from the harvest. `/blind` sits 1600 B and 5 allocs BELOW HEAD, so
      agreement item 14 is satisfied on it in the passing direction.
    The pair's meaning moves with the figure: `/guarded` against `/blind` priced the dedupe PLUS
    five slice mints only the drifted twin ever paid, and now prices **the dedupe alone** —
    `candidate` and one `strings.Clone` per OCCURRENCE against per DISTINCT URL. **The lesson
    generalises: a twin is faithful only to the tree it was written against** — this one was correct
    the day it landed and was falsified by a change to the body it mirrors, in a file that change
    never opened, and no assertion in the package caught it.
  - **The axis the committed arms cannot express: the distinct-candidate curve.**
    `BenchmarkHarvestPagesByDistinct` (`internal/crawl/reject_bench_test.go:539`) holds page
    bytes, page count, occurrences per page and URL length, and varies only the repost factor, so
    the single moving part is how many DISTINCT candidates the pages yield — and with it the size
    of `cand`. At 6 / 12 / 24 / 30 / 60 / 120 distinct accepted candidates the shipped tree reads
    **1824 / 3528 / 6952 / 10024 / 19752 / 38792 B/op** and 15 / 30 / 56 / 70 / 132 / 254
    allocs/op, against HEAD `8c8ec21`'s 3424 / 5128 / 8552 / 11624 / 21352 / 40392 and
    20 / 35 / 61 / 75 / 137 / 259 — both columns `-count=5` medians, 2026-08-19, this machine,
    HEAD being that same export with this family backported as its only change. **-1600 B and
    -5 allocs/op at every single point**, so the harvest is under HEAD on both halves of
    agreement item 14 at every distinct count measured. Both columns reproduce
    `agent://PostAsNumber`'s to the byte; that is a second RUN, not a second link, which is all
    an allocation figure needs and is not offered as an ns control.
  - **The mechanism is why that curve is FLAT, and it is the transferable part.** The saving is
    one `[]string` per PAGE, not per candidate: at `8c8ec21` `extractURLs` minted
    `make([]string, 0, strings.Count(page, urlScheme))` for every page (`extract.go:35` there),
    where `harvestPages` now hoists one `urls` slice across the whole channel and `harvestPage`
    grows it only when a page needs more (`discover.go:375` and `:404`). This fixture holds 20
    occurrences per page at every point on the curve, so that slice is a 20-element header block —
    320 B — and five of six pages stop allocating one: -5 allocations and -1600 B, whatever the
    distinct count. Anything stored per KEY moves the other way and SCALES with it: a 24 B
    `map[string]origin` value read -1456 B against HEAD at 6 distinct, crossed ABOVE HEAD between
    24 and 30, and reached +6512 B (+16.1%) at 120 — on a tree whose `guarded` and `segmented`
    arms both read 1992 B against HEAD's 3448 and so showed nothing at all
    (`agent://PostAsNumber`, 2026-08-19). **A fixture that pins the key count cannot see a per-key
    cost**, and every arm of `BenchmarkHarvestPages` is repost factor 20 — six distinct keys over
    six pages, the most favourable shape such a cost can meet. `/guarded` and `/segmented` price
    the flat win and MUST NOT be read as covering this axis; an item-14 reading on a per-candidate
    value belongs on `BenchmarkHarvestPagesByDistinct`.
  - **Do not splice the curve's 6-distinct point onto `/guarded`'s row: 24 B of the gap is the
    benchmark's own.** The two run byte-identical pages — the family asserts that at repost factor
    `benchLinkRepeats` or fails — and still read 1824 B / 15 allocs against 1848 / 16.
    `BenchmarkHarvestPages` drives its arms through a `harvest` func field, so its per-iteration
    `var inline []string` escapes and every arm pays a heap slice header, while the curve's direct
    call keeps that variable on the stack: `go test -gcflags=-m` prints
    `internal/crawl/reject_bench_test.go:518:9: moved to heap: inline` for the table loop and
    nothing for `BenchmarkHarvestPagesByDistinct`'s. The same run prints two other heap moves in
    this family, both built before `b.Run` and so outside every figure: the per-arm delivery
    guard's `internal/crawl/reject_bench_test.go:446:3: moved to heap: probe`, which now lives in
    `benchHarvestArm.check` (`:443-466`) rather than in the benchmark's own body, and
    `internal/crawl/reject_bench_test.go:216:7: moved to heap: sb`, which is `benchInlinePages`'
    builder escaping into `fmt.Fprintf` as it writes the inline arm's fixture. A fourth move,
    `:339:6: moved to heap: parseErr` inside `benchCandidateCase.check`, belongs to the candidate
    benchmarks rather than to this family. (All four anchors are 2026-08-19 `-m` output, re-run on
    the reworked file; the `:463:9`, `:454:7` and `:502` this bullet cited until then named
    constructs that have since moved between functions. Compiler output is quoted, never
    renumbered by hand.) One 24 B
    allocation, in the harness and not in the harvest, and it is there in BOTH trees: HEAD reads
    3448 B / 21 allocs on its table arms against 3424 / 20 at 6 distinct, the shipped tree 1848 / 16
    against 1824 / 15. The -1600 B and -5 allocs are the same figure whichever harness you read them
    in, which is the only reason the two rows can be compared at all — and only after subtracting
    that header, never by splicing the rows.
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
    **The timed body is the harness's own reassembly of `Probe`'s steps, not a call to `Probe`**:
    it runs `convert.ConvertsV2Ray`, `probeNodes` and `parseLive` in sequence with `benchSpareEvery`
    standing in for the pre-check (`internal/stable/hotpath_bench_test.go:240`), so the figure
    prices exactly those three, and a step added to `Probe` anywhere else would not appear in it.
    Nothing asserts that the sequence still matches `Probe`'s body — the same twin axis as `/blind`
    above, UNCLOSED here, and the scaled `~739000 allocations and ~81 MB` inherits it.
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
