# Ordered Object storage evaluation


## Candidates and correctness

- **S:** the published capacity/constant-VR implementation. Its five Go files were verified byte-for-byte against the published commit. The common isolated module came from `d5b3e3796a390fc4b6f02afac6d77e28d94ac679`; unrelated JSON, filters, pixel-data, rendering and application changes were excluded from all snapshots.
- **A:** elements in a sorted slice, binary lookup and append for new tags strictly above the maximum. The existing insertion-order representation remains separate to preserve last-wins/last-position semantics. Removal clears the old element tail; sequence facades detach their element and order backings before mutation.
- **B:** A with a `map[Tag]int` lookup index. Sorted insert/removal reconciles positions; copy-on-write includes the index. This is a hybrid, rather than the literal proposal to remove all maps/order structures.

Both prototypes passed the existing Object suite, deterministic public-API traces for lookup/order/update/removal, snapshot independence, tail-clear and facade detach/miss probes. Object race tests, `go vet ./object`, and Go 1.22.12 tests with external linking passed; `dicomjson`, `dicomxml` and `validation` tests passed. One existing test deliberately wrote directly to the private storage to observe charset-cache behavior; only that representation-specific access was adapted, preserving its assertions. No public API changed. No prototype was integrated.

## Lookup campaign

Go 1.27.1, darwin/arm64, Apple M4 Pro, `GOMAXPROCS=1`, one exclusive window. Inputs/values, fixed lookup traces and binaries were prepared before timing. Each snapshot ran 70 Get/Has cases: 8/128/1024/4096/16384 attributes × first/middle/last hot hits, uniformly permuted hits, and misses before/between/after the stored range. Tags were spaced by two to create real interior misses.

Three rounds used S/A/B, B/S/A, A/B/S, with 100 ms per case. The campaign completed in 85.1 seconds. The practical limit was a **5% lookup regression at 128+ attributes**, fixed before results. Ratios pair candidate/S within each round. The intervals below are approximate 95% Student-t intervals on the three paired ratios (df=2); short runs and only three pairs limit precision. They do not establish platform-wide equivalence.

| Operation / attributes | S ns/op median | A/S ratio [95% interval] | B/S ratio [95% interval] |
| --- | ---: | ---: | ---: |
| Get uniform / 8 | 4.632 | 1.692 [1.606, 1.745] | 0.787 [0.737, 0.843] |
| Get uniform / 128 | 14.230 | 1.142 [1.094, 1.180] | 1.027 [1.020, 1.033] |
| Get uniform / 1024 | 15.320 | 1.409 [1.211, 1.605] | 1.016 [1.006, 1.023] |
| Get uniform / 4096 | 21.290 | 1.708 [1.523, 1.830] | 1.137 [1.085, 1.173] |
| Get uniform / 16384 | 24.110 | 2.961 [2.742, 3.178] | 1.153 [1.142, 1.164] |
| Has uniform / 8 | 2.658 | 2.456 [1.897, 2.938] | 1.023 [0.832, 1.195] |
| Has uniform / 128 | 3.293 | 4.497 [3.821, 4.923] | 0.999 [0.977, 1.032] |
| Has uniform / 1024 | 3.789 | 5.044 [4.399, 5.812] | 0.984 [0.966, 1.002] |
| Has uniform / 4096 | 4.682 | 7.223 [5.234, 8.589] | 0.881 [0.853, 0.901] |
| Has uniform / 16384 | 4.965 | 14.158 [11.750, 17.102] | 0.959 [0.706, 1.364] |
| Get interior miss / 128 | 5.159 | 2.642 [2.551, 2.711] | 0.753 [0.732, 0.767] |
| Get interior miss / 1024 | 5.213 | 3.147 [2.939, 3.389] | 0.766 [0.748, 0.781] |
| Get interior miss / 4096 | 5.224 | 3.685 [3.220, 3.979] | 0.761 [0.713, 0.816] |
| Get interior miss / 16384 | 5.242 | 3.915 [3.455, 4.367] | 0.767 [0.745, 0.791] |

A regressed on uniform hits and interior misses across the mandatory sizes, despite fast boundary cases. B improved several hot Get and miss cases and kept Has close to or below S, but uniform Get regressed by about 14%/15% at 4096/16384 attributes. The lower interval bounds (1.085/1.142) already exceed 1.05. Borderline cells do not change this admission decision, so the lookup campaign was not repeated.

Construction, mutation, Elements/SortedElements, sequence/copy-on-write cost and retained-heap harnesses were prepared but **not executed** after lookup rejection. No allocation, retained-memory or complete-workload improvement is claimed for A/B. This rejects these concrete prototypes, not every possible sorted or hybrid implementation. S already met the metadata/cold-sequence allocation acceptance criterion documented in OBJECT_ALLOCATION.md.

## Local evidence and reproduction


The driver uses `-test.run=^$ -test.benchmem -test.cpu=1 -test.benchtime=100ms -test.count=1 -test.bench=^BenchmarkObjectOrdered(Get|Has)1301$` with `GOMAXPROCS=1` and the round order above. Keep the snapshots/harness fixed, preserve every sample, and use fresh output files for a new campaign. Results are local synthetic evidence; no native Windows or live-PACS qualification was performed.
