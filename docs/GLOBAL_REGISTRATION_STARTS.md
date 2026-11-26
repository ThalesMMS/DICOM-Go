# Global starts for offline registration

`registration.ImagePrincipalMoments` computes physical LPS center, covariance
in mm², ascending principal moments and an orthonormal right-handed basis. It
uses `max(0,intensity+IntensityOffset)` weights. This explicit nonnegative policy
differs from using signed CT values as mass; choose preprocessing/offset for the
actual image. Nonfinite samples, physical overflow, zero mass and finite voxel
admission fail explicitly. The default voxel cap is 2^26. Input samples and
geometry are borrowed and must stay stable. Two passes use fixed scratch.

`PrincipalAxisCandidates` aligns centers and axes with four independently owned
Euler3D candidates. Second moments leave four proper rotation sign choices;
the actual image metric must choose among them. Repeated or weakly separated
eigenvalues (default relative gap 1e-6) refuse orientation initialization.
This is an initialization proposal, not an anatomical orientation classifier.

`optimize.Exhaustive` lazily visits the entire admitted Cartesian grid, parameter
zero fastest, with offsets `-Steps..Steps` multiplied by `StepSizes` in actual
parameter units. No grid array is allocated. `MultiStart` evaluates serially
from detached starts and can invoke a caller's local optimizer. Local returned
positions are evaluated again; reported values cannot bypass the objective.
Serial execution supports existing mutable registration objectives.

Both searches default to 4096 parameters/starts, 100000 total objective calls
and 64 MiB owned logical workspace. Negative limits are invalid; positive caps
remain finite. Admission occurs before snapshots/callbacks. The reservation is
`8192 + 96*N + (8*N+24)*starts` bytes, covering owned start vectors, reusable
trial/gradient/best/result state and headers. It excludes caller objectives,
local optimizer scratch, callback/runtime overhead and input images; it is not
an RSS limit. Local optimizers must evaluate only the passed budgeted function
and propagate its errors. A blocking callback cannot be forcibly interrupted.
Cancellation/errors return no partial result. `+Inf` means an unavailable
candidate; NaN/-Inf or invalid gradients/results fail. Ties retain the first
finite candidate. Callbacks borrow trial vectors and must not retain/mutate them.

`CoarseSearchMinimizer` supplies the existing `Method.Minimize` hook. Only level
zero searches the supplied starts and optional coarse grid; later levels refine
the preceding result. Starts must use the SAME transform family, fixed center
and Euler order as `Method.Transform`. Principal candidates already share a
center/order; pass their `Parameters()` and a clone of the first as the initial
transform. For Euler3D, rotation grid increments are radians and translation
increments are mm. The local refiner receives each level's existing physical
scales. Options are snapshotted before calls. The hook reserves its immutable arrays
inside the workspace cap; when a grid is nested, the remaining bytes are split
equally between multi-start and grid stages (an explicitly smaller grid cap is
preserved). Caller local-optimizer scratch is excluded. This conservative
admission bounds coexisting stages, not RSS. No extra image pyramid or metric
is built.
On later levels the caller's local refiner supplies its own iteration,
evaluation and workspace budgets; search limits apply to level zero.
Method's existing defaults remain unchanged.

This computation belongs to reusable offline registration. It adds no Twin
CPU 3D rendering, preview, reslice consumer, dose or registration application
workflow. A successful synthetic fit is not clinical acceptance or guaranteed
convergence for arbitrary multimodal, cropped or symmetric images.

Behavioral references are the public ITK 5.4 APIs:
[image moments](https://docs.itk.org/projects/doxygen/en/v5.4.0/classitk_1_1ImageMomentsCalculator.html),
[exhaustive optimization](https://docs.itk.org/projects/doxygen/en/v5.4.0/classitk_1_1ExhaustiveOptimizerv4.html)
and [multi-start optimization](https://docs.itk.org/projects/doxygen/en/v5.4.0/classitk_1_1MultiStartOptimizerv4Template.html).
Implementation uses independently written symmetric Jacobi rotations and lazy
grid enumeration. Parameter-unit steps, finite budgets and nonnegative weights
are this API's contract; iteration/axis-sign/bitwise ITK parity is not claimed.

Qualification includes independent point-summed physical covariance,
analytic rotated eigenvalues over wide finite scales, an independently
enumerated Cartesian grid, ownership/budget/cancellation/local-result controls,
and asymmetric synthetic 90°/180° image pairs built by exact voxel permutation.
The public multiresolution Method hook must recover both within 0.5° and 0.5 mm.
