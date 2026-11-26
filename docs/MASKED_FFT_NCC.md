# Masked linear FFT normalized correlation

`registration.EstimateMaskedFFTTranslation(ctx, fixed, moving, fixedMask,
movingMask, options)` proposes an integer translation for compatible scalar
2D images (`z=1` on both) or 3D volumes. Masks are separate images, aligned
exactly to their respective image origin, size and index-to-LPS matrix;
positive mask samples include a voxel, all other finite samples exclude it.
Inputs remain stable during the call. The routine neither changes images nor
applies registration. Scores are correlation coefficients, not probabilities.

## Coordinate and peak contract

A shift `s` pairs fixed index `i` with moving index `i+s`. Its fixed-to-moving
translation is `moving.origin-fixed.origin + fixed.indexToPhysical*s` in LPS
millimetres. Equal index-to-physical matrices are required (element-wise
relative tolerance 1e-10), including spacing and direction. Origins and sizes
may differ. No implicit resampling, rotation, scale fitting or frame-identity
inference occurs; the caller establishes a common patient coordinate frame.

Each axis is padded to the checked next power of two of `fixedSize+movingSize-1`.
All shifts from `1-fixedSize` through `movingSize-1` are evaluated as linear
correlation lags; negative lags wrap only in FFT storage. Circular image overlap
never participates. An optional inclusive physical LPS box limits admitted
candidates, including origin offset. SearchBoundary means an adjacent integer
shift along an active axis lies outside this box, so capture may be clipped.
It is diagnostic, not proof that the requested box contains the true solution.
LinearBoundary marks an extreme linear lag on a non-singleton axis. Such peaks
are retained for diagnostics but discarded as usable proposals. Ambiguous
marks more than one distinct shift within AmbiguityTolerance of the best
score, including neighboring shifts. Usable excludes ambiguous and linear
boundary peaks. Inspect SearchBoundary independently before adoption.

Peak ties within 1e-12 use deterministic z/y/x order; ambiguity is separately
counted using the requested tolerance. There is no subvoxel interpolation:
resolution is one index step transformed into physical coordinates, and peaks
between indices can be biased or missed. Existing parametric registrators may
refine a reviewed proposal. DSA phase correlation is unchanged.

## Correct masked normalization

For the actual intersection `I_s={i:fixedMask(i)>0 and movingMask(i+s)>0}`,
compute `n`, `sumF`, `sumM`, `sumFF`, `sumMM` and `sumFM`. NCC is

```
(sumFM - sumF*sumM/n) /
sqrt((sumFF-sumF²/n)*(sumMM-sumM²/n)).
```

Six cross-spectra `conj(FFT(A))*FFT(B)` recover those six moments using mask,
mask-times-intensity and mask-times-squared-intensity fields. Multiplying both
images by masks and using unmasked global normalization would be incorrect.
Before FFT, each included intensity is centered by its own mask-wide mean and
scaled by its largest absolute centered value. This positive affine change
preserves each overlap's NCC and reduces numerical dynamic range. Mask overlap
is rounded to an integer with a 1e-5 residual admission check. Variances at or
below `1e-10*n` in these scaled units are unresolved and excluded. Scores beyond
[-1,1] by more than 1e-6 fail; smaller roundoff is clipped. This explicitly
limits nearly constant, extreme dynamic-range and very large numerical cases.

MinOverlap defaults to 3; MinOverlapFraction defaults to 0.1 of the smaller
included mask population. Both constraints apply. AmbiguityTolerance defaults
to 1e-8; zero selects defaults rather than an explicit zero threshold. Empty
masks or no sufficient candidate return ErrMaskedFFTNoOverlap. Zero global
variance or no resolvable candidate variance return ErrMaskedFFTVariance.
Invalid geometry/options/resource limits and any nonfinite sample (including
outside masks), FFT or physical arithmetic return ErrMaskedFFT. Errors and
cancellation return a zero result, including at the terminal publication check.

## Resources and cancellation

Admission happens before volume scratch allocation. Default combined input
limit is 2^22 voxels (images plus equally sized borrowed masks are verified);
padded limit is 2^22 voxels, logical scratch limit 768 MiB and FFT work limit
2^32. Nonzero options override these caps; integer sums/products/padding are
checked. Scratch is `160*paddedVoxels + 16*maxPaddedAxis*fftWorkers` bytes:
six complex spectra, one complex inverse map, six real moment maps and one
complex axis line per FFT worker, where `fftWorkers` is
`max(1, paddedVoxels/2^18)`, bounded by a nonzero Workers option.
Slice/object headers, borrowed input storage and Go runtime/RSS are not
included. The caller retains no scratch after return. MaxFFTWork bounds
`12*paddedVoxels*sum(log2(paddedAxis))`, a transform-work proxy, not elapsed time
or an exact machine-operation counter. Candidate loops are bounded by padded
volume and constant passes. Context checks occur in input scans, field fill,
spectrum products, every axis line, FFT butterflies, moments, candidate scans
and final publication. Individual allocations are bounded but not interruptible.

