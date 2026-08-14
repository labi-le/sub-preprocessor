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
- Recent optimization work improved:
  - geofeed parsing allocations
  - fragment rewrite allocations
  - inner filter hot-path allocations
  - skipping non-URI lines during subscription parse
- Still-hot areas worth revisiting before large refactors:
  - `internal/subscription.Parse`
  - `internal/filter.ParseAllowed`
  - IPv6 support in resolver/filter path is still incomplete / not yet generalized
