# Acquired 2D interpolation preparation cost


## Profile and method


First, three 500 ms benchmark windows measured the current complete preparation
(both axis arrays, span, background row and closure) separately from the current
full raster. Median preparation was 41.011 us at GOMAXPROCS=1 and 38.930 us at
GOMAXPROCS=8. Median full raster was 16.538 ms and 6.687 ms respectively.
Preparation therefore represented about 0.25% / 0.58% of those separate timing
observations. It allocated 218,592 B and four objects versus 23,087,128 B / ten
objects for the single-worker complete raster. These ratios characterize this
profile, not a universal upper bound on all output geometries.

A disposable Go overlay then retained all preparation, including the background
row, for this one fixed request. It changed no sampling or rounding arithmetic,
still allocated fresh outputs and still used exclusive per-operation/per-worker
scratch. This deliberately optimistic prototype omitted key lookup, locking,
budget admission and invalidation, and retained roughly 218 KB. It was never
integrated into production and is unsafe as a general cache. Its only purpose
was to test whether eliminating preparation could demonstrate a useful gain
before introducing those costs and ownership contracts.

Each pair ran the current binary followed by that prototype for 100 frames.
Executables were compiled before timing; three pairs ran per CPU setting.
P95 is the nearest-rank quantile of the 100 measured frame durations. Process
CPU includes fixture preparation and Go benchmark calibration; ns/frame and
allocation metrics exclude fixture construction. No child processes were
launched by the measured binaries. Process peak working set was unavailable
from the post-exit collector and is explicitly unmeasured; allocation counts
do not establish reduced peak memory.

| Workers | Pair | Current ms/frame | Prototype ms/frame | Current P95 ms | Prototype P95 ms | Current process CPU ms | Prototype process CPU ms |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 1 | 15.062 | 14.744 | 17.326 | 16.394 | 1562.500 | 1546.875 |
| 1 | 2 | 15.898 | 16.149 | 21.009 | 22.369 | 1640.625 | 1656.250 |
| 1 | 3 | 16.362 | 16.521 | 23.400 | 23.986 | 1687.500 | 1687.500 |
| 8 | 1 | 4.247 | 4.109 | 6.611 | 6.301 | 2156.250 | 2343.750 |
| 8 | 2 | 4.723 | 4.328 | 7.029 | 6.887 | 1953.125 | 1937.500 |
| 8 | 3 | 4.394 | 4.439 | 5.531 | 6.227 | 2250.000 | 1812.500 |

Single-worker allocation fell from 23,087,128 to 22,868,760 B/frame (0.95%),
ten to seven objects. Eight-worker allocation was approximately 23.118 to
22.899 MB/frame, 29 to 26 objects; goroutine scheduling introduces small
variations. The closure and worker scratch remain per call.

Two single-worker pairs regressed; the last eight-worker pair regressed by
about 1% in mean latency and 12.6% in P95. The optimistic cache therefore did
not demonstrate a consistent timing improvement. Retaining plans and adding a
new bounded owner, budget charging and exact geometry invalidation is not
justified by these observations. A materially different workload may warrant
repeating the experiment. Pixel-kernel and output-copy costs remain independent
opportunities; this result does not qualify a change to either.

## Repeating the production measurements

From the library module:

```sh
go test -p 2 ./render -run '^$' -bench '^BenchmarkRasterViewport2D(Preparation4K|Clinical4K)$' -benchtime=500ms -count=3 -cpu=1,8
go test -p 2 ./render -run '^$' -bench '^BenchmarkRasterViewport2DRepeatedGeometry4K$' -benchtime=100x -count=3 -cpu=1,8
```

The checked-in repeated task reports both mean and P95. The preparation probe
does not execute its returned row kernel and reuses an unmodified output only
to avoid charging publication allocation to preparation. It never publishes or
mutates a UI-owned image. No benchmark result is a fixed performance gate.

The local experiment retained its raw JSON, per-run stdout/stderr, fixed-request
overlay source, overlay generator, collector and binaries in the ignored
`C:/Users/user/Documents/go-dev-worktrees/rav64-perf-1578/tmp/rav64-perf-1578/`
directory. `measurements.json` contains all twelve measured runs. The separate
preparation measurements are in `planning-baseline-cpu1.log`; the eight-worker
preparation figures above were captured in the tool output. None of those
machine-local artifacts is a shipped runtime dependency. The disposable probe
was built/run with the following commands from the library module:

```sh
go test -p 2 -c -o ../tmp/rav64-perf-1578/raster-before.test.exe ./render
python ../tmp/rav64-perf-1578/create-probe.py
go test -p 2 -overlay ../tmp/rav64-perf-1578/probe-overlay.json -c -o ../tmp/rav64-perf-1578/raster-ideal-preparation.test.exe ./render
```

The PowerShell `measure.ps1` collector launched those two executables in that
order for each pair, with `-test.run=^$`,
`-test.bench=^BenchmarkRasterViewport2DRepeatedGeometry4K$`,
`-test.benchtime=100x`, `-test.count=1` and `-test.cpu=1` or `8`. Process wall
times including launch/calibration were, in pair order, 1949.6/1768.6,
1685.1/1720.9 and 1750.3/1742.3 ms for one worker; 505.2/492.4, 572.4/517.5
and 517.0/521.9 ms for eight workers. Scheduling/launch costs also explain why
these values differ from the summed benchmark frame durations.

## RAV64 reference and limits

The retained static decompilation of `RadiAntViewer.exe` at `0x14015cf80` and
`0x14015d120` separates enlargement/reduction preparation from consumption in
object-owned buffers; `0x140160000` prepares the resampling contract. That
establishes a preparation/consumption pattern, not demonstrated cross-frame
caching or measured superiority. SHA-256:
`fb4ae2f1c72dc5ba393bb7139107d0dc1e741a4ed10224b0d8c56a4cc3d0dbb3`.
The experiment used independent Go changes, copied no recovered source and ran
no RAV64 target or runtime comparison. No new cache correctness, macOS runtime
or native GPU performance claim follows from this assessment.
