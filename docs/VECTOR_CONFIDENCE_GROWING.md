# Vector confidence connected growing


## Input, alignment and units

`VectorConfidenceVolume` declares a physical `VoxelGrid` in mm and a nonempty
`FrameOfReferenceID`. Every `VectorConfidenceChannel` supplies its own identical
grid/frame, unique ID, raw Unit, Offset, positive Scale and immutable `ValueAt`.
Numerical equality of all grid fields is required, without a tolerance or an
implicit registration. IDs, units and frame IDs have a 256-byte limit. Grid steps
must span 3D even for a single slice, and geometry/transformed corners must be
finite. Declared matching geometry is the caller's alignment assertion, not a
validation of acquisition or registration accuracy.

Each normalized value is `(raw-Offset)/Scale`. For unchanged units use Offset=0,
Scale=1 explicitly. A callback's unavailable result, NaN/Inf, nonfinite arithmetic
or normalized magnitude above 1e100 aborts the call with nil result. Values outside
visited coordinates are not queried or certified. Channel data remains under
callbacks; the algorithm does allocate a dense **binary** visitation array.
Callbacks must return promptly and remain stable throughout the call. Cancellation
cannot preempt an arbitrary blocking callback; no goroutine is launched to hide it.

```go
params := roi.DefaultVectorConfidenceParams()
// volume has explicit aligned grids, identities, units, scales and callbacks.
proposal, err := roi.GrowVectorConfidenceConnected(ctx, volume,
    []roi.RegionGrowSeed{{X: 20, Y: 30, Slice: 4}}, params,
    roi.VectorConfidenceOptions{Connectivity: roi.Connectivity6})
```

## Model and seed behavior

Distinct seeds are sorted in grid order. Duplicates are reported and do not weight
the initial model again; out-of-grid seeds refuse the call. `InitialRadius` is a
uniform index box: `(2r+1)^2` samples per seed for one slice, otherwise `(2r+1)^3`.
Border coordinates are clamped and counted again, including overlapping seed
boxes. Radius zero uses distinct seeds only. At least two samples are required,
including for ridge. A neighborhood containing an unavailable channel refuses
the call instead of selecting a smaller set of samples.

Multivariate Welford updates estimate the mean and centered cross moments.
`VectorCovarianceSample` is the zero/default normalization and divides by `n-1`.
Explicit `VectorCovariancePopulation` divides by `n`; the result reports this
choice, and an unknown enum refuses before callbacks. Both require at least two
samples. The default covariance policy
`VectorCovarianceRequirePositiveDefinite` refuses singular or poorly admitted
matrices. `VectorCovarianceRidge` adds the explicitly supplied `Ridge*I` in
**normalized** units; raw channel units do not determine this diagonal addition.
Ridge must lie in [1e-200,1e200], and must be zero for strict covariance.

Cholesky factors the symmetric applied covariance. Every pivot must be positive
and its ratio to the largest applied diagonal must exceed `MinPivotRatio`. The
division preserves this rule at subnormal matrix scales. Zero ratio selects
1e-12; explicit ratios range from 1e-15 to 1e-2. This pivot rule is an admission
profile, not a complete condition-number bound. No pseudo-inverse, channel
removal, covariance clamping or inferred regularization is performed.

Pivot admission depends on the declared channel order. Ordinary admitted models
have permutation-equivariant distances, but the finite conditioning profile can
refuse one ordering where another passes; no universal admission invariance is
claimed at a pivot boundary.

For applied covariance `A=L Lᵀ`, distance is `||L⁻¹(v-mean)||₂`. The norm uses
`math.Hypot`; it need not square an extreme finite distance. Nonfinite solves
still refuse. Classification is `distance <= EffectiveMultiplier`, without an
epsilon. The effective radius is the maximum of the requested multiplier and
all seed distances for each model. This expansion is reported, so a small
requested multiplier is not falsely presented as the actual selection boundary.
A multiplier is a distance parameter; it is not a dimension-independent Gaussian
coverage probability.

The qualifying components connected to seeds are filled. Connectivity 6/18/26
becomes 4/8/8 in a single slice. `Iterations` is the number of model refits and
regrows after the initial fill; zero means one fill. There is no convergence or
zero-variance early-stop. Refits traverse the accepted voxels in grid order.
`Mean`, `Covariance`, `AppliedCovariance`, `ModelSamples` and effective radius
describe the model used by the **last fill**, not post-fill mask statistics.
Defaults are multiplier 2.5, four refits and radius 1; the zero params struct is
invalid because its multiplier is zero.

## Resource and cancellation contract

Zero limit fields select defaults; negative integer or over-hard values refuse.

| Limit | Default | Hard maximum |
| --- | ---: | ---: |
| Channels | 16 | 32 |
| Grid voxels | 2^24 | 2^26 |
| Region voxels | 2^22 | 2^26 |
| Initial samples, including border/overlap repeats | 2^20 | 2^24 |
| Input seeds, before deduplication | 4096 | 65536 |
| Fills | 16 | 65 |
| Work units | 2^29 | 2^34 |
| Estimated bytes | 128 MiB | 1 GiB |

