# Bounded optimization and free-form deformation

`optimize.LBFGSB` minimizes a differentiable objective over a coordinate box.
It builds a compact limited-memory BFGS Hessian, follows the projected
steepest-descent path to a generalized Cauchy point, solves the free-variable
subspace model and searches a feasible direction. Bound clipping of an ordinary
L-BFGS two-loop direction does not implement these stages. The existing
`optimize.LBFGS` and the default FFD path retain their previous behavior.

## Optimizer contract

`LowerBounds` and `UpperBounds` are borrowed only during admission/snapshot.
A nil side is unbounded; a supplied side has exactly N values. Lower -Inf and
upper +Inf are allowed. NaN, opposite infinities, reversed bounds and nonfinite
starting values are errors. Equal finite bounds fix a variable. The starting
vector is projected before its first evaluation. Every objective callback sees
a feasible borrowed trial vector and must neither retain nor mutate it. A
callback may reuse its gradient buffer: the optimizer copies it before another
evaluation. `Result.X` is independently owned and does not retain the arena.
Caller slices are not changed; concurrent mutation while being borrowed is invalid.

Zero options select these finite defaults:

| Option | Default |
| --- | --- |
| Memory | 5 pairs; admitted range 1..64 |
| MaximumIterations | 100 |
| MaximumEvaluations | 2000, including the initial and rejected trials |
| MaximumLineSearchEvaluations | 20 per search |
| ProjectedGradientTolerance | 1e-5 |
| ValueTolerance | 1e7 float64 epsilons |
| InitialStepLength | Euclidean length one for the initial direction |
| Limits.MaxParameters | 2^20 |
| Limits.MaxWorkspaceBytes | 512 MiB |

An explicit initial step instead caps the initial direction's largest component.
Subsequent line-search trials may expand within the analytically feasible
interval. Negative counts/limits and nonfinite or negative scalar tolerances
are invalid. There is no negative/unlimited iteration mode in this new type.
Convergence uses `||x - projection(x-gradient)||` infinity, in the original
parameter units; `StopProjectedGradient` distinguishes this from a small raw
gradient. Fixed or active variables can have nonzero raw derivatives at a
constrained optimum. The existing value/iteration/evaluation/search stops are
also returned. A stop reports the applied criterion, not a global-optimum proof.

The line search prefers strong Wolfe conditions, admits an Armijo bound endpoint
and retains its best sufficient-decrease trial when its finite budget ends.
Unsafe correction pairs are skipped. An unusable compact factor resets history
and recomputes the Cauchy/subspace model; no ordinary clipped-LBFGS fallback is
used. Breakpoint ties are deterministic batches. A +Inf objective declares an
unavailable domain trial and may omit its gradient. Finite values require exactly
N finite gradient entries. NaN/-Inf values, malformed gradients and invalid
options return errors. Overflow that makes the numerical model unusable also
returns an error rather than a partial success.

`MinimizeContext` checks cancellation in vector, breakpoint, matrix and search
work and before/after callbacks. It returns zero Result for cancellation or
callback errors. Nil context/function, including typed nil functions, are
invalid. Sort and blocking caller callbacks cannot be forcibly interrupted.
`Minimize` uses a background context. Each call owns its state; there are no
global caches, pools or worker goroutines in this optimizer.

`WorkspaceBytes(N)` validates a dry admission without numeric-array allocation
or function evaluation. With m history pairs its conservative owned ceiling is:

```
W(N,m) = (16*m + 192)*N + 256*m*m + 512*m + 8192 bytes
```

It covers the `(2m+16)*N` float64 arena, independently owned result, N
breakpoints and active flags, two compact matrices, two LU factors/pivot arrays,
24m small-work floats, history slice headers and a fixed descriptor reserve.
Rows of W are obtained from history, so no N-by-2m W array is stored. Resource
admission precedes workspace creation/callbacks and returns
`ErrLBFGSBResourceLimit`. The ceiling counts logical owned buffers, not process
RSS, input slices, callback/metric allocations, stacks or runtime overhead.

