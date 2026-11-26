# Diffusion tensor reconstruction

`mrdiffusion.FitTensor` reconstructs a mono-compartment Gaussian diffusion
tensor per voxel from explicitly aligned signal volumes, with a b=0 reference
and at least six independent tensor encoding rows. Existing scalar ADC/fitmap
and diffusion readers retain their APIs and policies. This is an offline
quantitative initializer, not tractography, fiber-crossing reconstruction or
clinical qualification. No Twin grouping, viewer integration or 3D rendering
is introduced; any later 3D display remains GPU-only on both styles/platforms.

## Encoding, units and physical axes

Tensor3 order is xx, yy, zz, xy, xz, yz. Diffusion tensors are mm²/s; b-values
and b-matrices are s/mm². Encoding axes must be patient LPS or explicitly image
axes. An image gradient maps by gLPS=R*gImage, and an image b-matrix maps by
BLPS=R*BImage*Rᵀ. Geometry uses unit orthonormal right-handed direction columns,
positive anisotropic spacing, finite physical corners, origin at first voxel
center and an explicit Frame of Reference UID. All channel grids must match
exactly, including size/origin/spacing/direction/UID; no implicit resampling,
gradient vendor guess or registration occurs. Index spacing does not scale a
unit diffusion direction or its b-value.

A unit gradient and b produce B=b*g*gᵀ. A supplied b-matrix must be finite,
positive semidefinite to numerical tolerance and have trace b. If both forms
are supplied they must agree. Gradients with unknown axes/nonunit magnitude,
missing positive-b encoding or inconsistent b-matrices refuse. b=0 has no
nonzero direction or matrix. Repeated nonzero encodings are counted (antipodal
gradients have the same b-matrix); repeats are legitimate repeated observations
but never repair insufficient rank. b=0 absence and rank deficiency are explicit
errors. B-values beyond 1e7 s/mm² refuse. No units conversion is silently inferred.

`ReadTensorEncoding` reads standard enhanced/classic b-matrix attributes and
reuses the existing direction reader. Standard/Siemens patient axes and Philips
RL/AP/FH patient axes are accepted. GE logical/unknown vendor frames need an
explicit independently established conversion and cannot be guessed. Existing
Read errors retain their type; added tensor validation uses ErrTensor.
`ReadTensorSource` captures Study/Series/SOP class/instance and optional 1-based
frame reference, with frame range and UID validation. Geometry and decoded
signal grouping remain caller responsibilities; input datasets are not changed.

## Mathematical fit and noise policy

For each voxel, subtract the explicit nonnegative NoiseFloor from every signal.
If any corrected signal is at or below MinSignal (default zero), mark LowSignal.
Nonfinite encountered signals mark Nonfinite. Masked voxels are marked Masked
without fitting. The first failing channel determines the status, in input
order; these counts are voxel counts, not counts of every invalid sample.

S0 is the arithmetic mean of corrected b=0 signals. For each positive-b volume,
fit y=log(S0)-log(S_i)=Bxx*Dxx+Byy*Dyy+Bzz*Dzz+2Bxy*Dxy+2Bxz*Dxz+2Byz*Dyz.
The method is unweighted least squares anchored to measured S0, using a globally
scaled, twice reorthogonalized modified Gram-Schmidt QR. A normalized column
norm≤1e-10*sqrt(number of weighted rows) refuses insufficient/unresolved rank.
DesignDiagonalRatio is max/min QR diagonal, a rank-conditioning diagnostic,
not a full spectral condition number. There is no fitted intercept, Rician
noise likelihood, robust outlier fit, automatic floor estimate or gradient
motion correction. Duplicate samples affect OLS weighting as supplied.

The result stores six coefficients, descending eigenvalues, MD=(λ1+λ2+λ3)/3
and FA=sqrt(3/2*sum((λi-MD)²)/sum(λi²)). Zero tensor yields FA=MD=0. Eigen
quantities use scale normalization for finite arithmetic. Eigenvalue magnitudes
above MaxDiffusivity (default .1 mm²/s) or unresolved arithmetic mark Numerical.
Negative eigenvalues below -NegativeTolerance (default1e-10 mm²/s) mark Negative
under the default reject policy. Explicit clamp policy projects negative
values to zero and reconstructs the tensor. Smaller negative roundoff is also
clamped; every corrected voxel has Clamped=true and is counted in result.Clamped.
Invalid voxels expose zero numerical fields with explicit Status, never a valid
zero measurement. Result.Counts counts all six statuses, totaling the voxel count.