For grid voxels `N`, region queue capacity `R=min(N,MaxRegionVoxels)`, input seed
count `S`, channels `C`, dimensions `Y,Z`, the structural estimate is:

`N + 20R + 4S + 8SC + 24YZ + 512Z + 8(16C²+16C) + 1024C + 4096` bytes.

This admits visitation, the uint32 queue, seed indices/values, conservative
matrix/result scratch, maximum run payload, row headers, metadata, mask objects
and a map-entry allowance before allocating workspace. Output runs are counted
exactly and allocated once, with each row's capacity restricted to its own span.
There is no capacity-doubling per-voxel queue/map or dense vector-channel cache.
Actual output may occupy far less than this estimate.

Estimated bytes are not RSS, Go heap, GC reservation, a literal implementation-
independent map allocation cap, or callback/input memory. Runtime allocator/map
overhead is outside that literal guarantee. A callback may allocate arbitrary
memory. Time, cumulative allocation and process peak memory are separate metrics.

`Work` is a deterministic admission counter, not elapsed CPU instructions:

- Initialization: `N+R+S(1+4*bitlen(S))+4C²+4C`, conservatively charging the
  bounded seed sort and zero-initialized principal buffers.
- Each vector read: `3C`; CallbackReads separately counts attempted callbacks.
- Sample update: `4C+3C(C+1)/2`; covariance completion: `C²`.
- Matrix factorization: `2C²+2C³`; each distance: `C²+3C`.
- Refit reset: `C+C²`, plus `N` state visits; every fill clears `N` states.
- Each queue removal, selected neighbor candidate and visit attempt costs one.
- Output scans: `2N+YZ`, plus exact runs and occupied-slice row headers; result
  model/metadata copy costs `2C²+2C`.

Each block is charged before execution, without counter overflow. Grid products,
channel/seed/neighborhood/fill bounds, the structural estimate and mandatory known
work minima are admitted before their corresponding allocations. A later work
or region exhaustion returns nil result. Context is checked at entry, before and
after each callback, every at most 1024 sequential bulk visits and before returning.
Seed validation/copy/deduplication and an in-place bounded heapsort are polled;
the output counter includes row sentinels, even for one-column masks. Context is
checked before workspace allocations. A single Go allocation/GC pause cannot be
interrupted internally by this API; checks occur before/after those allocations.
Errors preserve `errors.Is` for the existing region growing sentinels and context
cancellation/deadline. Additional sentinels distinguish geometry, unavailable
channels, numeric failure and covariance rejection. Errors contain channel IDs
and coordinates but do not include voxel intensities. Returned arrays and masks
are owned by the caller; no successful partial result is returned on any error.

## Local qualification and external oracle

Synthetic fixtures distinguish classes whose first channel is identical. The
small 3x3 initial box independently has mean zero and diagonal sample covariance
0.75. The 3D box has 27 samples and diagonal 18/26. Tests include independent
centered covariance/direct 1D or 2x2 inverse calculations, dense fixed-point
connectivity, explicit singular/ridge policies, scaled units/channel permutations,
duplicate seeds, border repeats, callback failures, finite extremes, resource
boundaries, cancellation, ownership and concurrent immutable inputs.

The initial sealed comparison observed ITK covariance divided by `n`, while
the library's approved sample default divided by `n-1`. All eight selected
masks and means matched before covariance checks failed. That diagnostic is
preserved. The explicit population normalization supplies the corresponding
model without changing the sample default or widening tolerances.

The opt-in public-API oracle selects `VectorCovariancePopulation` and is pinned to ITK5.4.5, commit
`f51594ad88194a72cdc06a314dafff709fcdc2e9`. It qualifies only the selected regular
one/two-channel 2D/3D fixtures with iterations 0/2. The source in
`roi/testdata/vector_confidence/oracle.cpp` calls ITK; it does not reproduce its
algorithm. Build output is separate from the SDK and repository. With an explicit
readonly SDK and RegionGrowing public include directory:

```sh
cmake -S roi/testdata/vector_confidence -B /tmp/vector-confidence-oracle \
  -DCMAKE_BUILD_TYPE=Release -DITK_DIR=/absolute/readonly/ITK-build \
  -DVECTOR_CONFIDENCE_ITK_REGION_GROWING_INCLUDE=/absolute/ITK-source/Modules/Segmentation/RegionGrowing/include
cmake --build /tmp/vector-confidence-oracle --parallel 2
DICOM_GO_VECTOR_CONFIDENCE_ITK=1 \
DICOM_GO_VECTOR_CONFIDENCE_ITK_BIN=/tmp/vector-confidence-oracle/vector-confidence-oracle \
  go test ./roi -run '^TestVectorConfidenceITKOracle$' -count=1 -timeout=5m
```

