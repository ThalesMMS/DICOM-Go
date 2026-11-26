# Gaussian pointset registration

`registration.NewGaussianPointSetMetric` is a reusable, immutable `Metric` and
`MetricComponent`. It works with `Objective`, the existing optimizers,
`Method.NewMetric` when explicitly configured, and the weighted compositor.
It maps fixed points into the moving cloud's patient LPS mm frame. This is a
one-way Gaussian density objective, not paired least squares or ICP. Twin
integration is not included; no contour association or source is changed.

## Scalar objective and exact derivative

For each sampled fixed point `x`, let `y = T(x)` and let `N_K(y)` be the K nearest
eligible moving points. With physical `sigma = SigmaMM`, define

`a_j = exp(-||y-q_j||²/(2*sigma²))`,
`V = -(1/N) * sum_x log(sum_{j in N_K(y)} a_j / K)`.

No `(2*pi*sigma²)^(-3/2)` normalization is included. Thus V is dimensionless,
a negative log of the local mean kernel response. It averages fixed samples,
not moving cardinality or an unbounded full-cloud mixture. Changing moving
density, duplication, sigma or K can change the optimum. It is not a calibrated
probability of anatomical correspondence. There is no gradient normalization.

For an unchanged neighborhood the derivative is
`(1/N) * sum_x sum_j [a_j/sum(a) * (y-q_j)/sigma²] dot dT(x)/dp`.
It differentiates the same scalar objective. Nearest-neighbor sets change at
boundaries: the value is continuous but derivatives need not agree there.
Equal-distance neighbors use original moving input index to break ties; finite
difference qualification uses points away from those boundaries. Parameters
and Jacobians follow the existing transform contract and units (e.g. radians
for rotations, mm for translations).

The implementation shifts exponents by the nearest squared distance:
`value_x = d_min²/(2*sigma²) - log(sum(exp(-(d_j²-d_min²)/(2*sigma²)))) + log(K)`.
This avoids a zero density when every unshifted Gaussian underflows. Individual
far-neighbor relative weights can underflow to zero, while the closest term
remains one. Sigma, sigma² and its reciprocal must be finite and positive;
nonfinite mapped coordinates, differences, squared distances, scaled distances
or gradients fail the complete evaluation. An overflowing distance is not
silently discarded as an outlier. Tiny roundoff around zero is retained rather
than clamped. Failures/cancellation publish value zero and nil derivative.

## Clouds, labels and samples

Both clouds must be nonempty and contain finite points. Exact duplicates are
accepted and retained as density multiplicity; no deduplication or hidden
jitter occurs. By default all labels match. To restrict correspondence, supply
both `FixedLabels` and `MovingLabels` with their exact cloud lengths. Only equal
labels are eligible. Every fixed label, including unsampled input points, must
have at least K moving points; missing labels or excessive K fail construction.
Labels are caller assertions, not inferred anatomy. `FrameOfReference` is a
shared nonempty identifier for LPS coordinates, not an automatic conversion
between unrelated frames.

`Neighbors == 0` selects five; it is never clamped to a small cloud. Explicit K
must be in `[1, MaxNeighbors]` and no greater than eligible moving cardinality.
`MaxNeighbors == 0` selects 1024; greater or negative limits are rejected.
`SampleCount == 0` visits all fixed points in their original order. A positive
count draws with replacement once using `Seed`; the stored index sequence is
reused by every evaluation. Original fixed/moving samples and optional labels
are copied after admission. Inputs must remain stable until construction ends;
subsequent caller mutation is independent of the metric.

## KD reuse, determinism and finite work

The implementation reuses registration's existing `kdTree`, `kdNode` and
coordinate helpers, with new cancellable deterministic construction/query
methods. Existing ICP construction and nearest search stay unchanged. The new
builder merge-sorts index ranges by alternating axes, breaking coordinate ties
by original index, and stores median nodes. Its work is O(M log² M) with linear
indices/sort scratch. Queries maintain only K ordered candidates, use exact
split-plane pruning including distance ties, and may visit the entire admitted
moving cloud for labels or unfavorable geometry. There is no claim of constant
query cost or guaranteed superiority to ICP.

Evaluation and reduction are serial. `Workers` is an admitted ceiling, not a
request for extra goroutines; varying it does not change samples or arithmetic.
Contexts are checked inside snapshot/build/merge operations every 64 items,
inside KD traversal every 32 node visits, inside neighbor sums every 32 entries,
before each fixed point and before publication. Each evaluation has a total
`MaxNodeVisits` quota (zero selects 2^26). A quota below the sample count is
rejected at construction; exhaustion during search returns `ErrPointSetWorkLimit`
without partial objective/gradient. Immutable metrics support concurrent reads
using independently owned transform clones. Concurrent transform mutation and
blocking custom transform callbacks are outside that contract.

