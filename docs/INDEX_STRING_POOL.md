# Index string sharing: rejected experiment


The pool exists only in `index/string_pool_experiment_test.go`. Normal library imports do not expose its identifiers; no global catalogue, permanent consumer service, normalization or default scanner change was added. Pool logic was relocated byte-for-byte from the private G1 prototype. Functional tests, race case, benchmark data and worker oracles were unchanged. G1 measurements remain historical; all tables below are actual G2 test-only results. G3 changes only this guide.

## Scope and budgets

The test-only pool accepts byte-identical builtin FileMeta, Patient, Study, Series and Instance strings. It clones new admissions so a short substring does not retain a larger backing allocation. It caps 16,384 entries and 4 MiB unique UTF-8 payload, bypassing full pools and giant values without changing content or failing indexing. Payload limits exclude map/runtime overhead and are not RSS limits. Patient is copied; Source/Geometry/Selected/Missing/Diagnostics are unchanged, with their documented borrowed references. No Record/source/callback is retained by the pool.

Close drops pool ownership without invalidating strings held by outputs. Methods serialize concurrent calls; cancellation returns a zero Record while already admitted bounded entries may remain until Close. A single hash/copy or mutex acquisition cannot be interrupted. Giant bypassed values may retain their original backing allocation through the consumer independently of the pool.

## Fixed recipe and actual qualification

Darwin arm64, macOS 27.0.1, Go1.27.1; GOMAXPROCS=2, GOMEMLIMIT=1GiB, p=1/count=1 for functional/race; normal build cache, no optional codec or GPU claim. Actual focal4 and race1 passed without skips/failures. One source-bound binary produced 30 benchmark rows and 30 fresh retention workers, all process groups reaped/drained to EOF with exact before/finally source guards. Benchmark cases have five repetitions each. Retention uses five separate processes per cell.

Low cardinality retains 4 studies/32 series; high cardinality has unique study/series/patient metadata. Both retain unique instance UIDs and fixed inherent codes. Stream uses low cardinality and discards outputs. Each generated string has fresh backing allocation. Actual scanner `TransferSyntaxName`, however, is assigned from `syntax.Name`/`header.TransferSyntax.Name` in index/reader.go and can already share static metadata. Other real parser values and application lifetimes were not measured. This is consumer construction/retention evidence, not parser/IO/scanner throughput or Twin archive proof.

Every number is median [min–max] across all five observations. Fixed-order repeats are descriptive, not a randomized statistical campaign. No selective omission or measurement retuning occurred. One benchmark operation is a 4,096-record batch and includes synthetic construction, retention/checksum, pool hashing/insertion, Patient copies and Close. Exact Record equality is checked outside timing. Retention workers build 32,768 records.

## Benchmark, 30 actual rows

| Cell | ns/batch | B/batch | allocs/batch |
|---|---:|---:|---:|
| low/baseline | 3909292 [3055375–4554541] | 6287920 [6287904–6288048] | 151301 [151301–151302] |
| low/pool | 4843875 [4699875–5200459] | 10219056 [10219040–10219376] | 167826 [167826–167830] |
| high/baseline | 2879333 [2556917–3028334] | 6538392 [6538376–6538504] | 174339 [174339–174340] |
| high/pool | 5721375 [5546083–5810792] | 12678592 [12678576–12678816] | 203162 [203162–203165] |
| stream/baseline | 2386792 [2343417–2446792] | 4682024 [4682008–4682304] | 151298 [151298–151301] |
| stream/pool | 4347292 [4288625–4483833] | 8613632 [8613424–8614120] | 167827 [167825–167831] |

## Retention construction, 30 actual workers

| Cell | construction ns | cumulative allocated B | mallocs |
|---|---:|---:|---:|
| low/baseline | 21038458 [20654459–21977083] | 50328952 [50328792–50329016] | 1212173 [1212171–1212175] |
| low/pool | 35440917 [34928833–37111209] | 78638344 [78638328–78638504] | 1327008 [1327008–1327010] |
| high/baseline | 23622208 [23553917–23921708] | 52779224 [52778840–52784176] | 1407247 [1407243–1407249] |
| high/pool | 37498667 [36903291–37942791] | 80939128 [80939064–80939208] | 1522082 [1522082–1522084] |
| stream/baseline | 20579667 [20331500–20972292] | 37486968 [37486776–37487592] | 1212196 [1212194–1212204] |
| stream/pool | 32372292 [32214333–33357416] | 65798872 [65798488–65799224] | 1327053 [1327048–1327057] |