An unavailable binary fails when opt-in is enabled; it does not silently skip or
use historical results. Each process has a 30-second context and a 64-KiB output
limit. Use a separately supervised private process group for a bounded campaign.
Exact mask indices and absolute mean/covariance tolerance 1e-12 are prescribed
in `fixtures.json` before running. Singular/ridge handling, metadata, unavailable
values, expansion and arbitrary inputs are explicit library profiles; this is
not blanket ITK equivalence. The sealed local population comparison passed all
eight prescribed cases: one/two channels, 2D/3D, and zero/two refits, with exact
mask indices and the unchanged absolute mean/covariance tolerance 1e-12. The
initial sample-normalization comparison remains a preserved failed diagnostic;
it is not counted as successful external parity.

`BenchmarkVectorConfidence` provides opt-in descriptive 2D/3D cost fixtures. A
separate sealed external harness measured the public API without executing that
benchmark. Local qualification used Go1.27.1 on Darwin27.0.0/macOS27.0.1 arm64,
GOMAXPROCS=1, GOGC=100 and soft GOMEMLIMIT=1GiB. Inputs were two synthetic lazy
channels, sample covariance, radius 1, multiplier 2.5 and strict covariance or
explicit ridge 0.1. The 2D grid was 64x64x1 with 2048 selected voxels; 3D was
32x32x32 with 16384 selected voxels. No parameters, expected counts or criteria
were changed after observations.

One campaign ran 32 fresh private processes. Each made one reported warmup and
three measured calls: 128 API calls, 32 warmups and 96 measurements, with 12
timing/allocation samples and four whole-process RSS observations per cell. GC
ran before each call, outside its timer and allocation interval. Every call's
selected count matched its prescribed count. All processes exited successfully
and their private groups were reaped and empty; no timeout, cleanup, retry or
sample filtering occurred. Source, harness, binary and observer bindings were
unchanged before/after measurement.

The table reports minimum/median/maximum. Time covers only the public API and is
rounded to 0.001 ms. `TotalAlloc` is cumulative allocated bytes per call; `Mallocs`
counts allocations, and all 12 observations were identical for these two metrics
within each cell. `HeapAlloc` endpoints remain distinct observations in the raw
data and are not peak measurements. RSS is the kernel maximum for each private
harness process's entire lifetime, including runtime, warmup, all measured calls,
GC and output; it is not an exclusive API peak or allocated bytes per operation.
The installed `/usr/bin/time -l` byte units were qualified by two separate control
processes with zero product calls, including a touched 64 MiB allocation. No
control-baseline subtraction is used to invent an incremental API peak.

| Grid | Refits | Policy | API ms (min / median / max) | TotalAlloc B (min / median / max) | Mallocs (min / median / max) | Whole-process RSS B (min / median / max) |
| --- | ---: | --- | ---: | ---: | ---: | ---: |
| 2D | 0 | strict | 0.176 / 0.197 / 0.207 | 24288 / 24288 / 24288 | 15 / 15 / 15 | 8208384 / 8232960 / 8273920 |
| 2D | 0 | ridge 0.1 | 0.187 / 0.196 / 0.201 | 24288 / 24288 / 24288 | 15 / 15 / 15 | 8142848 / 8192000 / 8306688 |
| 2D | 2 | strict | 0.652 / 0.675 / 0.692 | 24288 / 24288 / 24288 | 15 / 15 / 15 | 8077312 / 8151040 / 8208384 |
| 2D | 2 | ridge 0.1 | 0.654 / 0.675 / 0.694 | 24288 / 24288 / 24288 | 15 / 15 / 15 | 8011776 / 8159232 / 8323072 |
| 3D | 0 | strict | 1.664 / 1.683 / 1.754 | 212424 / 212424 / 212424 | 79 / 79 / 79 | 7995392 / 8110080 / 8323072 |
| 3D | 0 | ridge 0.1 | 1.650 / 1.670 / 1.797 | 212424 / 212424 / 212424 | 79 / 79 / 79 | 8175616 / 8224768 / 8339456 |
| 3D | 2 | strict | 5.603 / 5.736 / 5.826 | 212424 / 212424 / 212424 | 79 / 79 / 79 | 8044544 / 8142848 / 8175616 |
| 3D | 2 | ridge 0.1 | 5.705 / 5.832 / 6.024 | 212424 / 212424 / 212424 | 79 / 79 / 79 | 8011776 / 8118272 / 8372224 |

These absolute observations describe only this host and these fixtures. The
coordinated quiet window does not establish machine isolation, statistical
significance, a comparison baseline, speedup, peak-memory improvement or clinical
quality. No Twin default is adopted. The structural admission estimate and soft
runtime memory setting remain separate from measured RSS. Full-library `make
check`, focused race and Go1.22 external-link qualification are separate from the
optional oracle and descriptive cost campaigns.

Conceptual references: [ITK public filter documentation](https://docs.itk.org/projects/doxygen/en/stable/classitk_1_1VectorConfidenceConnectedImageFilter.html)
and [NIST multivariate quadratic statistics](https://www.itl.nist.gov/div898/handbook/pmc/section3/pmc341.htm).
The kernel is independently implemented from the mathematics; no external filter
implementation is transcribed.