## Logical budgets

`MetricLimits` supplies the previously documented finite defaults: 2^24 input
points, 4096 transform parameters and 256 MiB logical bytes, plus the compositor's
component/channel limits. Fixed plus moving input cardinality must fit the
point cap; positive sample count must also fit it. Sample repetition is allowed
and consumes its admitted index/work quota. The frame label is at most 1024
bytes; workers must fit the configured worker ceiling.

Before copying or building, constructor reservation is
`8192 + 128*parameterLimit + 64*K + 32*fixedCount + 160*movingCount + 8*sampleCount`.
It conservatively covers copied point/label arrays, KD nodes, construction
indices/sort scratch, label counts, stored sample indices, neighbor candidates
and parameter/Jacobian scratch. It excludes caller inputs/transforms, optimizer
storage, allocator/runtime overhead and process RSS. The composite counts the
whole reservation again for each active entry, with zero image channels and
sampleCount attempted points; it applies its existing aggregate bounds.
Arithmetic admission uses remaining-byte division before payload multiplication.
No dense fixed-by-moving distance matrix is allocated.

## Synthetic qualification and comparison

Mandatory focused tests compare the KD result with an independent exhaustive
sort and direct Gaussian calculation, compare affine derivatives with central
finite differences, exercise exact ties/duplicates/labels, finite admission and
numerical errors, stable far-point log values, deterministic samples/worker
ceilings, detached snapshots/concurrent reads, constructor/query cancellation
and node quotas. The density objective participates in the existing compositor
without changing old metrics or ICP.

A rectangular 3D cloud translated by `(0.3,-0.2,0.1)` mm provides the known
transform. Complete, duplicated-density, partially observed and fixed-outlier
cases run sigma 0.4/1.6 mm and K 1/3, recording objective, translation error,
nearest RMS and evaluations. K1 complete/density cases must recover the known
translation; partial/outlier/K3 cases report their results without an accuracy
promise. Existing ICP runs on the same cases and keeps its current regressions.

`BenchmarkGaussianPointSetBuildAndEvaluate` includes construction plus one
value/gradient. `BenchmarkGaussianPointSetRegistrationVersusICP` includes each
complete registration call, snapshots and KD construction. It compares Gaussian
translation optimization (sigma1/K1) with ICP rigid fitting on this synthetic
translation, so the transform families and optimization procedures differ.
Cost and geometric error must be reported separately; no timings or improvement
are inferred from source inspection. The bounded local qualification below records actual focused and cost results;
the integrated library baseline remains a separate delivery gate.

## Optional external ITK oracle

`TestGaussianPointSetITKOracleOptIn` reads an externally produced JSON artifact
when `REGISTRATION_GAUSSIAN_ITK_ORACLE` is set; otherwise it skips explicitly.
The artifact must contain `Provider` identifying ITK, `Version`,
`Formula: "negative-log-mean-topk-isotropic-gaussian-v1"`, physical `Fixed`/
`Moving` arrays of `{X,Y,Z}`, `SigmaMM`, `Neighbors`, translation `Parameters`,
independently computed `Value`, `Gradient` and finite `Tolerance` in `(0,1e-4]`.
Generate it with equivalent physical coordinates, sigma, K and exact scalar
normalization. An ITK expectation-based pseudo-gradient with a differently
normalized scalar is not an oracle for this derivative. The adapter validates
schema/value agreement but cannot authenticate a provider string; keep external
producer/version/recipe evidence with the artifact. No ITK oracle was generated
or executed for this implementation; the mandatory analytic oracle is separate.

Run the focused registration/optimizer selectors and relevant read-only race
checks, then library documentation and `make check` baseline. No CI, native
viewer, GPU or external PACS is required. Synthetic matches, benchmark results
and optional numerical comparisons are not clinical validation.

## Local results (macOS arm64, Apple M4 Pro)

Root execution used Go 1.27.1, GOMAXPROCS=2, GOMEMLIMIT=1GiB and the
default host cache. The focused selector passed 15 top-level tests (six new and
nine affected existing tests), and two targeted race tests passed, with zero
failure/skip events. Pre/post guards confirmed 3046 library Go files and all
six owned source/document inputs unchanged. This is a focused result; the
integrated library `make check` baseline subsequently passed in 51.205 seconds.
The combined 33 focused tests also passed on Go 1.22.12 with default vet and no
overlay. Both delivery checks preserved 6041 Go source files across the two
modules and all 12 owned postimages. The optional external ITK oracle was not
configured or executed.

The 16 cases below are four clouds × two sigma values × two K values.
Translation error compares the optimized translation with the synthetic known
(0.3,-0.2,0.1) mm transform; nearest RMS is a different statistic.

