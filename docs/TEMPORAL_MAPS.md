# Temporal scalar maps

[`dynamic/temporal_map.go`](../dynamic/temporal_map.go) provides
`ComputeTemporalMap` for mean, minimum, maximum and standard deviation, and
`MovingTemporalMean` for one explicitly selected target and bounded window.
These are derived numerical results; they do not alter acquisition values or
automatically smooth cine playback.

`TemporalSource` supplies immutable metadata and scalar frames. Values are
X-fastest on an explicitly declared regular physical grid, with spacing in mm,
orthonormal directions and Frame of Reference. Every accepted frame must have
the exact same grid and unit identifier. The source fills one caller-owned read
buffer sequentially and must not retain or alias it. Registration, resampling
and source decoding are caller responsibilities.

Sample weighting uses 1 per observation. Duration weighting requires actual
positive declared durations; timestamp gaps and display intervals do not create
weights. The mean is `sum(w*x)/sum(w)`. Population variance divides by `sum(w)`;
sample variance uses `sum(w)-sum(w*w)/sum(w)`, treating durations as reliability
weights. Standard deviation is its square root. Weighted Welford accumulation
avoids subtracting two large squared moments; nonfinite intermediate arithmetic
is refused.

Moving windows count temporal positions, including missing ones. Causal windows
end at the target; centered windows require odd length. Clip borders truncate
and renormalize; require-full borders refuse incomplete support. Missing whole
frames and nonfinite voxel observations have separate reject/skip policies.
Missing observations are not interpolated. Output retains validity, counts and
weight sums; no observations, or insufficient sample-variance observations,
produce an explicitly invalid voxel.

Mandatory finite `TemporalLimits` bound frames, voxels and owned payload.
The working-byte estimate includes output, accumulators, one read buffer,
provenance arrays and copied retained metadata text. Text fields have separate
finite limits and are charged before pixels are read. The estimate excludes Go
headers, external source caches/decoding and runtime/allocator overhead. It is neither process RSS
nor a global application memory budget. No frames-times-voxels array is owned.
Applications must account for source metadata, decoding and retained derived
results separately. Cancellation/error returns no partially calculated map.

Provenance retains options, effective indices, timing presence, actual weights,
source identities and supplied registration descriptions. Retained text is
cloned so small source substrings do not pin larger backing data; numeric arrays
also belong to the result. Calling another
moving target is another explicit operation, without a hidden 4D result cache.

Independent synthetic constant/ramp/impulse/irregular-time references, temporal
distortion, bounded-workspace tests and measured library/application costs are
recorded in the Twin integration qualification.
Its application tolerances and finite input profile are narrower than the
general API. Headless numerical and 2D publication evidence does not qualify
clinical use, native Windows execution or GPU rendering.
