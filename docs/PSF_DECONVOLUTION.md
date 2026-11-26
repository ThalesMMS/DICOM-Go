# Known-PSF restoration for research

`imagefilter.RichardsonLucy` is an opt-in spatial restoration API for a known,
linear, shift-invariant point-spread function and nonnegative scalar 2D/3D
research or microscopy data. `ConvolvePSFContext` exposes the same forward
operator. Both consume `Volume` with x-fastest float32 samples and spacing in
millimetres. No application tool or default enhancement is enabled.

The noise assumption is independent Poisson observations. Calibrated count data
is the natural domain; arbitrary CT/MR signed intensities or appearance alone do
not justify it. The first profile uses direct convolution, not FFT, Wiener,
Tikhonov or blind estimation. Incorrect PSFs and excessive iterations can amplify
noise and create artifacts; residual reduction does not prove recovered physical
information or clinical utility.

## Model, PSF and field boundary

PSF dimensions must be positive, odd on each axis, no larger than the source,
and have exactly that number of finite nonnegative samples with positive mass.
A 2D source has z=1 and requires z=1 PSF support. On every active source axis,
source and PSF spacing must be equal, finite and positive. Singleton spacing may
be zero but remains finite and nonnegative. Anisotropy is retained; there is no
implicit PSF resampling or voxel-volume multiplication. PSF entries are discrete
mass weights sampled on that matching grid, not a continuous density. The center
index is `(size-1)/2` on each axis. No implicit origin or half-voxel shift exists.

`PSFRequireUnitMass` (default) admits mass within 1e-6 of one;
`PSFNormalizeMass` explicitly admits arbitrary positive finite mass. Both divide
by the admitted mass so the actual operator has unit-mass weights. Original mass
and the policy are recorded. The data-only PSF SHA256 covers the original
float32 entries in little-endian order; dimensions and spacing are separate
metadata fields.

`PSFZeroBoundary` (default) defines `A[i,j]=k[i-j+center]` where the referenced
voxel is inside the field; outside values are zero. This is a finite cropped
operator and its edges lose mass. `PSFCircularBoundary` wraps each axis and models
periodic data. Neither is reflect/replicate padding. No enlarged padded grid is
allocated. The exact adjoint scatters with the same coefficients. Restoration
uses sensitivity `s=A^T 1`, including at cropped edges:

```
x(0) = constant mean(y)
x(next) = x * (A^T [ y / positivePrediction(A x, epsilon) ]) / s
```

All columns of A must be observable (`s>0`); otherwise restoration fails rather
than assign an invented value to unobserved input pixels. This can occur for a
PSF whose only nonzero tap lies off-center at a cropped edge. An all-zero input
with an observable model returns zeros with zero completed iterations.

Data is divided by its maximum nonnegative value for arithmetic, then restored
to physical input units. `Epsilon` is a zero-prediction fallback in that normalized
domain (default 1e-12, at most one), used only when a predicted value is exactly
zero. Every positive representable prediction is retained without clipping,
including small samples beside very large intensities. A nonfinite ratio fails
rather than silently drop its sample. Returned RMSE is in input units. The reported
Poisson deviance uses this same zero-prediction fallback for its logarithm; it is a stabilized diagnostic, not an unstabilized likelihood
certificate. Residuals describe the float64 estimate before final float32
rounding. Overflow/nonfinite restoration or residual arithmetic fails.

## Domain, stopping and ownership

`PSFRejectNegative` rejects negative input (default). `PSFClampNegative` explicitly
sets it to zero in an owned snapshot and records the clamped count. Nonfinite
input and any negative/nonfinite PSF are always rejected. Input pixels never
change. The caller must keep input slices and options references stable during a
call; independent concurrent calls have no shared mutable algorithm state.

