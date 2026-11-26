# Intensity statistics by demand

`roi.IntensityMomentsVisit` streams scalar samples through the existing
`IntensityAccumulator`. It retains no voxel-sized sample array and performs
no order selection or texture analysis. `IntensityStatisticsVisit` validates
`maximum` and `maxBytes` before allocating its sole private float64 buffer,
collects finite samples once, and sorts it in place with cancellable heapsort.
The visitor emits synchronously and must stop at an emit error; a swallowed
emit error is also retained by the collector. The caller retains its inputs.
`NewIntensityStatistics` continues to copy caller values.

Both APIs discard NaN and positive/negative infinity into `NonFinite`. Moments
use unbiased sample variance (n−1); a single finite sample has zero variance.
A zero finite count is unavailable, not a measured zero. Percentiles are exact
linear interpolation at rank p/100 × (n−1), the existing type-7 convention.
An empty collection has no percentile and the new visitor result's median is
NaN. Values and masks are never modified.

`SampleStorageBytes` reports eight times the private buffer's capacity, including
unused slots after non-finite filtering. This is logical retained storage, not
RSS or allocator overhead. Copies share immutable percentile storage; the
collector marks its already sorted order complete before exposing results.

`VolumeMask.Summary` obtains count and physical volume with the same voxel
boundary convention as shape statistics, including irregular slice positions.
It does not build shape runs or calculate principal axes, surfaces or radiomics.
`ShapeWorkspaceBytes` conservatively admits the dense extrema planes and
Crofton row indices from dimensions, plus run/point/tree and axis scratch;
foreground count alone is insufficient for sparse distant voxels.
`TextureFeaturesBounded` admits a conservative logical workspace upper bound
for collection growth, the dense grid, all directional matrices and feature
scratch before sampling. It then calls the existing GLCM/GLRLM kernel without
changing its mathematics. Zero/invalid bounds refuse admission; it is opt-in
and leaves `TextureFeatures` defaults unchanged.

Synthetic independent oracles and cancellation/admission tests are in
`roi/intensity_visit_test.go`. The two-size summary/full benchmarks generate
values procedurally, without a resident input float64 array. Actual benchmark
and process-memory qualification is separate; no speedup or RSS reduction is
claimed from static implementation.
