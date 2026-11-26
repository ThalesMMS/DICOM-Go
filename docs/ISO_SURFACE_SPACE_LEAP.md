# Headless CPU iso-surface skipping


The prepared generation caches the existing conservative min/max grid of
8 by 8 by 8 voxel blocks. Each block includes its interpolation neighbours;
float32 bounds round outward and are widened for interpolation rounding.
A block can be skipped only when its maximum is strictly below the iso-value.
An all-solid block cannot be skipped: its first sample may be the surface.
There is no inverted threshold mode in this API. Non-finite or unknown bounds
continue through the original interpolation path.

Skipping retains repeated addition of the original march step rather than
multiplying an index by the step. Every skipped logical sample still charges
the primary ray budget and checks cancellation at the original stride.
Samples near block faces remain on the full path. The first retained solid
sample uses the same preceding step for bisection, even when it was skipped
because it was proven to be air.

Each frame acquires a sampler lease on the exact prepared generation and
releases it on every return. The scalar source and min/max grid remain owned
by the prepared volume; returned image pixels belong to the caller. Concurrent
frames share the immutable grid. Waiting for its mutex is cancellable, and a
cancelled build is not published. Source geometry and prepared identity must
remain immutable according to the existing volume contract. Closed or stale
prepared sources report their acquisition error.

The retained grid uses eight bytes per block for its min/max pair, with
`ceil(cols/8) * ceil(rows/8) * ceil(depth/8)` blocks. Building it scans the
source once with overlapping interpolation neighbours. A cold frame pays that
build; later frames reuse it. Dense fields, thresholds that hit at the ray
entry, very small volumes and non-finite blocks need not become faster.
Existing volume, sampling, output-image and geometry limits still apply; this
optimization does not introduce an RSS quota or remove source preparation.

## Qualification scope

Synthetic tests compare every image byte with the full march across float32
and signed/unsigned packed16 sources, negative rescale, perspective/parallel
cameras, fractional steps, shading, crops, cuts and opaque/translucent geometry.
They also cover block threshold rounding, non-finite bounds, logical sample
budgets, cancellation while skipping or waiting for the cache, filtered
prepared generations, concurrent reuse and source closure.

The following preparation-stage cancellation limits remain: the existing grid
builder checks context between Z-block slabs, and the filtered prepared range
scan has no context parameter. Cache waits and the ray march are cancellable,
but this qualification does not establish a maximum cancellation latency
inside those scans. Prepared geometry and source identity must remain immutable.

## Complete synthetic comparison

A reserved run on macOS 27.0.1 arm64 / Apple M4 Pro, Go 1.27.1,
GOMAXPROCS=2 compared the original surface implementation with this change.
The fixture is a 128-cubed volume containing a radius-six sphere, a 96x80
opaque frame, a camera rotated 13/-7 degrees and fixed .25 spacing fraction,
without shading or clipping. Sparse iso=128; empty iso=10000; dense iso=-1
hits at entry. The dense control retains that first-solid behavior.

Each case has five alternating baseline/candidate pairs in fresh processes.
Tables show median [minimum, maximum], using every sample. Speedups are the
median and full range of the five paired baseline/candidate time ratios.

| Case | Baseline ms/frame | Candidate ms/frame | Paired speedup |
| --- | ---: | ---: | ---: |
| WarmSparse128 | 43.202333 [42.868056, 43.376500] | 4.596667 [4.419986, 5.036111] | 9.326x [8.613, 9.765] |
| WarmEmpty128 | 43.761042 [43.472167, 47.255319] | 4.490681 [4.382333, 4.539833] | 9.875x [9.658, 10.506] |
| WarmDense128 | 0.282875 [0.262042, 0.292930] | 0.299570 [0.296556, 0.302417] | 0.954x [0.876, 0.969] |
| PreparedWarmSparse128 | 43.265417 [43.088958, 43.727333] | 4.556042 [4.436722, 4.598250] | 9.496x [9.400, 9.821] |
| ColdSparse128 | 51.765055 [51.529500, 52.011597] | 15.741903 [15.361333, 15.965514] | 3.291x [3.243, 3.370] |

The several-fold reduction is supported for these sparse, empty, explicitly
prepared and cold sparse scenes. WarmDense regresses: its paired median is
4.836% slower, with a 3.239–14.120% slowdown range. No case or outlier was
removed, and this is not a universal speedup or a Twin GPU result.

| Case | Baseline B/frame | Candidate B/frame | Baseline allocations/frame | Candidate allocations/frame |
| --- | ---: | ---: | ---: | ---: |
| WarmSparse128 | 34085 [34085, 34085] | 34306 [34306, 34306] | 15 [15, 15] | 19 [19, 19] |
| WarmEmpty128 | 34085 [34085, 34240] | 34306 [34306, 34306] | 15 [15, 15] | 19 [19, 19] |
| WarmDense128 | 34085 [34085, 34085] | 34306 [34306, 34306] | 15 [15, 15] | 19 [19, 19] |
| PreparedWarmSparse128 | 34085 [34085, 34085] | 34229 [34229, 34384] | 15 [15, 15] | 16 [16, 16] |
| ColdSparse128 | 8705464 [8705464, 8705661] | 8739936 [8739936, 8740133] | 610 [610, 610] | 626 [626, 626] |