## Signed retained Go heap after GC

| Cell | pool/outputs active B | after Close, outputs held B | after outputs dropped B |
|---|---:|---:|---:|
| low/baseline | 45346648 [45346648–45346664] | 45346680 [45346456–45346856] | -832 [-848–-832] |
| low/pool | 38540968 [38540760–38541144] | 37228824 [37228616–37228840] | -848 [-864–-848] |
| high/baseline | 45792632 [45792616–45797744] | 45792648 [45792424–45797760] | -832 [-848–4472] |
| high/pool | 43361544 [43361448–43361544] | 42049416 [42049416–42049592] | -864 [-864–-752] |
| stream/baseline | -328 [-344–-232] | -832 [-832–-720] | -832 [-832–-720] |
| stream/pool | 1834872 [1834856–1835176] | -832 [-848–-720] | -832 [-848–-720] |

These are signed HeapAlloc deltas, not physical recovery or RSS. Small negative values and the high/baseline +4,472 B maximum after release are runtime/cache noise. Baseline has no pool; its Close column repeats GC while outputs remain. After Close the retained profiles still hold output strings. Streaming holds no outputs; its active pool adds about 1.835 MB versus a near-zero baseline and returns to noise after Close.

## Whole-process CPU and peak RSS

| Cell | real s | user CPU s | system CPU s | Darwin peak RSS B |
|---|---:|---:|---:|---:|
| low/baseline | 0.09 [0.08–0.09] | 0.1 [0.1–0.1] | 0 [0–0] | 105824256 [105529344–106397696] |
| low/pool | 0.1 [0.1–0.1] | 0.12 [0.12–0.12] | 0 [0–0] | 97419264 [97353728–97550336] |
| high/baseline | 0.09 [0.09–0.09] | 0.11 [0.1–0.11] | 0 [0–0] | 107200512 [106938368–107298816] |
| high/pool | 0.1 [0.1–0.1] | 0.12 [0.12–0.13] | 0 [0–0] | 101679104 [101482496–101761024] |
| stream/baseline | 0.02 [0.02–0.02] | 0.02 [0.02–0.02] | 0 [0–0] | 20021248 [19660800–20168704] |
| stream/pool | 0.03 [0.03–0.04] | 0.04 [0.04–0.04] | 0 [0–0] | 22560768 [22265856–22822912] |

Whole-process CPU/RSS include setup, GC and the post-Close full-Record equality oracle. ConstructionNS excludes those later operations. time-l CPU values are coarsely rounded; zero system seconds do not mean no kernel work. Cumulative allocated bytes/Mallocs and B/op are separate metrics. RSS was lower in retained pool cells and higher in streaming; that does not establish an acceptable net application tradeoff.

## All checksums and pool budgets

| Cell | checksum, all five | entries | payload B | hits | bypasses |
|---|---:|---:|---:|---:|---:|
| low/baseline | 2446490 | 0 [0–0] | 0 [0–0] | 0 [0–0] | 0 [0–0] |
| low/pool | 2446490 | 16384 [16384–16384] | 511689 [511689–511689] | 671564 [671564–671564] | 32948 [32948–32948] |
| high/baseline | 2686414 | 0 [0–0] | 0 [0–0] | 0 [0–0] | 0 [0–0] |
| high/pool | 2686414 | 16384 [16384–16384] | 333818 [333818–333818] | 461082 [461082–461082] | 243430 [243430–243430] |
| stream/baseline | 2446490 | 0 [0–0] | 0 [0–0] | 0 [0–0] | 0 [0–0] |
| stream/pool | 2446490 | 16384 [16384–16384] | 511689 [511689–511689] | 671564 [671564–671564] | 32948 [32948–32948] |

Checksum is an aggregate length checksum, not a cryptographic content oracle. Full Record/Unicode equality is independently checked in focal tests and benchmark preflight, and retained worker Records are checked after Close. Every baseline/pool checksum pair matches. Entry/payload budgets held in all workers; insertion counts equal entry counts. Counters and logs contain synthetic aggregates only, with no patient values/paths.

## Why the experiment was rejected

| Profile | benchmark time change | B/op change | allocs/op change | active retained heap change | construction time change |
|---|---:|---:|---:|---:|---:|
| low | +23.91% | +62.52% | +10.92% | -15.01% | +68.46% |
| high | +98.70% | +93.91% | +16.53% | -5.31% | +58.74% |
| stream | +82.14% | +83.97% | +10.92% | baseline near zero; +1,834,872 B | +57.30% |