The principal direction is LPS and defined only if the largest positive
eigenvalue exceeds the second by more than 1e-8 times the largest. Repeated
largest eigenvalues/isotropy leave DirectionDefined=false and RGB zero. Defined
vectors choose sign so the component with largest absolute magnitude is positive
(ties x/y/z order). Orientation represents an unoriented axis, not axonal polarity.
RGB is round(255*FA*abs(directionLPS)), red/green/blue corresponding to L/P/S
axes, not display/image axes. FA roundoff is capped at1. Input float32 precision
limits very small decays; the model does not recover information rounded away.

## Ownership, bounds, shared workers and cancellation

Inputs remain stable during the call and are not modified. Result owns Voxels,
geometry value, encoding rows and copied source metadata slices; borrowed signal
and mask slices are not retained. Callers own the result and must not mutate
Voxels concurrently with map writing or readers. Metadata strings are immutable
Go values. Source reference counts/lengths are bounded before output allocation.

Default limits are256 volumes,2^24 voxels and512 MiB logical request storage,
with an absolute4096-channel cap. Admission counts borrowed float32 signals,
output TensorVoxel payload, mask allowance, design/QR/provenance payload and
conservative bounded worker scratch. Thus the limit includes borrowed input,
not only incremental output; LogicalBytes is a request admission amount, not
measured allocations/RSS. Slice/runtime/object overhead and cumulative per-voxel
eigen-decomposition allocations are not RSS/peak measurements. Integer products
and additions are checked before allocating volume outputs or design arrays.

CPU helpers are obtained through the existing process-wide parallel budget,
with a per-call maximum64 workers and requested ceiling. Disjoint voxel writes
and fixed per-voxel reduction order give the same results for different worker
availability. Context is checked during metadata/design/QR, each voxel and
channel groups, and before publication. Eigen decomposition is an existing
bounded3x3 kernel (at most64 sweeps) checked between calls, not interruptible at
each Jacobi operation. Cancellation returns a zero result without partial maps.

## Derived DICOM maps

`WriteTensorMap(ctx,result,quantity,options)` writes FA, MD or any of six LPS
components as scalar research Parametric Maps via the existing writer. FA uses
(110808,DCM,Fractional Anisotropy), UCUM1; MD uses113202/DCM, UCUMmm2/s.
Tensor coefficients use DCM110810–110815 with correct component labels and
UCUMmm2/s. Direction/RGB remain explicitly typed offline maps in TensorResult;
this scalar writer does not disguise vector or RGB channels as scalar quantities.
Six component maps preserve the physical tensor for further vector reconstruction.
The quantity code lives in RWVM; the separate Derivation Code is
(129105,DCM,Diffusion image analysis), from CID7203. It describes processing,
not FA/MD/tensor coefficients. The accompanying method description preserves
the fit parameters.

Every input volume must provide a source for every output plane. Source study
must equal output study; multiple source series are grouped correctly in Common
Instance Reference, and enhanced frame numbers are preserved in per-plane
Derivation Image sources. Shared identity real-world mapping, oblique plane
geometry and RESEARCH qualification are retained. Method description records
OLS anchoring, LPS axes, noise floor, thresholds, negative policy/tolerance,
range limit, volume/duplicate/clamp/invalid counts. Original diffusion acquisition
parameters remain discoverable through source references; private encoding rows
are also retained in the result during its lifetime.

Invalid/masked measurements write NaN, rather than valid zero values. Public
result values are validated for finite float32 conversion before encoding.
Output IDs are supplied by the caller; inherited common Frame of Reference
and Study identity must agree with fit/output identity. The map operation has
its own512 MiB default staging cap:24 bytes per voxel,8192 per plane and1024 per
source metadata record. These are logical staging/conservative metadata
allowances, not a byte-exact runtime/RSS cap. The caller-supplied common patient
module is separate. The existing bounded Parametric Map encoder has no internal
context; checks immediately before/after it and at final publication prevent a
canceled call returning a file. SDKs, external oracles and UI are not dependencies.