Default warm calls add 221 B/frame and four allocations at the median. The
explicitly prepared call adds 144 B/frame and one allocation. ColdSparse adds
34472 B/frame and sixteen allocations; it includes constructing the 32 KiB
block grid and supporting objects. Allocation savings are not claimed.

| Case | Baseline absolute RSS bytes | Candidate absolute RSS bytes | Baseline sampled group RSS bytes | Candidate sampled group RSS bytes |
| --- | ---: | ---: | ---: | ---: |
| WarmSparse128 | 32440320 [32309248, 32735232] | 32604160 [32440320, 32817152] | 33505280 [33406976, 33783808] | 33669120 [33505280, 33849344] |
| WarmEmpty128 | 32522240 [32374784, 32800768] | 32522240 [32407552, 40878080] | 33603584 [33456128, 33882112] | 33636352 [33505280, 41926656] |
| WarmDense128 | 32505856 [32309248, 32702464] | 32342016 [32243712, 32505856] | 33439744 [33308672, 33652736] | 33193984 [33013760, 33374208] |
| PreparedWarmSparse128 | 32456704 [32325632, 40812544] | 32604160 [32489472, 32784384] | 33538048 [33439744, 41893888] | 33669120 [33587200, 33882112] |
| ColdSparse128 | 50184192 [49807360, 50266112] | 50249728 [49987584, 50479104] | 51216384 [50855936, 51314688] | 51232768 [51068928, 51544064] |

RSS did not show an overall improvement. In particular, the candidate empty
case's 40878080-byte absolute RSS maximum and the baseline prepared case's
40812544-byte maximum remain in the ranges. BSD `/usr/bin/time -l` measures
the Go process's absolute maximum RSS, including runtime, fixture, preparation
and all calibration/admission frames. It is not retained grid size or a heap
delta. The 20 ms guard samples sum the private group's Go process and time
wrapper; sampled direct-wrapper RSS is recorded separately and is not Go RSS.
Sampling can miss peaks, so the group guard is not a kernel hard ceiling.

## Work accounting and preserved failed attempt

The complete run has 50 groups, 25 pairs and 300 render calls: 150 frames
represented by reported N=3 results, 50 N=1 calibration frames and 100
admission frames. Go's benchmark framework performs runN(1) followed by
runN(3); each executes one admission frame outside ResetTimer. Every measured
frame includes SHA-256 of its RGBA bytes and is checked against its admission
hash. All versions and samples matched; the three sparse lifecycle/API cases
also share the same hash:

| Cases | RGBA SHA-256 |
| --- | --- |
| WarmSparse128, PreparedWarmSparse128, ColdSparse128 | `44c688b065968f5639d6c131ca163e6e0dfe62cc66c5fcfafc2e6e899d43840d` |
| WarmEmpty128 | `4c7eea521d2218c5965fd3666c694a967a939e29a6928d528b6ed1101176b8ac` |
| WarmDense128 | `db972233ae9c49e07b2e736d0f2e623577fd690562d247a7d8032e74cb86dc4d` |

Warm timing excludes source construction, preparation and the admission frame;
the candidate's admission primes its grid. ColdSparse timing includes each
BuildVolume, first render/grid build, Close and output hash. Source fixture
creation is outside timing. The baseline has no prepared-surface API, so its
test-only adapter passes the already prepared Volume to the old surface API;
the candidate uses RenderVRSurfacePreparedContext. This is the qualified
comparison, not an assertion that the baseline exported that new API.

The selector is `^BenchmarkVRSurfaceSpaceLeap$/^<case>$`: Go testing splits
benchmark patterns at slash, so each component needs its own anchors. An
initial run used an unanchored second component and admitted both
WarmSparse128 and PreparedWarmSparse128. It stopped after one reaped, empty
group with twelve actual render calls when the strict parser rejected two
result rows. That attempt remains FAIL and contributes no performance result.
The new protocol preserves all five cases, pairs, caps and binaries; it is a
complete separate run rather than selected retries of favorable cells.

The complete run used a 120 s shared cap, 10 s/process, a 1 GiB sampled group
RSS cap, 4 MiB per log, and fatal STOP on any error. All 50 direct children
were reaped and all groups were empty before metrics parsing or the next cell.
Its 11451 sealed source/binary/harness inputs matched before and after.
Explicit binary-file reads warmed OS file cache before each pair; caches were
not cleared. Process launch, the time wrapper and 20 ms guard polling are
outside the Go benchmark's frame latency, but included in absolute RSS and
campaign duration. The successful complete campaign lasted 11.125974 s.

The product/test sources and binaries remained frozen throughout both
attempts. This final document alone was updated in a separate snapshot, so
both measured input seals and raw outputs remain revalidatable. Native full
render tests, focused race/Go 1.22/vet, 24 independent public baseline/candidate
RGBA frames, and static review qualify correctness separately. These are
headless synthetic results, not clinical validation, PACS proof, desktop CPU
fallback permission or an application GPU benchmark.