Low-cardinality held heap falls from 45,346,648 to 38,540,968 B while the pool is active, and to 37,228,824 B after pool Close, but its time and allocation costs rise. High-cardinality savings are smaller. Streaming has no retention benefit and incurs all additional pool costs. The fresh-backing synthetic assumption may overstate savings versus real already-shared metadata. There is no demonstrated representative net benefit sufficient to ship the API, so the experiment remains test-only and rejected for production.

## Reproduce the fixed experiment

From dicom-go-dev, set GOMAXPROCS=2, GOMEMLIMIT=1GiB, GOTOOLCHAIN=local and GOWORK=off. Use a distinct binary/output path; coordinate available disk (40 GiB minimum) and other builds. The original recipe uses fixed counts, budgets and cells:

```sh
go test -json -count=1 -p=1 -timeout=60s ./index -run '^TestStringPool(ContentUnicodeAndOwnership|BudgetsFullAndLargeBacking|CancellationAndCloseLifetime|ConcurrentBatchesAndClose)$'
go test -race -json -count=1 -p=1 -timeout=60s ./index -run '^TestStringPoolConcurrentBatchesAndClose$'
go test -c -o /tmp/index-pool-experiment.test ./index
/tmp/index-pool-experiment.test -test.run='^$' -test.bench='^BenchmarkIndexStringPool$' -test.benchtime=1x -test.count=5 -test.benchmem -test.timeout=120s
env DICOM_INDEX_POOL_RETENTION_CELL=low/baseline /usr/bin/time -l /tmp/index-pool-experiment.test -test.run='^TestIndexStringPoolRetentionWorker$' -test.v -test.count=1 -test.timeout=60s
```

Run that retention command in a fresh process five times for each exact cell: low/baseline, low/pool, high/baseline, high/pool, stream/baseline, stream/pool. Preserve all raw outputs. The worker is explicitly opt-in and skips when its environment variable is unset; ordinary module checks do not repeat this memory campaign. Interpret time-l peak RSS in Darwin bytes; the command is not a Windows qualification.

## Original six criteria and evidence limits

1. Baseline/pool low/high/stream recipe, parameters and environment are frozen and all six cells/five repeats reported.
2. Retained heap after GC, allocations, CPU/elapsed and RSS are separated with all ranges and limitations; no universal gain is claimed.
3. Production integration was rejected on measured costs; the test-only experiment preserves the documented negative conclusion.
4. Independent entry/byte budgets, giant backing strings, unique/full pools and unchanged content passed focal checks and all worker caps.
5. Actual race1 and lifecycle/cancel/concurrent Close focals passed; consumer-held strings remain valid and outputs/pool are released separately.
6. Full Record fields/Unicode equality and synthetic non-PHI logs are verified; worker checksums corroborate matched workloads.

These actual targeted tests/measurements are not a full module baseline or full-module race claim. The integrated library make check and docs-check are recorded separately in delivery evidence. Updating this guide does not change the three test files or require remeasurement merely to make an experiment green.

## Actual G2 provenance

- Original recipe SHA256: 58f9abeb55657fde54755d6fd333650ee80f66e02639abb94ac3ae52a127dfc3
- G2 owned manifest SHA256: cc5496aea8185cb9b4a08eb535c00be05794a6da02b5e53a4cb422a6535057f8
- Focal4 RESULT SHA256: ba9007b7dfb93ab0e09504a03b79219bef896e1ca313d069620b011fd3a02fcc
- race RESULT SHA256: 68e4ef1943f7b0297153ae54c1e5d277976788fc2cb7d1954360896377ec77d7
- compile RESULT SHA256: 499fca1e2384104cf6d82682fef5328956efe1b0623ae001e0f4f393444c8b41
- benchmark RESULT SHA256: 8e608294dba92f67be373778e61ad9b77a594d0a167bae009b20df17d88647cb
- retention RESULT SHA256: e11e97613e22929ca35044901b39d14a78f2885c0843c5d5b26ab28d915a35e4
- Compiled binary SHA256: 46d30b52d1f4668f30211fba6f7b956220986bd9ef7daf109d7c73b2c8ca9b2c

G1 actual receipts and its negative conclusion remain immutable historical evidence; G2 reran the fixed experiment because compilation filenames changed, not to select a better outcome. G3 is guide-only with all three Go postimages unchanged.