The twelve 3-D transforms run their axis lines in parallel: the
calling goroutine plus helpers from the module's shared concurrency budget,
at most `fftWorkers` (one per 2^18 padded voxels, after ITK's FFT thread
limit) and at most Workers when it is nonzero. Smaller transforms run on the
calling goroutine alone. Each line is transformed exactly as serially, so
results are bit-identical for every worker count. Field fill, spectrum
products, moments and the candidate scans stay serial. Read-only
concurrent calls have independent scratch and deterministic reduction/order.


| Fixture (padded) | Before median | After median | Change (benchstat) |
| --- | ---: | ---: | ---: |
| 64³ volumes (128³, 8 FFT workers) | 4.303 s ± 46% | 1.810 s ± 66% | -57.93% (p=0.015) |
| 512² images (1024², 4 FFT workers) | 2.118 s ± 41% | 1.148 s ± 93% | not significant (p=0.093) |

Bytes per call grow by the extra axis lines (+0.08% and +0.44%).
Concurrent mutation of image data/geometry/masks or options is unsupported.

## Qualification recipe and current limits

The prespecified focused tests cover independent direct two-pass spatial
covariance, every selected lag including the reverse convention, irregular
independent masks, contrast scaling, different dimensions, small overlap,
oblique anisotropic coordinates, physical clipping, repeated peaks, linear
border rejection, degeneracy, admission, axis roundtrip and cancellation.
The concurrent test and cancellation test are the affected race selector.
Existing NCC/FFT and DSA callers remain part of the affected baseline.

`BenchmarkMaskedFFTEquivalentSearch` compares the complete FFT estimator
(including admission/allocation/transforms) against exhaustive spatial search
with identical masks, overlap constraints and physical search box. Fixtures
32x24x1 and 12x10x6 are prepared in the parent before each child timer; every
child checks its result. Run three 200ms samples of each of four rows, report
all twelve samples plus median/range, ns/op, B/op and allocs/op. Actual root execution passed all fifteen focal tests, two affected race tests
and twelve benchmark rows (three samples per series), without skips or failures.
Measured native Darwin arm64 / Apple M4 Pro, Go 1.27.1 costs are recorded below. Neither a gain nor capture-range
advantage is presumed; bounded search can favor spatial search on small inputs.
Native RSS is not measured by this benchmark and must not be inferred from B/op.

Optional `TestMaskedFFTITKOptIn` accepts an externally generated JSON artifact
via `DICOM_MASKED_FFT_ITK_ORACLE`. Use the exact deterministic depth-three
`maskedFFTFixture` values/masks and options MinOverlap=12,
MinOverlapFraction=0.2. Record ITK version and fixed/moving index convention,
and JSON Provider="ITK", Version, Convention="movingIndex=fixedIndex+shift",
Shift=[x,y,z], Score and Overlap. Peak/search and mask normalization settings
must match this guide; generate independently rather than copying expected
values from this implementation. The adapter verifies an artifact, not its
producer authenticity; absent artifacts skip this optional check. The original single-artifact adapter was not executed in this qualification;
a separate root-owned twelve-row ITK matrix comparison was executed as
described below. Synthetic numerical
agreement is not clinical validation. No Twin UI integration is included.