## Local synthetic qualification and measured costs

Independent forward fixtures evaluate exp(-B:D) explicitly, without the QR or
eigen solver. Analytic tensor coefficients/trace/Frobenius FA, rotated diagonal
spectra and principal axis verify isotropic/anisotropic recovery, patient/image
rotation consistency and anisotropic oblique grids. Degenerate rank/b0/unknown
encoding/inconsistent gradients/matrix/geometry, noise/negative policies,
undefined direction, snapshots, cancellation and workers have focused cases.
Synthetic DICOM classic/enhanced extraction is connected to fitting and writing;
eight scalar quantities roundtrip pixels/codes/units/geometry/source series and
enhanced frame numbers. Existing scalar ADC/fitmap and Parametric Map callers
remain selected regressions. No Twin adapter changed.

Native cost recipe is three200ms samples per row: reconstruction8x8x4 and
32x24x8 (seven channels), plus FA map writing4x3x2. Fixtures and the map's fit
are outside child timers; fitting includes admission, design, allocations,
per-voxel eigen decomposition and diagnostics; writing includes method/source
metadata, staging and encoding, with result retained outside timer. Report all
nine rows with medians/ranges, ns/op, B/op, allocs/op. Numerical checks are
separate from speed. The local native run passed all eighteen focused top-level cases (ten new and
eight diffusion/ADC/fitmap/Parametric Map callers), with no skips or failures.
The two selected ownership/concurrency and atomic-cancellation cases also passed
with `-race`; this is not a full-module race qualification. A prior compilation
attempt failed because the map writer used a nonexistent vector method; its
receipt is retained, and the corrected writer uses the existing Vec3.Scale API.

Native cost was measured once with the fixed recipe above on Darwin/arm64,
Apple M4 Pro, native Go1.27.1, GOMAXPROCS2, GOMEMLIMIT1GiB, Workers1 in fitting,
default warmed build cache and `-count=3 -benchtime=200ms -benchmem`. The scalar
map row measures FA writing only, with its fitted result retained before timing.
All reconstruction rows include admission/QR, output allocation, per-voxel
bounded eigen decomposition and numerical assertions.

| Operation | ns/op median [min,max] | B/op median [min,max] | allocs/op median [min,max] |
| --- | --- | --- | --- |
| Fit7 channels,8×8×4 | 162928 [160081,163290] | 180848 [180848,180848] | 4121 [4121,4121] |
| Fit7 channels,32×24×8 | 3815154 [3791283,3865531] | 4185104 [4185104,4185186] | 98329 [98329,98329] |
| FA scalar PM,4×3×2 | 48584 [48481,48763] | 103638 [103637,103638] | 1549 [1549,1549] |

These are complete operation costs per volume/map, not per DICOM frame. B/op
is cumulative allocation per operation, not retained output, logical admission
or peak RSS. The existing per-voxel eigen solver allocates small matrices; its
cost remains included and no allocation-efficiency or speedup claim is made.
No process RSS or comparison with another implementation was measured. The
three samples per row describe these synthetic sizes/host, not clinical data
or a universal cost bound. All eighteen selected cases also passed on the real
integrated tree with Go 1.22.12 and default vet, using external linking and
CGO on Darwin/arm64. Library `make check` and `make docs-check` passed for the
integrated delivery. The earlier new-file overlay attempt failed because vet
required actual files; that attempt is retained and was not counted as passing.
Optional ITK local numerical comparison is separate and NOT_RUN. No PACS or
clinical qualification is claimed.

Public clean-room mathematical/standard references:
[DICOM CID7203 derivation methods](https://dicom.nema.org/medical/dicom/current/output/chtml/part16/sect_CID_7203.html),
[ITK reconstruction contract](https://docs.itk.org/projects/doxygen/en/latest/classitk_1_1DiffusionTensor3DReconstructionImageFilter.html)
and [DICOM CID7263 quantity codes](https://dicom.nema.org/medical/Dicom/2025a/output/chtml/part16/sect_CID_7263.html).
No implementation or assets were copied. Synthetic agreement does not validate
real noise, crossing fibers, clinical usefulness or interoperability with a
particular PACS.