Iterations default to 20 and are bounded. `RelativeChange=0` disables early
stopping; a positive value stops when the iteration's L1 estimate change is at
most that value times `max(previous L1 sum,epsilon)`. Reasons are `iteration-limit`,
`relative-L1`, and `zero-input`. This is an estimate-change criterion, not a
clinical or likelihood stopping rule. No monotonic improvement is promised with
all stabilization settings or arbitrary inconsistent observations.

Successful restoration owns a new volume, reference slice and reference string
backing. Its metadata identifies a derived result, method, Poisson model, PSF
hash/geometry, boundary/domain/normalization policies, intensity scale, epsilon,
stopping parameters and actual iterations, and caller-supplied stable source
references. At least one nonblank source reference is required; identifiers are
not interpreted as DICOM UIDs. This metadata is a library result contract, not a
complete DICOM derived-image encoder. Persisting it into an object must preserve
source relationships and derived status. Never replace source pixels or present
restored pixels as acquisition measurements.

Any error or cancellation returns a zero restoration result (nil restored
volume and no published metadata). Forward convolution returns nil on failure.
Cancellation is checked before work and throughout scans, iterations and kernel
visits. It cannot interrupt allocation itself or a single floating-point operation.

## Finite resource admissions

Zero limits select finite defaults. Negative options/limits are invalid.

| Limit | Default | Hard admission ceiling |
| --- | ---: | ---: |
| Source voxels | 1,048,576 | 16,777,216 |
| PSF samples | 4,096 | 65,536 |
| Iterations | 128 | 4,096 |
| Source references | 32 | 1,024 |
| Total reference string bytes | 4,096 | 1,048,576 |
| Scratch payload bytes | 67,108,864 | caller-specified finite int64 |
| Working payload bytes | 100,663,296 | caller-specified finite int64 |
| Kernel visits | 104,857,600 | caller-specified finite int64 |

With `n` source voxels and `k` PSF samples, scratch admission is `48*n+8*k`
(six float64 images and normalized kernel). Working admission adds `4*n` for the
owned float32 output and owned reference byte/header payload. Fixed Go structs,
allocator overhead, hash state, stacks and RSS are outside this logical ledger.
It is not a peak-process-memory guarantee. The forward API conservatively uses
the same ledger although it needs fewer buffers. Work admission is `n*k` for
forward and `n*k*(2*iterations+3)` for restoration, a conservative full-iteration
bound independent of early stopping or sparse taps. Checked dimensions and hard
voxel/PSF/iteration ceilings make this arithmetic representable. All admissions
precede owned numeric/reference allocation. FFT working bytes are exactly zero
in this direct profile; no hidden FFT scratch or overlap-add grid exists.

The algorithm costs O(iterations*n*k) time and O(n+k) space. The finite work
budget may reject modest grids with broad PSFs. Existing `internal/fft` remains
unchanged; a future accelerated backend must qualify identical boundary/adjoint
semantics and separately admit 3D/padding/FFT workspace. This delivery claims no
large-volume speedup or universal superiority to FFT convolution.

## Local and optional peer qualification

The default tests use explicit asymmetric forward/transpose matrices, delta
identity in 2D/3D including dynamic range from smallest float32 subnormal to
maximum float32 (the causal `[1e30,1,0]` control retains its value one), and independently calculated axis-neighbor blur in anisotropic
2D/3D fields. Restoration controls compare known truth within 2e-4 and forward
values within 1e-6, and require residual reduction. They also cover PSF/domain/
geometry admission, logical caps, stopping, derived ownership, cancellation and
independent concurrency. Focused existing imagefilter and FFT regression tests
must accompany integration. Focal, race, minimum-toolchain, baseline and cost
commands are in the private execution recipe. Actual selected qualification is
recorded below; full integration/minimum-toolchain gates remain separate.

`TestPSFITKOracleOptIn` runs only when `DICOM_GO_PSF_ITK_ORACLE` names an external
JSON fixture (maximum 1 MiB, 2–8 cases including 2D and 3D). Its schema is:

```json
{"Producer":"ITK","Version":"actual ITK version","SourceSHA256":"64 hex digits",
 "Model":"matched-known-PSF-Poisson","Cases":[
 {"Name":"case","Dims":[3,3,1],"SpacingMM":[0.2,0.4,1.5],
  "Observed":[],"PSF":[],"PSFDims":[3,3,1],"Expected":[],
  "Iterations":128,"Boundary":0,"Tolerance":0.0002}]}
```

Empty arrays above illustrate fields only, not a runnable positive fixture.
An independent producer must record its actual ITK version/build and generator
source SHA, original inputs, boundary/normalization and initialization choices,
iteration count, output and numerical tolerance. Keep its runtime command and
raw output/hash receipts outside the JSON as well. Tolerance is an absolute
sample error, strictly positive and at most 1e-2; choose it before seeing results.
The loader verifies provenance declarations, geometry, domains and output
agreement; declarations alone do not establish that ITK ran. Root qualification
must verify the external receipts and hashes.

ITK's default padding/initialization and edge treatment must not be assumed to
match this finite cropped, sensitivity-corrected model. Delta is an applicable
control; nontrivial comparisons require independently establishing a matched
operator/boundary and initialization or a separately justified convergence
comparison. If these prerequisites are unavailable, report the oracle as not run,
not passed, and retain analytic local evidence as such. No ITK implementation is
copied. Public behavioral authority:
[ITK RichardsonLucyDeconvolutionImageFilter](https://docs.itk.org/projects/doxygen/en/stable/classitk_1_1RichardsonLucyDeconvolutionImageFilter.html)
and its stated linear shift-invariant known-kernel/Poisson model.

Prerecorded cost profile: `BenchmarkPSFDeconvolutionCost`, grids 16x16x1,
32x32x1, 8x8x8, normalized 3-axis-neighbor PSF, zero boundary, 20 iterations,
`-benchmem -benchtime=100ms -count=5` (15 rows). Setup forward blur is excluded;
restoration preparation, all allocation, iterations, output and residuals are
included. Measured allocation bytes differ from the logical bound and RSS.
Actual cost was measured on Darwin arm64 / Apple M4 Pro, GOMAXPROCS=2,
GOMEMLIMIT=1GiB, using the command above: 15 retained raw rows, five samples per
cell. Restore preparation, allocation, solve, residual and output were timed;
synthetic forward blur was prepared before ResetTimer.

| Grid | ns/op min / median / max | B/op (all five) | allocs/op (all five) |
| --- | ---: | ---: | ---: |
| 16x16x1 | 159335 / 163108 / 164543 | 13672 | 14 |
| 32x32x1 | 616143 / 617441 / 621566 | 53608 | 14 |
| 8x8x8 | 750919 / 757141 / 768771 | 27128 | 14 |


## Actual selected qualification and remaining gates

The G2 implementation passed 17 exact focal top-level tests in imagefilter/FFT,
zero failures/skips, and two targeted race tests for cancellation and concurrent
independent results, zero failures/skips. The new delta dynamic-range control
actually passed all 36 combinations. G1's positive-denominator-floor defect was
found analytically during review, not an executed RED; its immutable source and
review are preserved. G2 preserves positive predictions instead of clipping them.

All three executions (focal, race, cost) terminated successfully without timeout,
log cap or signals; owned process groups were reaped/empty and capture drained.
Before/after/final guards matched 3052 Go inputs, 3060 dependencies and five owned
postimages. This is selected functional/race/cost evidence, not a full suite or
universal stress guarantee. Minimum Go1.22 with default vet and all 17 selectors
requires the new source files to be present canonically (no absent-target overlay)
and remains pending, along with delivery docs-check and module make check.
The opt-in ITK oracle is **not run**; its matched-model provenance prerequisites
remain explicit above. There is no clinical, Twin UX, PACS or acceleration
qualification.