The public mathematical context is Dirk Padfield's [Masked Object Registration
in the Fourier Domain](https://pubmed.ncbi.nlm.nih.gov/22203712/),
DOI 10.1109/TIP.2011.2181402. This implementation independently expands the
intersection covariance into six cross-correlations; no ITK source is copied.


## Actual local cost, three samples per series

| Fixture / implementation | ns/op min / median / max | B/op min / median / max | allocs/op |
|---|---|---|---|
|32x24x1 FFT|1148934 / 1150218 / 1164245|656432 / 656432 / 656433|15|
|32x24x1 spatial|138620 / 138833 / 140619|0 / 0 / 0|0|
|12x10x6 FFT|4573884 / 4584959 / 7010818|2622000 / 2622000 / 2622108|15|
|12x10x6 spatial|492131 / 494121 / 501569|0 / 0 / 0|0|

All twelve rows are retained; the larger first 3D FFT sample is included.
FFT was slower and allocated more on both fixed fixtures. The comparator
performs the same spatial search box, mask overlap and peak score/count, but
omits FFT's second ambiguity pass and usability diagnostics. Therefore these
are declared estimator-versus-spatial-search workloads, not identical complete
diagnostic work or evidence of an algorithmic speed advantage. No RSS was
measured. This evidence uses GOMAXPROCS=2, GOMEMLIMIT=1GiB, -p=1,
-benchtime=200ms and -count=3; fixture construction is outside child timers.

A separate root-owned opt-in ITK 5.4.5 producer executed four profiles:
known 2D/3D unequal-size, oblique anisotropic masks with explicit origin offsets,
and the two cost fixtures above. Its absolute overlap threshold reproduces the
Go threshold; ITK fraction is zero because its built-in fractional denominator
is the maximum observed overlap rather than the smaller mask population.
The producer verifies every eligible searched lag against independent spatial
covariance and declares output-index-to-lag conversion. Three fresh filter
instances per profile record filter creation/configuration/Update time; input
image construction, map scanning and spatial oracle validation are outside
that timer. Separate direct-search times and all samples are retained.
Those actual C++ timings are not identical to the complete Go estimator
boundary, and must not be used as cross-language speed ratios. All four
profiles, twelve producer rows and the separate Go matrix test passed.


## Actual opt-in ITK scope and precision

Native ITK 5.4.5, Release C++17 with one ITK thread and its existing VNL FFT
backend (FFTW disabled), passed configure/build and produced three samples for
each profile. The independent two-pass covariance checked every eligible lag,
then an actual Go matrix test compared all twelve rows' peak, score, overlap
and true-geometry LPS translation against the public estimator. Inputs were
synthetic; this is numerical qualification of those inputs, not an anatomical
or clinical benchmark.

| Profile | Fixed / moving sizes | Eligible lags | Peak overlap / minimum | Max absolute ITK vs spatial score error |
|---|---|---|---|---|
|known2D|7x6x1 / 9x7x1|38|30 / 12|1.9984014443252818e-15|
|known3D|7x6x3 / 9x7x4|184|88 / 21|1.0158540675320182e-14|
|cost2D|32x24x1 / 32x24x1|49|489 / 123|6.661338147750939e-16|
|cost3D|12x10x6 / 12x10x6|245|408 / 102|1.887379141862766e-15|

Every profile selected index shift (1,1,0), with peak score within 2e-15 of 1.
The known profiles use xy rotation 0.6 rad, spacing (1.25,2.5,3) mm and true
moving-origin offset (10,-7,0) or (10,-7,2) mm. Their physical translations were
(9.62006333514951,-4.2308578709820095,0) and the same xy with z=2 mm. Cost
profiles use spacing (1,1,2) mm and zero origin offset, yielding (1,1,0) mm.
The overlap numbers in this table are independent exact selected mask
intersection counts, not a per-peak overlap output exposed by the ITK filter.

| Profile | ITK filter ns min / median / max | C++ spatial-search ns min / median / max |
|---|---|---|
|known2D|734167 / 748209 / 1128625|21125 / 29625 / 58833|
|known3D|1029625 / 1086208 / 1201375|268542 / 374958 / 401625|
|cost2D|935583 / 938625 / 1126958|84250 / 86750 / 158958|
|cost3D|1726667 / 1857791 / 1968000|303208 / 338000 / 522083|

All samples, including first-call costs, are retained. These timers report
filter creation/configuration/Update and separate exhaustive spatial peak
search, respectively; fixture construction and oracle-map validation are
excluded. No C++ allocation or native RSS measurement was made. The small
fixtures do not support a general speedup claim for either FFT implementation.

The first actual ITK attempt rejected unequal physical origins; that failure
and its native raw/process evidence are preserved. The initial driver also
failed parsing that non-JSON diagnostic and did not produce an oracle result.
The successful producer used a disclosed common-origin metadata adapter only
inside ITK's index-domain filter. Original geometry, pixels, masks, dimensions,
thresholds and search bounds were retained; the true origin offset was applied
externally for LPS conversion. Consequently this comparison qualifies ITK
index-domain NCC and the external physical mapping, not native ITK acceptance
of differing image origins. No ITK admission check was disabled. The updated
driver retains non-JSON diagnostics as failed receipts, and the Go comparison
verified successful producer receipt/raw/binary hashes before and after use.

Qualification executed: focal fifteen tests and affected race two tests,
original twelve Go benchmark rows, ITK twelve rows with spatial checks and
one Go matrix comparison test, all with zero skips or failures in those
successful stages. Historical failed origin admission remains a failure.
The integrated module baseline, documentation checks and minimum-toolchain
qualification are separate delivery checks performed by the integrator.
The SDK, producer executable and runtime artifacts are private qualification
inputs and are not dependencies of the shipped library.