| Cloud | Sigma mm | K | Objective | Translation error mm | Nearest RMS mm | Evaluations |
|---|---:|---:|---:|---:|---:|---:|
| complete | 0.4 | 1 | 4.8619442e-13 | 3.9443911e-07 | 3.9443911e-07 | 26 |
| complete | 0.4 | 3 | 1.0986123 | 3.9443911e-07 | 3.9443911e-07 | 26 |
| complete | 1.6 | 1 | 3.0387151e-14 | 3.9443911e-07 | 3.9443911e-07 | 26 |
| complete | 1.6 | 3 | 0.90275068 | 1.0603877e-06 | 1.0603877e-06 | 27 |
| different-density | 0.4 | 1 | 4.8619442e-13 | 3.9443911e-07 | 3.9443911e-07 | 26 |
| different-density | 0.4 | 3 | 0.40546511 | 3.9443911e-07 | 3.9443911e-07 | 26 |
| different-density | 1.6 | 1 | 3.0387151e-14 | 3.9443911e-07 | 3.9443911e-07 | 26 |
| different-density | 1.6 | 3 | 0.32276979 | 1.0367275e-06 | 1.0367275e-06 | 31 |
| partial | 0.4 | 1 | 9.375 | 0.99999945 | 1.7320508 | 36 |
| partial | 0.4 | 3 | 10.473612 | 0.99999945 | 1.7320508 | 36 |
| partial | 1.6 | 1 | 0.5859375 | 0.99999945 | 1.7320508 | 36 |
| partial | 1.6 | 3 | 1.4694477 | 1.154131 | 1.7393074 | 35 |
| fixed-outlier | 0.4 | 1 | 45.37037 | 1.34715 | 3.8103174 | 33 |
| fixed-outlier | 0.4 | 3 | 46.468982 | 1.3471504 | 3.8103174 | 33 |
| fixed-outlier | 1.6 | 1 | 2.8356481 | 1.34715 | 3.8103174 | 33 |
| fixed-outlier | 1.6 | 3 | 3.6486367 | 1.8255883 | 3.850967 | 34 |

| Cloud | ICP RMS mm | ICP mean mm | Stop | Iterations |
|---|---:|---:|---|---:|
| complete | 3.0467987e-16 | 2.6938555e-16 | tolerance | 2 |
| different-density | 3.0467987e-16 | 2.6938555e-16 | tolerance | 2 |
| partial | 1.5030171 | 1.3255698 | tolerance | 2 |
| fixed-outlier | 3.8029512 | 2.3939531 | worsened | 1 |

Complete/density translation error ranged from 3.94e-7 to 1.06e-6 mm.
Partial observation biased the translation by about 1.00–1.15 mm and the fixed
outlier by about 1.35–1.83 mm. ICP had lower nearest RMS on these particular
partial/outlier fixtures; its outlier case stopped with `worsened`. The new
metric does not guarantee outlier robustness, anatomical improvement or a
better result than ICP.

The cost run used the original two benchmark selectors, 200 ms per sample and
three samples, producing nine rows. All three observed samples are retained
below. One-time fixture setup precedes the measured loops. The constructor +
gradient benchmark does not reset its timer, so its initial fixture-array setup
is included and amortized across iterations. For the two registration child
benchmarks, fixture setup is performed by the parent outside the child timers.
Constructors/snapshots/KD building are included in each measured operation.
Only the complete eight-corner cloud and its eight translated points were measured.

| Operation | ns/op samples | ns/op median [min, max] | B/op samples | allocs/op samples |
|---|---|---|---|---|
| Constructor + gradient, sigma1/K3 | 9323, 9496, 9318 | 9323 [9318, 9496] | 6704, 6704, 6704 | 16, 16, 16 |
| Full Gaussian translation, sigma1/K1 | 18325, 18240, 18094 | 18240 [18094, 18325] | 9672, 9672, 9672 | 103, 103, 103 |
| Full ICP rigid | 3010, 2981, 3009 | 3009 [2981, 3010] | 4688, 4688, 4688 | 91, 91, 91 |

The Gaussian full-call median was 18,240 ns/op versus ICP 3,009 ns/op, with
9,672 versus 4,688 B/op and 103 versus 91 allocations/op. Thus it was slower
and allocated more in this tiny fixture. The Gaussian cost benchmark uses
translation optimization at sigma1/K1 and its declared default gradient
tolerance, whereas ICP fits a rigid transform. The numeric matrix above uses
sigma0.4/1.6 and an explicit 1e-7 gradient tolerance. These timing rows are
descriptive costs of different procedures, not accuracy-matched comparisons.
Constructor+gradient and full registrations have different measurement boundaries.

No RSS, large-cloud scaling, partial/outlier registration cost or labeled-query
cost was measured. Costs and geometric errors are reported separately, with no
extrapolation to general method quality or clinical accuracy.