## Opt-in FFD boxes

Set `registration.FreeFormDeformation.BoundedOptimizer` to
`*registration.FFDBoundedOptions` to select this optimizer. Nil preserves the
legacy `Optimizer` and its defaults. With a non-nil pointer, the legacy Optimizer
is unused. `LowerMM`/`UpperMM` bound absolute world x/y/z coefficient displacements
in millimetres, for every control point. They are not accumulated step limits.
All three finite intervals must include zero. Zero bounds fix all coefficients;
explicitly set a box when motion is intended. The adapter expands the component
box in the B-spline's existing component-blocked parameter order at every level;
its nested LBFGSB LowerBounds/UpperBounds must be nil.

For bounded FFD, zero projected-gradient tolerance becomes 1e-9 and zero initial
step becomes `MaximumStepSize(virtual)`; other LBFGSB defaults, including its
finite evaluation budget, apply. Refined coefficients carry over through the
existing cubic subdivision and are checked against the same absolute box.
History starts fresh at each resolution. No coefficient restart or implicit
bound narrowing occurs. Source images/options/levels and Initial stay unchanged.
They must remain stable while borrowed. Initial is cloned and applied after the
spline: `Initial(x+D(x))`.

The new bounded path admits only known concrete Initial types: `*Affine`,
`*Euler3D`, `*VersorRigid3D`, `*Similarity3D`, `*Translation3D`,
`*ScaleVersor3D`, `*ScaleSkewVersor3D`, `*BSpline`,
`*DisplacementFieldTransform`, and `*Composite` of these. Custom transforms,
including wrappers around a built-in, are rejected before Clone because their
allocation behavior cannot be accounted. A composite has at most 256 transform
occurrences, depth at most 16 counting the root, no cycles and no nil children.
Shared children count per occurrence because Clone copies each occurrence.
Typed nil Initial values fail explicitly. The legacy FFD path continues to
accept general Transform implementations.

Zero `MaxLevels` selects 16 and zero `MaxControlPointBytes` selects 256 MiB.
Before any control-grid allocation or Initial Clone, the adapter admits all
level/refinement sizes, the existing BSpline ceiling of 2^26 control points,
and the optimizer's dry workspace admission.
For N_l=3*product(mesh_l+3), Nmax=max(N_l), L levels, its control ceiling is:

```
I = 4096*T + 16*P + 32*E
C = 8*(sum(N_l) + 10*Nmax) + 256*L + 8192 + 2*I bytes
```

T is the number of Initial transform occurrences, P is their total BSpline
coefficient count plus three times their dense displacement-vector count, and
E is the number of composite edges. I reserves one known Initial clone,
including conservative backing-capacity/header allowance. Two clones can
coexist when a refined level assembles its new composite before replacing the
old one. Results retain only the current final transform and per-level parameter
vectors, not previous Initial clones. Count/cycle/type admission has fixed-size
traversal scratch. Finitude scans occur after byte admission, before Clone.

C also reserves the old/new grids and axis-refinement intermediates, expanded
bounds, composite/start parameter copies, optimizer result and retained level
parameters. Bounded mode preallocates the LevelResult array and copies each
retained parameter vector to exact capacity, avoiding append-growth hidden in
the sum. Admission errors use `ErrFFDResourceLimit`; option/type/domain errors
wrap `ErrFFD`. W and C are separate conservative reservations; both include the
result vector deliberately. Images, image smoothing, metric/worker workspace
and caller progress code remain outside C. Neither is an RSS or total-allocation
cap. Bounded FFD follows the existing partial-level error/progress policy.

## What the Jacobian condition certifies

Displacement boxes alone do not prevent folds. Set
`MaximumDisplacementGradient` to zero for bounds only: the result makes no
positive-Jacobian claim. A finite value strictly between zero and one, such as
0.8, additionally requires a sufficient condition for the whole box at every
resolution. Unsafe boxes fail before allocation, including a finer level that
would be unsafe even when its coarse level is admissible.

