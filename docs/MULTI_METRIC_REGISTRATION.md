# Weighted image and paired-point metrics

`registration.NewCompositeMetric` is a public `Metric` usable through the
existing `Method.NewMetric` factory or `Objective` and the `optimize` package.
A composition contains labeled `WeightedMetric` entries. Only concrete library
image, paired-point and Gaussian pointset components are admitted; embedding wrappers and opaque
callbacks cannot bypass the component admission contract. Image channels use
`NewImageMetricComponent`: mean squares or negative squared normalized
correlation, with separate fixed/moving images and `MetricDomain` masks/points.
`NewPointSetMetric` compares explicitly corresponding patient LPS points. It
is paired least squares, not nearest-neighbor ICP. `NewGaussianPointSetMetric`
adds bounded local Gaussian densities with matching-label neighborhoods and
a stable negative-log objective; see [Gaussian pointsets](./GAUSSIAN_POINTSET_REGISTRATION.md). Existing metrics, optimizers
and transform APIs keep their previous contracts. Twin integration is outside
this library delivery.

## Objective, units and diagnostics

The objective and its derivative are exactly
`sum(weight[i] * value[i] / scale[i])` and
`sum(weight[i] * derivative[i] / scale[i])`. Weights are finite and nonnegative;
zero excludes the entry and releases the composition's reference to its metric.
Scale is a finite positive constant; zero selects one. Neither weights nor
parameter gradients are normalized dynamically. Unit-weight, unit-scale single
components reproduce their scalar objective; floating-point reduction order
may differ from the existing parallel image metrics within roundoff.

Mean squares measures squared intensity differences, correlation is
unitless and paired points measure mean squared Euclidean distance in mm².
The image `Units` label is caller-declared objective metadata, not automatic
conversion or calibration. Scales must be selected explicitly to make mixed
units meaningful. Arbitrary weight choices do not imply anatomical improvement.

`Evaluate(ctx, transform, derivatives)` returns ordered `MetricContribution`
entries with labels, weights, scales, raw/weighted values, frame, units,
attempted point count and actual overlap. Excluded entries are explicit.
If any active component fails, the objective is invalid: value zero, nil
derivative, returned error. Already computed contributions and the failed
entry are diagnostic only. No failed component is silently dropped; subsequent
components are not evaluated after the failure. Empty masks/no overlap fail
with `ErrTooFewSamples`; correlation additionally requires two samples and
positive variance in both images. All retained input samples, points, weights,
scales, mapped physical coordinates, continuous indices and final arithmetic
must be finite. Invalid mapping/index arithmetic fails the whole composition;
it is never classified as a missing overlap sample. Mask indices are checked
for finiteness and half-voxel floating bounds before conversion to integers;
large finite indices outside those bounds are ordinary non-overlap. Underflow/overflow in weighted
arithmetic can cause a rejected evaluation; weights are not rescaled to hide it.

## Shared parameterization and physical domain

Every active component declares the same nonempty `FrameOfReference` identifier
for physical LPS mm. This is an explicit caller assertion of a shared coordinate
frame, not discovery of registration between unrelated DICOM frames. Image and
mask geometries can differ: the existing physical interpolation and nearest-mask
sampling map them in that common frame. Sample points outside fixed/moving images
or masks do not overlap. Empty point domains and unmatched/empty paired sets are
rejected. Point pairs must be actual correspondences; the library cannot infer
semantic landmark identities from coordinates.

The composite snapshots a prototype transform. Evaluations require the same
concrete transform type, `Kind`, parameter count and fixed parameters, including
center and Euler order. Parameters themselves may vary during optimization.
All components evaluate that same supplied transform. No independent transforms
are hidden in individual components. Concurrent mutation of the transform is
forbidden; read-only evaluations with separate transform clones are supported.

## Sampling, ownership and cancellation

Constructors receive a context and copy images, masks and point arrays after
admission. Input data must remain stable until the constructor returns; subsequent
caller mutation does not alter the component. Image `SampleCount == 0` uses the
exact supplied domain (nil points means all fixed voxels). A positive count draws
fixed-domain indices with replacement, using the local `Seed`, once during
construction. Explicit points remain in caller order unless sampled. Changing
the seed changes the immutable sequence; evaluation does not redraw it.

The current kernels and component reduction are serial. `MetricDomain.Workers`
is an admitted ceiling, not a request for extra workers; varying it preserves
identical results and uses no additional goroutines. Image and point evaluation
check cancellation every 64 attempted samples and before publication;
construction checks during image/point copies. Cancellation returns no partial
objective or derivative, including cancellation during the final sample.
`Method` forwards its run context to metrics implementing contextual value and
derivative methods. Existing metrics without those methods retain their current
behavior; this does not claim in-kernel cancellation for Mattes or old metrics.
Custom transform methods must terminate and obey the transform read-only
contract; a blocking caller-defined transform callback cannot be interrupted.

## Resource admission

`MetricLimits` zero fields select 64 components, 2^24 attempted points,
64 image channels, 4096 transform parameters, a 64-worker ceiling and 256 MiB
logical bytes. Negative integer limits fail. Components and parameter counts
have absolute ceilings of 4096. Every scalar image component counts as one
channel; a paired-point component counts as zero channels; Gaussian pointsets also count as zero
channels, with their constructor reservation described in the linked guide. Attempted samples
are summed across active components, including repetitions from sampling.
Oversized labels/frame identifiers (1024 bytes) or unit labels (256 bytes) fail.

Per image component admission is `4096 + 128*parameterLimit` plus 4 bytes per
copied image/mask voxel and 24 bytes per retained physical sample point.
Paired-point admission is `4096 + 128*parameterLimit + 48*pairCount`.
The composite reserves `8192 + 2048*entryCount + 128*parameterCount +
8*fixedParameterCount` plus the admitted bytes of every active component,
conservatively counting repeated references again. Aggregate channels, points,
components and bytes are checked before the composition copies its entry list.
Each component constructor independently checks its own snapshot budget before
allocating data. A caller building many components should divide its construction
budget explicitly; a rejected aggregate composition cannot undo snapshots the
caller already owns.

These reservations cover logical retained arrays, bounded numerical workspace
and conservative header/string allowances, not allocator metadata, runtime
stacks, caller-owned inputs/transforms, optimizer storage or process RSS. Worker
ceilings are finite admission values, not a peak-memory or performance promise.
No whole sample buffer or channel-volume concatenation is created in evaluation.

## Local qualification recipe

Focused synthetic tests compare raw/weighted sums and derivatives with independent
quadratics and central finite differences; compare unit components to existing
mean squares/correlation; exercise masks, overlap, frame/family/fixed-parameter
mismatches, finite resource rejection, snapshots, deterministic seeded samples,
concurrent read-only evaluations and cancellation inside a component. Orthogonal
intensity ramps constrain complementary axes at a known translation; conflicting
landmark evidence moves the analytic optimum as weights change. Optimizer and
`Method.NewMetric` tests exercise the existing adapters. These are synthetic
mathematical checks, not clinical validation or external PACS evidence.

Run the focused package selectors, affected optimizer regressions, a relevant
read-only/cancellation race selector, and the library `make check` before delivery.
An independent ITK comparison is optional and has not been executed for this
profile. No hosted CI, GPU, GUI or Twin adoption is required by this profile.