`BSpline.JacobianBound(ctx)` is independently reusable. Let
A[d,a]=max adjacent absolute coefficient difference along grid axis a, and let
M be the stored physical-to-index matrix. A cubic derivative can be written as
adjacent differences weighted by nonnegative quadratic bases with sum one;
the other two cubic bases also sum one. Consequently
C[d,j]=sum_a A[d,a]*abs(M[a,j]) bounds each physical derivative entry.
Its Frobenius norm q also bounds the spectral norm of grad(D). If q<1,
`det(I+grad(D)) >= (1-q)^3`, and
`||T(x)-T(y)|| >= (1-q)||x-y||` on the convex full-support region.
The method rounds norm bounds upward and determinant bounds downward. A bound
that overflows or lacks a positive representable lower determinant is
uncertified. Nonfinite coefficients are invalid.

The full-support region is `1 <= continuous grid index < GridSize-2` in each
axis. The returned origin/dimensions/direction describe its grid chart in mm;
q and determinants are dimensionless. This excludes the existing identity
extension outside support, which may be discontinuous at its boundary.
The condition concerns the real spline represented by stored float64 values;
rounded evaluation/Jacobian sampling is tested with explicit numerical tolerance.
It is sufficient, not necessary. There is no global-R3, inverse, anatomical or
clinical guarantee.

Whole-box admission uses A[d,a]=UpperMM[d]-LowerMM[d] in the same physical
geometry. The outward-rounded q must be at most the caller's threshold and have
a positive representable determinant lower bound. Actual coefficients are
rechecked after refinement and each accepted level. Anisotropic, oblique and
nonorthogonal grid directions use M rather than voxel-unit assumptions.

The three additional linear families have small inlined parameter states and
use the same 4096-byte reservation per occurrence, with both simultaneously
live clones debited in `2*I`. Their Matrix/Offset must be finite, and typed nil
or custom wrappers remain refused before any Clone.

For the guaranteed composition, Initial must be nil or one of the seven concrete
matrix types above with finite Matrix/Offset and a conservatively certified
positive determinant. A reflected, nonlinear or numerically unprovable Initial
fails before Clone. Finite singular, zero-scale or negative-determinant forward
maps remain allowed in bounds-only mode; they do not qualify for the positive
certificate. Negative individual scales are evaluated by the actual matrix
determinant, rather than being refused solely by a parameter sign.
`FFDResult.JacobianBound` then contains both the spline
certificate/domain and a positive final lower bound formed with a downward
rounded Initial determinant interval. Bounds-only mode also supports the admitted
nonlinear built-ins and leaves this field nil.

## Authority and qualification

The optimization formulation follows [Byrd, Lu, Nocedal and Zhu (1995)](https://users.iems.northwestern.edu/~nocedal/PDFfiles/limited.pdf),
sections 2–5. [Choi and Lee (2000)](https://cg.postech.ac.kr/papers/gm00.pdf)
publish sufficient local-injectivity conditions and their non-necessity.
Our adjacent-difference norm derivation above is a separate conservative
criterion, not an assertion that their sharper normalized thresholds are equal.
[ITK's public optimizer API](https://docs.itk.org/projects/doxygen/en/stable/classitk_1_1LBFGSBOptimizerv4.html)
is behavioral context. No ITK/VNL, SciPy or Fortran implementation is used/copied;
iteration or bitwise parity with an external optimizer is not claimed.

Tests compare tiny coupled box quadratics against an exhaustive active-face
SPD oracle, compact BFGS products/Cauchy segments/subspace solves against dense
independent models, and bounded Rosenbrock against its analytic solution.
Jacobian tests use explicit cell-polynomial weights, a bounded linear field
whose determinant is -1, finite differences, nonorthogonal anisotropic geometry,
refinement and conservative threshold rounding. Admission/capacity tests include
known Initial clones, aliases, cycles, custom types, finite callbacks, reused
gradients, cancellation, concurrent calls and the unchanged nil-mode bit fixture.

